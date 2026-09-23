package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestBrowserUploadRevocationReleasesQueuedMaintenance(t *testing.T) {
	for _, action := range []string{"revoke", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			gate := api.NewOperationGate()
			ts, catalog := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
			srv := &testServer{ts: ts}
			token := packageBrowserToken(t, srv)
			headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
			hash := sha256.Sum256([]byte("synthetic"))
			digest := hex.EncodeToString(hash[:])
			created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{
				"container_id": "stalled-zip", "sha256": digest, "size": 9,
			}), headers)
			require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
			conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{
				HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}},
			})
			require.NoError(t, err)
			if response != nil && response.Body != nil {
				require.NoError(t, response.Body.Close())
			}
			t.Cleanup(func() { _ = conn.CloseNow() })
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "authenticate", "token": token, "nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}))
			var message struct {
				Type string `json:"type"`
			}
			require.NoError(t, wsjson.Read(t.Context(), conn, &message))
			require.Equal(t, "authenticated", message.Type)
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin", "request_id": "stalled", "container_id": "stalled-zip", "expected_hash": digest, "expected_size": 9}))
			require.NoError(t, wsjson.Read(t.Context(), conn, &message))
			require.Equal(t, "ready", message.Type)

			runCtx, cancel := context.WithCancel(t.Context())
			entered := make(chan struct{})
			release := make(chan struct{})
			maintained := make(chan struct{})
			var maintenanceErr error
			go func() {
				maintenanceErr = gate.MaintainContext(runCtx, func() error {
					close(entered)
					select {
					case <-release:
					case <-runCtx.Done():
					}
					return nil
				})
				close(maintained)
			}()
			t.Cleanup(func() { cancel(); _ = conn.CloseNow(); <-maintained })
			require.Eventually(t, func() bool {
				response, _ := do(t, ts, http.MethodPost, "/api/v1/tags", nil, map[string]string{"name": "maintenance-probe"})
				return response.StatusCode == http.StatusServiceUnavailable
			}, 5*time.Second, 10*time.Millisecond)

			stopped := make(chan struct{})
			var stopErr error
			go func() {
				defer close(stopped)
				if action == "shutdown" {
					stopErr = catalog.Server.Shutdown(runCtx)
					return
				}
				request, err := http.NewRequestWithContext(runCtx, http.MethodDelete, ts.URL+"/api/daemon/web-session", nil)
				if err != nil {
					stopErr = err
					return
				}
				request.Header.Set("X-Api-Key", "")
				request.Header.Set(api.WebSessionHeader, token)
				response, err := ts.Client().Do(request)
				if err != nil {
					stopErr = err
					return
				}
				stopErr = response.Body.Close()
				if response.StatusCode != http.StatusNoContent {
					stopErr = errors.New("session revocation failed")
				}
			}()
			t.Cleanup(func() { cancel(); _ = conn.CloseNow(); <-stopped })
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("revocation left maintenance waiting on the upload")
			}
			readCtx, cancelRead := context.WithTimeout(t.Context(), 5*time.Second)
			_, _, err = conn.Read(readCtx)
			cancelRead()
			require.Error(t, err)
			require.NotErrorIs(t, err, context.DeadlineExceeded)
			close(release)
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("revocation cleanup did not finish")
			}
			require.NoError(t, stopErr)
			<-maintained
			require.NoError(t, maintenanceErr)
			owner := sha256.Sum256([]byte(token))
			_, err = catalog.MailboxContainer(t.Context(), hex.EncodeToString(owner[:]), "stalled-zip")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}
