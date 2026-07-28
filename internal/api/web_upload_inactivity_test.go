package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
)

func TestWebUploadInactivityReleasesMutationGate(t *testing.T) {
	g := NewOperationGate()
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if err := wsjson.Write(r.Context(), conn, webUploadMessage{
			Type: "ready", RequestID: "stalled",
		}); err != nil {
			result <- err
			return
		}
		reader := &webUploadReader{
			ctx: r.Context(), conn: conn, requestID: "stalled",
			inactivity: 25 * time.Millisecond,
		}
		result <- g.mutate(func() error {
			_, err := reader.Read(make([]byte, 1))
			return err
		})
	}))
	t.Cleanup(server.Close)

	conn, response, err := websocket.Dial(
		t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil,
	)
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	var ready webUploadMessage
	require.NoError(t, wsjson.Read(t.Context(), conn, &ready))
	require.Equal(t, "ready", ready.Type)

	select {
	case err := <-result:
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded, err)
	case <-time.After(time.Second):
		t.Fatal("stalled upload did not release its mutation lease")
	}
	require.NoError(t, g.Maintain(func() error { return nil }))
}
