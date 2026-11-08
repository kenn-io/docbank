package api_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNumberedPublicationStalledBody(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		server, _ := newBubbleTestServer(t, nil)
		keyed, ok := server.Client().Transport.(*apiKeyTransport)
		require.True(t, ok)
		transport, ok := keyed.next.(*http.Transport)
		require.True(t, ok)
		conn, err := transport.DialContext(t.Context(), "tcp", "127.0.0.1:80")
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		_, err = fmt.Fprintf(conn, "POST /api/v1/redaction/publications HTTP/1.1\r\nHost: localhost\r\nX-Api-Key: %s\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{", testAPIKey)
		require.NoError(t, err)
		type result struct {
			status int
			err    error
		}
		done := make(chan result, 1)
		go func() {
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				done <- result{err: err}
				return
			}
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			done <- result{status: response.StatusCode, err: err}
		}()
		synctest.Wait()
		time.Sleep(59 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("upload ended before its body deadline: %+v", got)
		default:
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			require.NoError(t, got.err)
			require.Equal(t, http.StatusRequestTimeout, got.status)
		default:
			t.Fatal("stalled publication upload survived its body deadline")
		}
	})
}
