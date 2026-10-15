package daemonconn

import (
	"bufio"
	"net"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveRawChallenge answers each connection's first request with reply, raw.
func serveRawChallenge(t *testing.T, reply string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					return
				}
				_, _ = conn.Write([]byte(reply))
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

func TestPushKeyChallengeClassifiesBrokenAnswers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		reply     string
		permanent bool
	}{
		{name: "malformed status line", reply: "HTTP/1.1 abc\r\n\r\n", permanent: true},
		{name: "malformed header", reply: "HTTP/1.1 200 OK\r\nno colon here\r\n\r\n", permanent: true},
		{name: "connection closed before answering", reply: ""},
		{name: "headers cut off", reply: "HTTP/1.1 200 OK\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			connection, err := NewPushConnection(serveRawChallenge(t, test.reply), "synthetic-key")
			require.NoError(t, err)
			_, err = connection.PushCursors(t.Context(), "laptop")
			require.Error(t, err)
			assert.Equal(t, test.permanent, IsKeyProofError(err), "unexpected classification: %v", err)
			assert.True(t, IsTransportError(err), "no answer was a daemon response: %v", err)
		})
	}
}
