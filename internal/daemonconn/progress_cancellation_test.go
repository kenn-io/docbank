package daemonconn_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

// A short, complete response can already be buffered when a progress callback
// cancels. Cancellation must stop decoding even when no further network read
// is needed.
func TestProgressStreamsHonorCancellationWithBufferedResult(t *testing.T) {
	tests := []struct {
		name   string
		result string
		call   func(context.Context, *daemonconn.Connection, func()) error
	}{
		{"create", "snapshot", func(ctx context.Context, c *daemonconn.Connection, cancel func()) error {
			_, err := c.BackupCreateStream(ctx, daemonconn.BackupCreateOptions{}, func(api.BackupProgress) { cancel() })
			return err
		}},
		{"verify", "report", func(ctx context.Context, c *daemonconn.Connection, cancel func()) error {
			_, err := c.BackupVerifyStream(ctx, daemonconn.BackupVerifyOptions{}, func(api.BackupProgress) { cancel() })
			return err
		}},
		{"restore", "report", func(ctx context.Context, c *daemonconn.Connection, cancel func()) error {
			_, err := c.BackupRestoreStream(ctx, daemonconn.BackupRestoreOptions{}, func(api.BackupProgress) { cancel() })
			return err
		}},
		{"ingest", "report", func(ctx context.Context, c *daemonconn.Connection, cancel func()) error {
			_, err := c.IngestStream(ctx, []string{"/source"}, "/inbox", daemonconn.IngestOptions{}, func(api.IngestProgress) { cancel() })
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := "{\"type\":\"progress\",\"progress\":{}}\n" +
				"{\"type\":\"progress\",\"progress\":{}}\n" +
				"{\"type\":\"result\",\"" + tt.result + "\":{}}\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			err := tt.call(ctx, daemonconn.New(server.URL, serverKey), func() {
				calls++
				cancel()
			})
			require.ErrorIs(t, err, context.Canceled)
			require.Equal(t, 1, calls, "cancellation must stop further progress callbacks")
		})
	}
}
