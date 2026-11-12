package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
			synctest.Test(t, func(t *testing.T) {
				// The scoped producer is the processing boundary. Real processing verifies
				// PDF correctness separately; this exercises blocked writes on a socket
				// of the bubble's in-memory network, whose deadlines run on fake time.
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
				const timeout = 10 * time.Minute // the route's production limit
				server := httptest.NewTestServer(t, recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveRedactionRender(w, r, run, timeout) }), slog.New(slog.DiscardHandler), ""))
				client := server.Client()
				request := processing.RedactionRenderRequest{ReviewSHA256: strings.Repeat("b", 64), Masks: []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: strings.Repeat("c", 64), Pages: []int{1}}}}}
				body, err := json.Marshal(request)
				require.NoError(t, err)
				transport, ok := client.Transport.(*http.Transport)
				require.True(t, ok)
				conn, err := transport.DialContext(t.Context(), "tcp", "127.0.0.1:80")
				require.NoError(t, err)
				defer func() { _ = conn.Close() }()
				buffered, ok := conn.(interface{ SetReadBufferSize(size int) })
				require.True(t, ok)
				buffered.SetReadBufferSize(1024)
				_, err = fmt.Fprintf(conn, "POST /api/v1/redaction/render HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				require.NoError(t, err)
				if shutdown {
					requestCtx := <-started
					// Read response headers to establish that transmission has started, then stop reading.
					_, err := http.ReadResponse(bufio.NewReader(conn), nil)
					require.NoError(t, err)
					require.NoError(t, requestCtx.Err())
					synctest.Wait()
					select {
					case err := <-finished:
						t.Fatalf("render ended before shutdown: %v", err)
					default:
					}
					// Shutdown must interrupt the blocked write without waiting for the deadline.
					stopProcessing()
				} else {
					// The non-reading client holds the render until its deadline.
					synctest.Wait()
					select {
					case err := <-finished:
						t.Fatalf("render ended before its deadline: %v", err)
					default:
					}
					time.Sleep(timeout)
				}
				synctest.Wait()
				select {
				case err := <-finished:
					require.Error(t, err)
				default:
					t.Fatal("non-reading client held render beyond its deadline")
				}
				require.NoError(t, conn.Close())
				httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewReader(body))
				require.NoError(t, err)
				httpRequest.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(httpRequest)
				require.NoError(t, err)
				output, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, 200, resp.StatusCode)
				require.Equal(t, "ok", string(output))
			})
		})
	}
}
