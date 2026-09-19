package capselfhosted

import (
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbeTimeoutTakesPrecedenceOverCleanBodyEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &Client{
			deployment: Deployment{Origin: "https://example.com", ProbeTimeout: time.Second},
			http:       &http.Client{Transport: probeDeadlineTransport{}},
		}
		evidence, err := client.Probe(t.Context())
		require.NoError(t, err)
		require.Equal(t, ProbeProviderUnavailable, evidence.State)
	})
}

type probeDeadlineTransport struct{}

func (probeDeadlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	reader, writer := io.Pipe()
	go func() {
		// A TLS server can finish its response when cancellation closes the
		// connection. The body then reaches EOF without a read error.
		<-request.Context().Done()
		_, _ = io.WriteString(writer, `{"data":`)
		_ = writer.Close()
	}()
	return &http.Response{StatusCode: http.StatusOK, Body: reader, Header: make(http.Header)}, nil
}
