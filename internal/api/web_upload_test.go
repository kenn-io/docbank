package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestBrowserUploadUsesAuthenticatedPinnedChannel(t *testing.T) {
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Gate = gate
	})
	destination, err := s.Mkdir(t.Context(), s.RootID(), "Reports")
	require.NoError(t, err)

	request, err := http.NewRequest(
		http.MethodPost, ts.URL+"/api/daemon/web-session", nil,
	)
	require.NoError(t, err)
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&session))
	require.NoError(t, response.Body.Close())

	socketURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/daemon/web-upload"
	conn, response, err := websocket.Dial(t.Context(), socketURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}},
	})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type": "authenticate", "token": session.Token,
		"nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
	}))
	var message struct {
		Type      string            `json:"type"`
		RequestID string            `json:"request_id"`
		Proof     string            `json:"proof"`
		Code      string            `json:"code"`
		Receipt   api.UploadReceipt `json:"receipt"`
	}
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "authenticated", message.Type)
	require.NotEmpty(t, message.Proof)

	content := []byte("quarterly")
	digest := sha256.Sum256(content)
	maintenanceHeld := make(chan struct{})
	releaseMaintenance := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- gate.Maintain(func() error {
			close(maintenanceHeld)
			<-releaseMaintenance
			return nil
		})
	}()
	<-maintenanceHeld
	t.Cleanup(func() {
		select {
		case <-releaseMaintenance:
		default:
			close(releaseMaintenance)
		}
	})
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type":          "begin",
		"request_id":    "busy-browser-test",
		"parent_id":     destination.ID,
		"name":          "quarterly.txt",
		"mime_type":     "text/plain",
		"expected_hash": hex.EncodeToString(digest[:]),
		"expected_size": len(content),
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "error", message.Type)
	require.Equal(t, "maintenance_busy", message.Code)
	close(releaseMaintenance)
	require.NoError(t, <-maintenanceDone)

	const requestID = "browser-test"
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type":          "begin",
		"request_id":    requestID,
		"parent_id":     destination.ID,
		"name":          "quarterly.txt",
		"mime_type":     "text/plain",
		"expected_hash": hex.EncodeToString(digest[:]),
		"expected_size": len(content),
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "ready", message.Type)
	require.Equal(t, requestID, message.RequestID)
	require.NoError(t, conn.Write(
		t.Context(), websocket.MessageBinary, content,
	))
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type": "end", "request_id": requestID,
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "receipt", message.Type)
	require.Equal(t, requestID, message.RequestID)
	assert.Equal(t, "added", message.Receipt.Status)
	require.NotNil(t, message.Receipt.Node.ParentID)
	assert.Equal(t, destination.ID, *message.Receipt.Node.ParentID)
	assert.Equal(t, "quarterly.txt", message.Receipt.Node.Name)
	reader, err := s.Blobs.OpenContext(t.Context(), message.Receipt.Node.BlobHash)
	require.NoError(t, err)
	stored, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, content, stored)
}

func TestServerShutdownDrainsActiveBrowserUpload(t *testing.T) {
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Gate = gate
	})
	destination, err := s.Mkdir(t.Context(), s.RootID(), "Reports")
	require.NoError(t, err)

	request, err := http.NewRequest(
		http.MethodPost, ts.URL+"/api/daemon/web-session", nil,
	)
	require.NoError(t, err)
	response, err := ts.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&session))
	require.NoError(t, response.Body.Close())

	conn, response, err := websocket.Dial(
		t.Context(),
		"ws"+strings.TrimPrefix(ts.URL, "http")+"/api/daemon/web-upload",
		&websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}},
		},
	)
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type": "authenticate", "token": session.Token,
		"nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
	}))
	var message struct {
		Type string `json:"type"`
	}
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "authenticated", message.Type)

	content := []byte("incomplete")
	digest := sha256.Sum256(content)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type":          "begin",
		"request_id":    "shutdown-test",
		"parent_id":     destination.ID,
		"name":          "incomplete.txt",
		"mime_type":     "text/plain",
		"expected_hash": hex.EncodeToString(digest[:]),
		"expected_size": len(content),
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "ready", message.Type)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, s.Server.Shutdown(shutdownCtx))
	require.NoError(t, gate.Maintain(func() error { return nil }))
}
