package daemonconn

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadPreservesSourceReadFailure(t *testing.T) {
	t.Parallel()
	for _, response := range []bool{false, true} {
		name := "transport error"
		if response {
			name = "server rejection"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "notes.txt")
			require.NoError(t, os.WriteFile(path, []byte("abc"), 0o600))
			file, err := os.Open(path)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			c := New("http://daemon.invalid", "synthetic-key")
			c.hc.Transport = failingRoundTripper(func(request *http.Request) (*http.Response, error) {
				defer func() { _ = request.Body.Close() }()
				_, readErr := io.Copy(io.Discard, request.Body)
				require.ErrorIs(t, readErr, os.ErrClosed)
				if !response {
					return nil, readErr
				}
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"status":400,"detail":"incomplete upload"}`)),
				}, nil
			})
			_, err = c.Upload(t.Context(), 1, "notes.txt", "text/plain",
				"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", 3, file)
			require.ErrorIs(t, err, os.ErrClosed, "the caller must be able to identify a local read failure")
			if response {
				status, ok := ResponseStatus(err)
				assert.True(t, ok)
				assert.Equal(t, http.StatusBadRequest, status)
			}
		})
	}
}

func TestUploadTreatsUnusableSuccessReceiptAsUnknownOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty object", `{}`},
		{"truncated body", `{"status":"added"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New("http://daemon.invalid", "synthetic-key")
			c.hc.Transport = failingRoundTripper(func(request *http.Request) (*http.Response, error) {
				defer func() { _ = request.Body.Close() }()
				_, err := io.Copy(io.Discard, request.Body)
				require.NoError(t, err)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})
			receipt, err := c.Upload(t.Context(), 1, "notes.txt", "text/plain",
				"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", 3, strings.NewReader("abc"))
			require.Error(t, err, "malformed HTTP 200 receipt must have an unknown outcome")
			assert.True(t, IsResponseDecodeError(err), "malformed success receipts must be classified as unknown outcomes")
			assert.True(t, IsRetryableMutationError(err), "unknown outcomes must retain the caller's retry ID")
			assert.Empty(t, receipt, "malformed success receipts must return no partial receipt")
		})
	}
}
