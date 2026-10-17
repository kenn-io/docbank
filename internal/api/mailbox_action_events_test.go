package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

func TestMailboxActionCancellationAndResume(t *testing.T) {
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "1")
	t.Setenv("TELEMETRY_ENABLED", "1")
	for _, origin := range []string{"cli", "web", "observed"} {
		t.Run(origin, func(t *testing.T) {
			capture := newActionCollector(t)
			gate := api.NewOperationGate()
			server, catalog := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = capture.capture; d.Gate = gate })
			owner := "vault:" + catalog.VaultID()
			raw := []byte("From sender@example.com Sat Sep 12 10:00:00 2026\nSubject: Synthetic archive\nContent-Type: text/plain\n\nSynthetic body.\n")
			raw = append(raw, []byte("From sender@example.com Sat Sep 12 10:01:00 2026\nSubject: Second synthetic message\nContent-Type: text/plain\n\nSecond synthetic body.\n")...)
			digest := sha256.Sum256(raw)
			container := store.MailboxContainerRequest{ID: "synthetic-source", Owner: owner, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(raw)), Format: "mbox"}
			_, err := catalog.BeginMailboxContainer(t.Context(), container)
			require.NoError(t, err)
			service := mailbox.Service{Store: catalog.Store, Blobs: catalog.Blobs, Spool: t.TempDir(), Telemetry: capture.capture}
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
			canceledRequest := request
			canceledRequest.ID = "canceled-job"
			response, body := do(t, server, http.MethodPost, "/api/v1/mailbox/jobs", headers, canceledRequest)
			require.Equal(t, http.StatusCreated, response.StatusCode, body)
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/canceled-job/cancel", nil, nil)
			require.Equal(t, http.StatusNoContent, response.StatusCode, body)
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs", headers, request)
			require.Equal(t, http.StatusCreated, response.StatusCode, body)
			replayHeaders := map[string]string{api.ActionSurfaceHeader: "cli"}
			if origin != "web" {
				replayHeaders = map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, server)}
			}
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs", replayHeaders, request)
			require.Equal(t, http.StatusCreated, response.StatusCode, body)
			synctest.Test(t, func(t *testing.T) {
				claimed, release := make(chan struct{}), make(chan struct{})
				first := true
				service.Mutate = func(_ context.Context, fn func() error) error {
					err := fn()
					if first {
						first = false
						close(claimed)
						<-release
					}
					return err
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- service.RunWorker(ctx) }()
				<-claimed
				canceled := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, server.URL+"/api/v1/mailbox/jobs/synthetic-job/cancel", nil)
				r.Header.Set("X-Api-Key", testAPIKey)
				catalog.Server.Handler().ServeHTTP(canceled, r)
				require.Equal(t, http.StatusNoContent, canceled.Code, canceled.Body.String())
				close(release)
				synctest.Wait()
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
			})
			service.Mutate = nil
			paused, err := catalog.MailboxJob(t.Context(), owner, request.ID)
			require.NoError(t, err)
			require.Zero(t, paused.Checkpoint)
			require.Empty(t, capture.events)
			headers = map[string]string{"X-Docbank-Surface": "cli"}
			if origin == "cli" || origin == "observed" {
				headers = map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, server)}
			}
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/resume", headers, api.MailboxResumeInput{Request: request})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/resume", headers, api.MailboxResumeInput{Request: request})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			job, err := catalog.ClaimMailboxJob(t.Context())
			require.NoError(t, err)
			require.Equal(t, request.ID, job.ID)
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
