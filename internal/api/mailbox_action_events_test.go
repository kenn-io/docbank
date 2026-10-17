package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

func TestMailboxActionCancellationAndResume(t *testing.T) {
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "1")
	t.Setenv("TELEMETRY_ENABLED", "1")
	for _, origin := range []string{"cli", "web", "observed"} {
		for _, beforeAddition := range []bool{true, false} {
			t.Run(fmt.Sprintf("origin=%s/before=%t", origin, beforeAddition), func(t *testing.T) {
				capture := newActionCollector(t)
				gate := api.NewOperationGate()
				server, catalog := newTestServer(t, func(d *api.Deps) { d.Imports = capture.imports; d.Gate = gate })
				owner := "vault:" + catalog.VaultID()
				raw := []byte("From sender@example.com Sat Sep 12 10:00:00 2026\nSubject: Synthetic archive\nContent-Type: text/plain\n\nSynthetic body.\n")
				raw = append(raw, []byte("From sender@example.com Sat Sep 12 10:01:00 2026\nSubject: Second synthetic message\nContent-Type: text/plain\n\nSecond synthetic body.\n")...)
				digest := sha256.Sum256(raw)
				container := store.MailboxContainerRequest{ID: "synthetic-source", Owner: owner, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(raw)), Format: "mbox"}
				_, err := catalog.BeginMailboxContainer(t.Context(), container)
				require.NoError(t, err)
				service := mailbox.Service{Store: catalog.Store, Blobs: catalog.Blobs, Spool: t.TempDir(), ImportResult: api.MailboxImportResult(t.Context(), catalog.Store, gate, capture.imports)}
				require.NoError(t, service.UploadChunk(t.Context(), owner, container.ID, 0, container.SHA256, container.Size, bytes.NewReader(raw)))
				_, err = service.Seal(t.Context(), owner, container.ID)
				require.NoError(t, err)
				request := api.MailboxJobInput{ID: "synthetic-job", ContainerID: container.ID, ContainerSHA256: container.SHA256, Settings: api.MailboxSettingsInput{DestinationID: catalog.RootID()}}
				headers := map[string]string{"X-Docbank-Surface": "cli"}
				if origin == "observed" {
					headers = nil
				}
				if origin == "web" {
					headers = map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, server)}
				}
				response, body := do(t, server, http.MethodPost, "/api/v1/mailbox/jobs", headers, request)
				require.Equal(t, http.StatusCreated, response.StatusCode, body)
				job, err := catalog.ClaimMailboxJob(t.Context())
				require.NoError(t, err)
				cancelled := false
				cancel := func() {
					response, body := do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/cancel", nil, nil)
					require.Equal(t, http.StatusNoContent, response.StatusCode, body)
					cancelled = true
				}
				if beforeAddition {
					cancel()
				} else {
					service.Mutate = func(ctx context.Context, fn func() error) error {
						if err := gate.MutateContext(ctx, fn); err != nil {
							return err
						}
						current, err := catalog.MailboxJob(ctx, owner, job.ID)
						if err != nil {
							return err
						}
						if current.Imported > 0 && !cancelled {
							cancel()
						}
						return nil
					}
					require.Error(t, service.RunJob(t.Context(), job))
				}
				if origin != "cli" {
					headers = map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, server)}
				}
				response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/resume", headers, api.MailboxResumeInput{Request: request})
				require.Equal(t, http.StatusOK, response.StatusCode, body)
				response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/resume", headers, api.MailboxResumeInput{Request: request})
				require.Equal(t, http.StatusOK, response.StatusCode, body)
				service.ImportResult(job.ID) // a delayed callback from the cancelled run sees the resumed job
				job, err = catalog.ClaimMailboxJob(t.Context())
				require.NoError(t, err)
				require.NoError(t, service.RunJob(t.Context(), job))
				if origin == "observed" {
					require.NoError(t, capture.reporter.Close())
					require.Empty(t, capture.events)
				} else {
					capture.requireEvent(t, "document_added", map[string]any{"surface": origin})
				}
			})
		}
	}
}
