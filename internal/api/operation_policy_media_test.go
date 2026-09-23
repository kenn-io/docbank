package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedCredentialsCannotEnterMediaContinuations(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/media/sources"},
		{http.MethodPost, "/api/v1/media/sources/synthetic-source/artifacts"},
		{http.MethodPost, "/api/v1/media/sources/synthetic-source/retry"},
	} {
		require.False(t, scopedRequestAllowed(route.method, route.path), route.path)
	}
}
