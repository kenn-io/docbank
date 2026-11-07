package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNumberedPublicationDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var controllerErr, usualErr, publicationErr error
		handler := timeoutMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			controllerErr = clearRequestReadDeadline(r.Context())
			time.Sleep(61 * time.Second)
			usualErr = r.Context().Err()
			time.Sleep(9 * time.Minute)
			publicationErr = r.Context().Err()
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, redactionPublicationsPath, nil))
		require.NoError(t, controllerErr)
		require.NoError(t, usualErr, "publication must outlive the usual request deadline")
		require.ErrorIs(t, publicationErr, context.DeadlineExceeded, "publication must remain bounded by ten minutes")
	})
}
