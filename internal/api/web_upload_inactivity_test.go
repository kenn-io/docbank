package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"
)

// dialBubbleWebSocket serves handler over the bubble's in-memory network, so
// websocket read deadlines run on the bubble's fake clock.
func dialBubbleWebSocket(t *testing.T, handler http.HandlerFunc) *websocket.Conn {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	conn, response, err := websocket.Dial(t.Context(), "ws://example.com/", &websocket.DialOptions{HTTPClient: server.Client()})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestWebUploadInactivityExcludesConsumerWork(t *testing.T) {
	t.Parallel()
	for _, firstRead := range []int{1, 3} {
		t.Run(strconv.Itoa(firstRead), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				sent := make(chan error, 1)
				conn := dialBubbleWebSocket(t, func(w http.ResponseWriter, r *http.Request) {
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
				})
				t.Cleanup(func() { close(release) })
				reader := &webUploadReader{ctx: t.Context(), conn: conn, requestID: "slow-consumer", inactivity: webUploadInactivity}
				prefix := make([]byte, firstRead)
				_, err := io.ReadFull(reader, prefix)
				require.NoError(t, err)
				require.NoError(t, <-sent)
				// Storage work can outlast the network inactivity limit after bytes arrive.
				time.Sleep(2 * reader.inactivity)
				rest, err := io.ReadAll(reader)
				require.NoError(t, err)
				require.Equal(t, "abcdef", string(prefix)+string(rest))
				require.True(t, reader.ended)
			})
		})
	}
}

func TestWebUploadInactivityReleasesMutationGate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		g := NewOperationGate()
		result := make(chan error, 1)
		conn := dialBubbleWebSocket(t, func(w http.ResponseWriter, r *http.Request) {
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
				inactivity: webUploadInactivity,
			}
			result <- g.mutate(r.Context(), func() error {
				_, err := reader.Read(make([]byte, 1))
				return err
			})
		})
		var ready webUploadMessage
		require.NoError(t, wsjson.Read(t.Context(), conn, &ready))
		require.Equal(t, "ready", ready.Type)

		// The stalled reader holds the mutation lease until the limit passes.
		time.Sleep(webUploadInactivity - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("stalled upload ended before its inactivity limit: %v", err)
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-result:
			require.ErrorIs(t, err, context.DeadlineExceeded, err)
		default:
			t.Fatal("stalled upload did not release its mutation lease")
		}
		require.NoError(t, g.MaintainContext(t.Context(), func() error { return nil }))
	})
}
