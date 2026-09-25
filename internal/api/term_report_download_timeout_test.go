package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

func TestTermReportDirectDownloadsOutliveRequestTimeout(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	createFileWithContent(t, ts, catalog, "/synthetic.txt", "synthetic report")
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only"}
	// Distinct terms keep both artifacts larger than the stream copy buffer.
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic synthetic terms keep the fixture ZIP from compressing below the copy buffer.
	for i := 1; i <= 32; i++ {
		expression := make([]byte, 2048)
		for j := range expression {
			expression[j] = 'a' + byte(rng.IntN(26))
		}
		request.Terms = append(request.Terms, report.Term{Number: i, Expression: string(expression), Syntax: "simple",
			Dates: report.DateRange{Start: "2026-01-01", End: "2026-12-31"}})
	}
	response, body := do(t, ts, http.MethodPost, "/api/v1/search-exports", nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var summary report.Summary
	require.NoError(t, json.Unmarshal([]byte(body), &summary))
	for _, format := range []string{"csv", "bundle"} {
		t.Run(format, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server := httptest.NewTestServer(t, catalog.Server.Handler())
				client := server.Client()
				transport, ok := client.Transport.(*http.Transport)
				require.True(t, ok)
				dial := transport.DialContext
				transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := dial(ctx, network, address)
					if err == nil {
						// Model a connected client whose unread response fills the network buffer.
						buffered, ok := conn.(interface{ SetReadBufferSize(size int) })
						if !ok {
							return nil, errors.Join(errors.New("test connection cannot bound its read buffer"), conn.Close())
						}
						buffered.SetReadBufferSize(1024)
					}
					return conn, err
				}
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
					server.URL+"/api/v1/search-exports/"+summary.ID+"/"+format, nil)
				require.NoError(t, err)
				req.Header.Set("X-Api-Key", testAPIKey)
				response, err := client.Do(req)
				require.NoError(t, err)
				defer func() { require.NoError(t, response.Body.Close()) }()
				require.Equal(t, http.StatusOK, response.StatusCode)
				time.Sleep(61 * time.Second)
				stream := daemonconn.TermReportStream{ReadCloser: response.Body, Size: response.ContentLength,
					SHA256: response.Header.Get("X-Docbank-Report-Sha256")}
				written, err := stream.CopyVerified(io.Discard)
				require.NoError(t, err, "a slow download must retain the complete size and digest")
				require.Equal(t, response.ContentLength, written)
			})
		})
	}
}
