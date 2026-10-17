package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

func TestMailboxActionConcurrentAdmission(t *testing.T) {
	t.Setenv("DOCBANK_TELEMETRY_ENABLED", "1")
	t.Setenv("TELEMETRY_ENABLED", "1")
	capture := newActionCollector(t)
	gate := api.NewOperationGate()
	server, catalog := newTestServer(t, func(d *api.Deps) { d.TelemetryCapture = capture.capture; d.Gate = gate })
	owner := "vault:" + catalog.VaultID()
	raw := []byte("From sender@example.com Sat Sep 12 10:00:00 2026\nSubject: Synthetic archive\nContent-Type: text/plain\n\nSynthetic body.\n")
	digest := sha256.Sum256(raw)
	container := store.MailboxContainerRequest{ID: "synthetic-source", Owner: owner, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(raw)), Format: "mbox"}
	_, err := catalog.BeginMailboxContainer(t.Context(), container)
	require.NoError(t, err)
	service := mailbox.Service{Store: catalog.Store, Blobs: catalog.Blobs, Spool: t.TempDir(), Telemetry: capture.capture}
	require.NoError(t, service.UploadChunk(t.Context(), owner, container.ID, 0, container.SHA256, container.Size, bytes.NewReader(raw)))
	_, err = service.Seal(t.Context(), owner, container.ID)
	require.NoError(t, err)
	request := api.MailboxJobInput{ID: "synthetic-job", ContainerID: container.ID, ContainerSHA256: container.SHA256, Settings: api.MailboxSettingsInput{DestinationID: catalog.RootID()}}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		endFreeze := api.BeginTextCitationTestFreeze(t, gate)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		failed := httptest.NewRecorder()
		failedRequest := httptest.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/mailbox/jobs", bytes.NewReader(body))
		failedRequest.Header.Set("X-Api-Key", testAPIKey)
		failedRequest.Header.Set("X-Docbank-Surface", "cli")
		failedDone := make(chan struct{})
		go func() {
			defer close(failedDone)
			catalog.Server.Handler().ServeHTTP(failed, failedRequest)
		}()
		synctest.Wait()
		accepted := httptest.NewRecorder()
		acceptedRequest := httptest.NewRequest(http.MethodPost, server.URL+"/api/v1/mailbox/jobs", bytes.NewReader(body))
		acceptedRequest.Header.Set("X-Api-Key", testAPIKey)
		acceptedRequest.Header.Set("X-Docbank-Surface", "cli")
		acceptedDone := make(chan struct{})
		go func() {
			defer close(acceptedDone)
			catalog.Server.Handler().ServeHTTP(accepted, acceptedRequest)
		}()
		synctest.Wait()
		cancel()
		endFreeze()
		<-failedDone
		<-acceptedDone
		require.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
		require.Equal(t, http.StatusCreated, accepted.Code, accepted.Body.String())
	})
	response, replayBody := do(t, server, http.MethodPost, "/api/v1/mailbox/jobs", map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, server)}, request)
	require.Equal(t, http.StatusCreated, response.StatusCode, replayBody)
	job, err := catalog.ClaimMailboxJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, service.RunJob(t.Context(), job))
	capture.requireEvent(t, "document_added", map[string]any{"surface": "cli"})
}

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
			response, body := do(t, server, http.MethodPost, "/api/v1/mailbox/jobs", headers, request)
			require.Equal(t, http.StatusCreated, response.StatusCode, body)
			_, err = catalog.ClaimMailboxJob(t.Context())
			require.NoError(t, err)
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/cancel", nil, nil)
			require.Equal(t, http.StatusNoContent, response.StatusCode, body)
			if origin != "cli" {
				headers = map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, server)}
			}
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/resume", headers, api.MailboxResumeInput{Request: request})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			response, body = do(t, server, http.MethodPost, "/api/v1/mailbox/jobs/synthetic-job/resume", headers, api.MailboxResumeInput{Request: request})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			job, err := catalog.ClaimMailboxJob(t.Context())
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
