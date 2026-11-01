package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type photoQueryObserver struct {
	document.EmbeddingProvider

	calls atomic.Int32
}

func TestSavedPhotoRunsPreserveIdentityAndSkipUnneededEmbedding(t *testing.T) {
	t.Parallel()
	t.Run("semantic", func(t *testing.T) {
		t.Parallel()
		ts, _, observer := photoObservedFixture(t)
		saved, etag := createSavedQuery(t, ts.URL, "Synthetic saved photo query", `{"mode":"semantic","text":"sunset","sort":{"field":"name","direction":"asc"}}`)
		response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs", map[string]string{"X-Api-Key": testAPIKey, "If-Match": etag}, `{"photos":true}`)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		var result api.SavedQueryRunResult
		require.NoError(t, json.Unmarshal([]byte(body), &result))
		require.Equal(t, saved.Fingerprint, result.Run.QueryFingerprint)
		require.Equal(t, "semantic", result.Snapshot.RequestedMode)
		require.Equal(t, "semantic", result.Snapshot.ActualMode)
		require.Empty(t, result.Snapshot.FallbackReason)
		require.NotEmpty(t, result.Run.RunID)
		require.EqualValues(t, 1, result.Run.Total)
		require.EqualValues(t, 1, result.Snapshot.Total)
		require.EqualValues(t, 1, observer.calls.Load())
	})
	t.Run("stale revision", func(t *testing.T) {
		t.Parallel()
		ts, catalog, observer := photoObservedFixture(t)
		saved, etag := createSavedQuery(t, ts.URL, "Synthetic saved photo query", `{"mode":"semantic","text":"sunset"}`)
		updated := []byte(`{"mode":"semantic","text":"harbor"}`)
		_, err := catalog.UpdateSavedQuery(t.Context(), saved.ID, saved.Revision, store.SavedQueryPatch{Payload: &updated})
		require.NoError(t, err)
		response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs", map[string]string{"X-Api-Key": testAPIKey, "If-Match": etag}, `{"photos":true}`)
		require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
		require.Equal(t, "stale_revision", decodeProblem(t, body).Code)
		require.Zero(t, observer.calls.Load())
	})
}

func (p *photoQueryObserver) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if len(inputs) == 1 && inputs[0].Kind == document.EmbeddingInputQueryText {
		p.calls.Add(1)
	}
	return p.EmbeddingProvider.Embed(ctx, inputs, authorization)
}

func photoObservedFixture(t *testing.T) (*httptest.Server, *testStore, *photoQueryObserver) {
	t.Helper()
	provider := newProcessingTestEmbeddingProvider(t)
	provider.descriptor = photoImageDescriptor(t, provider.descriptor)
	observer := &photoQueryObserver{EmbeddingProvider: provider}
	ts, catalog, _ := photoImageHTTPProfilesFixture(t, map[string]document.EmbeddingProvider{"private": observer}, []string{"sunset.jpg"})
	return ts, catalog, observer
}
