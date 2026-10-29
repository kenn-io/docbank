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
	for _, tc := range []struct {
		name            string
		payload         string
		updated         string
		wantMode        string
		wantStatus      int
		wantProblem     string
		wantNoEmbedding bool
	}{
		{name: "semantic", payload: `{"mode":"semantic","text":"sunset","sort":{"field":"name","direction":"asc"}}`, wantMode: "semantic", wantStatus: http.StatusOK},
		{name: "hybrid", payload: `{"mode":"hybrid","text":"sunset","sort":{"field":"name","direction":"asc"}}`, wantMode: "hybrid", wantStatus: http.StatusOK},
		{name: "cleared text", payload: `{"mode":"semantic","text":"   ","filters":{"extensions":["jpg"]},"sort":{"field":"name","direction":"asc"}}`, wantStatus: http.StatusOK, wantNoEmbedding: true},
		{name: "stale revision", payload: `{"mode":"semantic","text":"sunset"}`, updated: `{"mode":"semantic","text":"harbor"}`, wantStatus: http.StatusPreconditionFailed, wantProblem: "stale_revision", wantNoEmbedding: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, catalog, observer := photoObservedFixture(t)
			saved, etag := createSavedQuery(t, ts.URL, "Synthetic saved photo query", tc.payload)
			if tc.updated != "" {
				updated := []byte(tc.updated)
				_, err := catalog.UpdateSavedQuery(t.Context(), saved.ID, saved.Revision, store.SavedQueryPatch{Payload: &updated})
				require.NoError(t, err)
			}
			response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs", map[string]string{"X-Api-Key": testAPIKey, "If-Match": etag}, `{"photos":true}`)
			require.Equal(t, tc.wantStatus, response.StatusCode, body)
			if tc.wantProblem != "" {
				require.Equal(t, tc.wantProblem, decodeProblem(t, body).Code)
			} else {
				var result api.SavedQueryRunResult
				require.NoError(t, json.Unmarshal([]byte(body), &result))
				require.Equal(t, saved.Fingerprint, result.Run.QueryFingerprint)
				if tc.wantMode != "" {
					require.Equal(t, tc.wantMode, result.Snapshot.RequestedMode)
					require.Equal(t, tc.wantMode, result.Snapshot.ActualMode)
					require.Empty(t, result.Snapshot.FallbackReason)
					require.NotEmpty(t, result.Run.RunID)
					require.EqualValues(t, 1, result.Run.Total)
					require.EqualValues(t, 1, result.Snapshot.Total)
				}
			}
			if tc.wantNoEmbedding {
				require.Zero(t, observer.calls.Load())
			}
		})
	}
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
	ts, catalog, _ := photoImageHTTPProfilesFixture(t, map[string]document.EmbeddingProvider{"private": observer}, []string{"sunset.jpg"}, false)
	return ts, catalog, observer
}

func TestPhotoRankingRoutesReachImageQueryProvider(t *testing.T) {
	t.Parallel()
	for _, savedRoute := range []bool{false, true} {
		name := "browse"
		if savedRoute {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ts, _, observer := photoObservedFixture(t)
			path, body := "/api/v1/photos/assets/query", `{"query":{"mode":"semantic","text":"sunset"}}`
			if savedRoute {
				saved, _ := createSavedQuery(t, ts.URL, "Synthetic ranked query", `{"mode":"semantic","text":"sunset"}`)
				path, body = "/api/v1/saved-queries/"+saved.ID+"/runs", `{"photos":true}`
			}
			response, result := rawJSONRequest(t, ts.URL, http.MethodPost, path, map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, body)
			require.Equal(t, http.StatusOK, response.StatusCode, result)
			require.EqualValues(t, 1, observer.calls.Load())
		})
	}
}
