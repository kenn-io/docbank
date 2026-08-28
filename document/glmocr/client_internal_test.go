package glmocr

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/providerhttp"
)

func TestDefaultClientRefusesEgressDestinationDrift(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "provider")
	}))
	defer provider.Close()
	var driftRequests atomic.Int32
	drift := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		driftRequests.Add(1)
		_, _ = io.WriteString(w, "drift")
	}))
	defer drift.Close()
	normalize, err := document.NewNormalizePolicy(1_000_000)
	require.NoError(t, err)
	policy, err := NewPolicy(PolicyConfig{
		Endpoint: provider.URL + "/glmocr/parse", MaxDocumentBytes: 1 << 20,
		MaxResponseBytes: 1 << 20, MaxUnits: 1, NormalizePolicy: normalize,
	})
	require.NoError(t, err)
	client, err := NewClient(policy, ClientConfig{Timeout: time.Second})
	require.NoError(t, err)

	response, err := client.http.Get(drift.URL)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, providerhttp.ErrDestinationDenied)
	assert.Zero(t, driftRequests.Load())
}
