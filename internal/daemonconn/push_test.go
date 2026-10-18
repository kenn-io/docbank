package daemonconn

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonauth"
)

func TestPushConnectionReusesAuthenticatedConnectionsAcrossWindows(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	arrived := make(chan struct{}, 32)
	release := make(chan struct{})
	var proofs atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == daemonauth.KeyChallengePath {
			proofs.Add(1)
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof("synthetic-key", nonce)})
			return
		}
		if r.URL.Path != "/api/v1/push/sources" || r.Header.Get("X-Api-Key") != "synthetic-key" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next":""}`))
	}))
	defer server.Close()
	connection, err := NewPushConnection(server.URL, "synthetic-key")
	require.NoError(t, err)
	defer func() { require.NoError(t, connection.Close()) }()
	// Release any blocked handlers before closing the server on a failed assertion.
	defer cancel()
	for range 2 {
		done := make(chan error, 32)
		for range 32 {
			go func() {
				_, err := connection.PushCursors(ctx, "synthetic-source")
				done <- err
			}()
		}
		for range 32 {
			select {
			case <-arrived:
			case <-ctx.Done():
				t.Fatal("requests did not fill the connection window")
			}
		}
		for range 32 {
			select {
			case release <- struct{}{}:
			case <-ctx.Done():
				t.Fatal("requests did not finish")
			}
		}
		for range 32 {
			require.NoError(t, <-done)
		}
		require.EqualValues(t, 32, proofs.Load(), "reuse authenticated connections across windows")
	}
}

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
