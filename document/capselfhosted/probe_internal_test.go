package capselfhosted

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbeTimeoutAtBodyEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":`))
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush response: %v", err)
				return
			}
			<-r.Context().Done()
		}))
		client := &Client{deployment: Deployment{Origin: "https://example.com", ProbeTimeout: time.Second}, http: server.Client()}
		evidence, err := client.Probe(t.Context())
		require.NoError(t, err)
		require.Equal(t, ProbeProviderUnavailable, evidence.State)
	})
}
