package api_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func TestFaceSimilarityRouteRequiresConsentAndReview(t *testing.T) {
	t.Parallel()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/faces", r.URL.Path)
		vector := make([]float32, 512)
		vector[0] = 1
		assert.NoError(t, json.MarshalWrite(w, embedclient.FaceResponse{Model: embedclient.FaceModel, Revision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe, Data: []embedclient.FaceResult{{Index: 0, Embedding: vector}}}))
	}))
	t.Cleanup(model.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:      embedconfig.Model{Name: embedclient.FaceModel, Dimensions: 512, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
		Deployment: embedconfig.Deployment{BaseURL: model.URL + "/v1"},
	})
	require.NoError(t, err)
	ts, catalog := newTestServer(t, func(d *api.Deps) {
		d.FaceClient = func(context.Context) (*embedclient.Client, error) { return client, nil }
	})
	person, err := catalog.CreatePerson(t.Context(), "Synthetic Reference", "operator")
	require.NoError(t, err)
	target := createTestSimilarityFace(t, catalog, "target.jpg", "")
	reference := createTestSimilarityFace(t, catalog, "reference.jpg", person.PersonID)
	path := "/api/v1/photos/versions/" + target.ContentVersionID + "/face-similarity"
	request := map[string]any{"face_id": target.FaceID, "references": []processing.FaceReference{reference}, "allow_local_face_recognition": false}
	response, body := do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	request["allow_local_face_recognition"] = true
	response, body = do(t, ts, http.MethodPost, path, nil, request)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	var result processing.FaceSimilarityResult
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	require.Len(t, result.Suggestions, 1)
	assert.Equal(t, person.PersonID, result.Suggestions[0].PersonID)
	assert.Zero(t, result.Suggestions[0].Distance)
	faces, _, _, err := catalog.PhotoFacePersonNames(t.Context(), target.ContentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	assert.Empty(t, faces[0].AssertionID)
	response, body = do(t, ts, http.MethodPost, "/api/v1/photos/versions/"+target.ContentVersionID+"/face-decisions", nil,
		api.PhotoFaceDecisionRequest{FaceID: target.FaceID, PersonID: person.PersonID, Action: "name", Revision: result.FaceRevision})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	faces, _, _, err = catalog.PhotoFacePersonNames(t.Context(), target.ContentVersionID)
	require.NoError(t, err)
	assert.NotEmpty(t, faces[0].AssertionID)
	response, body = do(t, ts, http.MethodPost, path, map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}, request)
	assert.Equal(t, http.StatusForbidden, response.StatusCode, body)
}

func createTestSimilarityFace(t *testing.T, catalog *testStore, name, personID string) processing.FaceReference {
	t.Helper()
	hash, size, err := catalog.Blobs.WriteContext(t.Context(), strings.NewReader("synthetic image source "+name))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), name, hash, size, "image/jpeg")
	require.NoError(t, err)
	publishTestPhotoPreview(t, catalog, node)
	face, _, err := catalog.CreatePhotoFace(t.Context(), node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .5, Height: .5}, personID)
	require.NoError(t, err)
	return processing.FaceReference{ContentVersionID: node.CurrentVersionID, FaceID: face.FaceID}
}

func TestFaceSimilarityRouteKeepsSlowInferenceAlive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		model := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/v1/faces", r.URL.Path)
			// Each inference exceeds Huma's default five-second socket deadline.
			time.Sleep(6 * time.Second)
			vector := make([]float32, 512)
			vector[0] = 1
			assert.NoError(t, json.MarshalWrite(w, embedclient.FaceResponse{
				Model: embedclient.FaceModel, Revision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe,
				Data: []embedclient.FaceResult{{Index: 0, Embedding: vector}},
			}))
		}))
		client, err := embedclient.New(embedclient.Options{
			Model:      embedconfig.Model{Name: embedclient.FaceModel, Dimensions: 512, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
			Deployment: embedconfig.Deployment{BaseURL: model.URL + "/v1"}, HTTP: model.Client(),
		})
		require.NoError(t, err)
		ts, catalog := newBubbleTestServer(t, func(d *api.Deps) {
			d.FaceClient = func(context.Context) (*embedclient.Client, error) { return client, nil }
		})
		person, err := catalog.CreatePerson(t.Context(), "Synthetic Reference", "operator")
		require.NoError(t, err)
		target := createTestSimilarityFace(t, catalog, "slow-target.jpg", "")
		reference := createTestSimilarityFace(t, catalog, "slow-reference.jpg", person.PersonID)
		started := time.Now()
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/versions/"+target.ContentVersionID+"/face-similarity", nil,
			api.FaceSimilarityRequest{FaceID: target.FaceID, AllowLocalFaceRecognition: true, References: []processing.FaceReference{reference}})
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		require.GreaterOrEqual(t, time.Since(started), 12*time.Second)
		var result processing.FaceSimilarityResult
		require.NoError(t, json.Unmarshal([]byte(body), &result))
		require.Len(t, result.Suggestions, 1)
		assert.Equal(t, person.PersonID, result.Suggestions[0].PersonID)
	})
}
