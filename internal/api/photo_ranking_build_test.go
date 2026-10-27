package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

type photoQueryObserver struct {
	document.EmbeddingProvider

	calls atomic.Int32
	query func(context.Context, int32)
}

func (p *photoQueryObserver) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if len(inputs) == 1 && inputs[0].Kind == document.EmbeddingInputQueryText {
		count := p.calls.Add(1)
		if p.query != nil {
			p.query(ctx, count)
		}
	}
	return p.EmbeddingProvider.Embed(ctx, inputs, authorization)
}

func photoObservedFixture(t *testing.T) (*httptest.Server, *testStore, *photoQueryObserver) {
	t.Helper()
	provider := newProcessingTestEmbeddingProvider(t)
	provider.descriptor.InputKinds = []document.EmbeddingInputKind{document.EmbeddingInputImage}
	var err error
	provider.descriptor, err = document.NewEmbeddingDescriptor(provider.descriptor)
	require.NoError(t, err)
	observer := &photoQueryObserver{EmbeddingProvider: provider}
	ts, catalog, _ := photoImageHTTPProfilesFixture(t, map[string]document.EmbeddingProvider{"private": observer}, []string{"sunset.jpg"}, false)
	return ts, catalog, observer
}

func TestStaleSavedPhotoQueryDoesNotEmbed(t *testing.T) {
	t.Parallel()
	ts, catalog, observer := photoObservedFixture(t)
	saved, _ := createSavedQuery(t, ts.URL, "Synthetic meaning query", `{"mode":"semantic","text":"sunset"}`)
	updated := []byte(`{"mode":"semantic","text":"harbor"}`)
	_, err := catalog.UpdateSavedQuery(t.Context(), saved.ID, saved.Revision, store.SavedQueryPatch{Payload: &updated})
	require.NoError(t, err)
	response, body := rawJSONRequest(t, ts.URL, http.MethodPost, "/api/v1/saved-queries/"+saved.ID+"/runs", map[string]string{"X-Api-Key": testAPIKey, "If-Match": `"1"`}, `{"photos":true}`)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	require.Equal(t, "stale_revision", decodeProblem(t, body).Code)
	require.Zero(t, observer.calls.Load())
}

func TestPhotoRankingUsesSnapshotAdmissionAndDeadline(t *testing.T) {
	t.Parallel()
	for _, savedRoute := range []bool{false, true} {
		name := "browse"
		if savedRoute {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			ts, catalog, observer := photoObservedFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			path, body := "/api/v1/photos/assets/query", `{"query":{"mode":"semantic","text":"sunset"}}`
			if savedRoute {
				saved, _ := createSavedQuery(t, ts.URL, "Synthetic admitted query", `{"mode":"semantic","text":"sunset"}`)
				path, body = "/api/v1/saved-queries/"+saved.ID+"/runs", `{"photos":true}`
			}
			entered := make(chan time.Time, 3)
			release := make(chan struct{})
			observer.query = func(ctx context.Context, count int32) {
				deadline, _ := ctx.Deadline()
				entered <- deadline
				if count <= 2 {
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
			request := func() *httptest.ResponseRecorder {
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, ts.URL+path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Api-Key", testAPIKey)
				req.Header.Set("If-Match", `"1"`)
				response := httptest.NewRecorder()
				catalog.Server.Handler().ServeHTTP(response, req)
				return response
			}
			done := make(chan *httptest.ResponseRecorder, 2)
			start := time.Now()
			for range 2 {
				go func() { done <- request() }()
			}
			var deadlines []time.Time
			for range 2 {
				select {
				case deadline := <-entered:
					deadlines = append(deadlines, deadline)
				case response := <-done:
					t.Fatalf("request returned before provider execution: %d %s", response.Code, response.Body.String())
				}
			}
			third := request()
			close(release)
			for range 2 {
				response := <-done
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			}
			require.Equal(t, http.StatusTooManyRequests, third.Code, third.Body.String())
			var problem struct {
				Code string `json:"code"`
			}
			require.NoError(t, json.Unmarshal(third.Body.Bytes(), &problem))
			require.Equal(t, "snapshot_busy", problem.Code)
			require.EqualValues(t, 2, observer.calls.Load())
			for _, deadline := range deadlines {
				require.False(t, deadline.IsZero(), "provider execution needs the build deadline")
				require.WithinDuration(t, start.Add(30*time.Second), deadline, time.Second)
			}
		})
	}
}
