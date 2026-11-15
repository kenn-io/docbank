package conversionworker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type faultingBackendSource struct{ err error }

type backendResponseReadFailure struct{ err error }

func (b backendResponseReadFailure) Read([]byte) (int, error) { return 0, b.err }
func (backendResponseReadFailure) Close() error               { return nil }

type cancelingBackendResponseBody struct{ cancel context.CancelFunc }

func (b cancelingBackendResponseBody) Read([]byte) (int, error) {
	b.cancel()
	return 0, errors.New("synthetic response body read failure")
}
func (cancelingBackendResponseBody) Close() error { return nil }

func (r faultingBackendSource) Read(p []byte) (int, error) {
	return copy(p, "source-prefix"), r.err
}

func TestBackendRejectionDoesNotMaskRetainedSourceReadFailure(t *testing.T) {
	fault := errors.New("synthetic retained source read fault")
	for _, test := range []struct {
		name   string
		source io.Reader
		drain  bool
		want   error
	}{
		{"source fault", faultingBackendSource{fault}, true, fault},
		{"complete rejected input", strings.NewReader("source"), true, nil},
		{"early definitive rejection", strings.NewReader("source"), false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := workerOptions(t)
			options.Client = &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
				if test.drain {
					_, _ = io.Copy(io.Discard, r.Body)
				}
				return backendResponse(r, 400, `{}`), nil
			})}
			w := openWorker(t, options)
			item := job{Filename: "synthetic.pdf", MediaType: "application/pdf", Bytes: 6, Fields: [][2]string{{"to_formats", "json"}, {"target_type", "inbody"}}}
			if test.want != nil {
				item.Bytes = 1024
			}
			status, _, err := w.submitBackendStream(t.Context(), item, test.source)
			require.Equal(t, 400, status)
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
