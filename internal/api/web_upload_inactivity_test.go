package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
)

func TestWebUploadInactivityExcludesConsumerWork(t *testing.T) {
	t.Parallel()
	for _, firstRead := range []int{1, 3} {
		t.Run(strconv.Itoa(firstRead), func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			sent := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					sent <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				for _, payload := range []string{"abc", "def"} {
					if err := conn.Write(r.Context(), websocket.MessageBinary, []byte(payload)); err != nil {
						sent <- err
						return
					}
				}
				sent <- wsjson.Write(r.Context(), conn, webUploadMessage{Type: "end", RequestID: "slow-consumer"})
				<-release
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })
			conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			if response != nil && response.Body != nil {
				require.NoError(t, response.Body.Close())
			}
			t.Cleanup(func() { _ = conn.CloseNow() })
			reader := &webUploadReader{ctx: t.Context(), conn: conn, requestID: "slow-consumer", inactivity: time.Second}
			prefix := make([]byte, firstRead)
			_, err = io.ReadFull(reader, prefix)
			require.NoError(t, err)
			require.NoError(t, <-sent)
			// Storage work can outlast the network inactivity limit after bytes arrive.
			time.Sleep(2 * reader.inactivity)
			rest, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Equal(t, "abcdef", string(prefix)+string(rest))
			require.True(t, reader.ended)
		})
	}
}

func TestWebUploadInactivityReleasesMutationGate(t *testing.T) {
	t.Parallel()
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
	require.NoError(t, g.MaintainContext(t.Context(), func() error { return nil }))
}
