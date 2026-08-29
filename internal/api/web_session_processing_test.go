package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSessionProcessingSurfaceIsNarrowlyAllowed(t *testing.T) {
	t.Parallel()
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/processing/profiles"},
		{http.MethodPost, "/api/v1/processing/plans"},
		{http.MethodPost, "/api/v1/processing/jobs"},
		{http.MethodGet, "/api/v1/processing/jobs/" + strings.Repeat("a", 64)},
		{http.MethodGet, "/api/v1/renditions/" + strings.Repeat("b", 64)},
		{http.MethodGet, "/api/v1/coverage?profile=private&vault_uid=v&content_version_id=x"},
		{http.MethodPost, "/api/v1/search"},
	}
	for _, test := range allowed {
		request, err := http.NewRequest(test.method, "http://localhost"+test.path, nil)
		require.NoError(t, err)
		assert.True(t, webSessionRequestAllowed(request), test.method+" "+test.path)
	}
	for _, test := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/processing/consent/grants"},
		{http.MethodPost, "/api/v1/processing/consent/revocations"},
		{http.MethodPost, "/api/v1/derivatives/purge-plans"},
		{http.MethodPost, "/api/v1/derivatives/purge-jobs"},
	} {
		request, err := http.NewRequest(test.method, "http://localhost"+test.path, nil)
		require.NoError(t, err)
		assert.False(t, webSessionRequestAllowed(request), test.method+" "+test.path)
	}
}
