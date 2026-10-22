package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/processing"
)

func TestRedactionRenderHTTPDeadline(t *testing.T) {
	t.Parallel()
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			// The scoped producer is the processing boundary. Real processing verifies
			// PDF correctness separately; this exercises blocked writes on a real socket.
			var active atomic.Bool
			var calls atomic.Int32
			finished := make(chan error, 1)
			started := make(chan context.Context, 1)
			processingCtx, stopProcessing := context.WithCancel(t.Context())
			defer stopProcessing()
			run := func(ctx context.Context, request processing.RedactionRenderRequest, send processing.RedactionSender) (resultErr error) {
				if !active.CompareAndSwap(false, true) {
					return processing.ErrRedactionPreparationBusy
				}
				count := calls.Add(1)
				defer func() {
					active.Store(false)
					if count == 1 {
						finished <- resultErr
					}
				}()
				data := "ok"
				if count == 1 {
					data = strings.Repeat("x", 32<<20)
				}
				if count == 1 && shutdown {
					started <- ctx
					ctx = processingCtx
				}
				err := send(ctx, strings.NewReader(data), processing.RedactionArchive{Size: int64(len(data)), SHA256: strings.Repeat("a", 64), Receipt: processing.RedactionReceipt{ReviewSHA256: request.ReviewSHA256}})
				return err
			}
			timeout := 250 * time.Millisecond
			if shutdown {
				timeout = time.Minute
			}
			server := httptest.NewServer(recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveRedactionRender(w, r, run, timeout) }), slog.New(slog.DiscardHandler)))
			defer server.Close()
			request := processing.RedactionRenderRequest{ReviewSHA256: strings.Repeat("b", 64), Masks: []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: strings.Repeat("c", 64), Pages: []int{1}}}}}
			body, err := json.Marshal(request)
			require.NoError(t, err)
			conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			tcp, ok := conn.(*net.TCPConn)
			require.True(t, ok)
			require.NoError(t, tcp.SetReadBuffer(1024))
			_, err = fmt.Fprintf(conn, "POST /api/v1/redaction/render HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			require.NoError(t, err)
			if shutdown {
				requestCtx := <-started
				// Read response headers to establish that transmission has started, then stop reading.
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, err := http.ReadResponse(bufio.NewReader(conn), nil)
				require.NoError(t, err)
				require.NoError(t, requestCtx.Err())
				stopProcessing()
			}
			select {
			case err := <-finished:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("non-reading client held render beyond its deadline")
			}
			require.NoError(t, conn.Close())
			resp, err := http.Post(server.URL, "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			output, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, 200, resp.StatusCode)
			require.Equal(t, "ok", string(output))
		})
	}
}
