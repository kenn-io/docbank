package processing

import (
	"context"
	"encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

func TestFaceSimilarityRanksReviewedReferencesWithoutNaming(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	person, err := f.catalog.CreatePerson(ctx, "Synthetic Person", "operator")
	require.NoError(t, err)
	other, err := f.catalog.CreatePerson(ctx, "Synthetic Person", "operator")
	require.NoError(t, err)
	face := func(x float64, personID string) FaceReference {
		created, _, e := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: x, Width: .2, Height: .2}, personID)
		require.NoError(t, e)
		return FaceReference{ContentVersionID: f.node.CurrentVersionID, FaceID: created.FaceID}
	}
	target := face(0, "")
	far := face(.2, person.PersonID)
	near := face(.4, person.PersonID)
	null := face(.6, other.PersonID)
	client := syntheticFaceClient(t, func(box embedclient.FaceBox) []float32 {
		if box.X > .5 {
			return nil
		}
		v := make([]float32, 512)
		switch box.X {
		case .2:
			v[1] = 1
		case .4:
			v[0], v[1] = .8, .6
		default:
			v[0] = 1
		}
		return v
	})
	loader := func(context.Context) (*embedclient.Client, error) { return client, nil }
	result, err := CompareFaces(ctx, f.catalog, f.blobs, loader, target, []FaceReference{far, near, null})
	require.NoError(t, err)
	require.Len(t, result.Suggestions, 1)
	assert.Equal(t, "suggested", result.Suggestions[0].State)
	assert.Equal(t, person.PersonID, result.Suggestions[0].PersonID)
	assert.Equal(t, near, result.Suggestions[0].Reference)
	assert.InDelta(t, math.Sqrt(.4), result.Suggestions[0].Distance, 1e-6)
	assert.NotEmpty(t, result.Suggestions[0].PreviewChecksum)
	assert.Equal(t, embedclient.FaceModel, result.Model)
	faces, _, _, err := f.catalog.PhotoFacePersonNames(ctx, target.ContentVersionID)
	require.NoError(t, err)
	for _, saved := range faces {
		if saved.FaceID == target.FaceID {
			assert.Empty(t, saved.AssertionID, "comparison must not identify the target")
		}
	}
	_, err = f.catalog.DecidePhotoFace(ctx, target.ContentVersionID, store.PhotoFaceDecisionRequest{FaceID: target.FaceID, PersonID: person.PersonID, Action: "reject", Revision: 1})
	require.NoError(t, err)
	result, err = CompareFaces(ctx, f.catalog, f.blobs, loader, target, []FaceReference{near})
	require.NoError(t, err)
	assert.Empty(t, result.Suggestions, "a rejected person must not be suggested again")
}

func TestFaceSimilarityRefusesUnreviewedOrHiddenEvidence(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	target, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, "")
	require.NoError(t, err)
	ref, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, "")
	require.NoError(t, err)
	loader := func(context.Context) (*embedclient.Client, error) {
		assert.Fail(t, "ineligible evidence must fail before local model discovery")
		return nil, store.ErrInvalidPhotoFace
	}
	_, err = CompareFaces(ctx, f.catalog, f.blobs, loader, FaceReference{f.node.CurrentVersionID, target.FaceID}, []FaceReference{{f.node.CurrentVersionID, ref.FaceID}})
	require.ErrorIs(t, err, store.ErrInvalidPhotoFace)
	require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = f.catalog.SetPhotoAssetHidden(ctx, f.asset.ID, f.asset.Revision, true)
	require.NoError(t, err)
	_, err = CompareFaces(ctx, f.catalog, f.blobs, loader, FaceReference{f.node.CurrentVersionID, target.FaceID}, []FaceReference{{f.node.CurrentVersionID, ref.FaceID}})
	require.Error(t, err)
}

func TestFaceSimilarityNullInvalidAndConcurrentEvidence(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"null target", "invalid vector", "person changed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t)
			ctx := t.Context()
			person, err := f.catalog.CreatePerson(ctx, "Synthetic Before", "operator")
			require.NoError(t, err)
			target, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, "")
			require.NoError(t, err)
			ref, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, person.PersonID)
			require.NoError(t, err)
			client := syntheticFaceClient(t, func(box embedclient.FaceBox) []float32 {
				if scenario == "null target" {
					return nil
				}
				v := make([]float32, 512)
				v[0] = 1
				if scenario == "invalid vector" {
					return v[:511]
				}
				if box.X == 0 {
					_, err := f.catalog.UpdatePerson(ctx, person.PersonID, person.Revision, "Synthetic After")
					assert.NoError(t, err)
				}
				return v
			})
			result, err := CompareFaces(ctx, f.catalog, f.blobs, func(context.Context) (*embedclient.Client, error) { return client, nil },
				FaceReference{f.node.CurrentVersionID, target.FaceID}, []FaceReference{{f.node.CurrentVersionID, ref.FaceID}})
			switch scenario {
			case "null target":
				require.NoError(t, err)
				assert.Equal(t, "unavailable", result.State)
			case "invalid vector":
				require.Error(t, err)
			case "person changed":
				require.ErrorIs(t, err, store.ErrStaleRevision)
			}
			assert.Empty(t, result.Suggestions)
		})
	}
}

func TestFaceSimilarityDoesNotTreatARetiredNamedTargetAsUnknown(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	former, err := f.catalog.CreatePerson(ctx, "Synthetic Retired", "operator")
	require.NoError(t, err)
	other, err := f.catalog.CreatePerson(ctx, "Synthetic Reference", "operator")
	require.NoError(t, err)
	target, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, former.PersonID)
	require.NoError(t, err)
	ref, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.NewString(), document.FaceBox{X: .5, Width: .2, Height: .2}, other.PersonID)
	require.NoError(t, err)
	former, _, err = f.catalog.PersonByID(ctx, former.PersonID)
	require.NoError(t, err)
	_, err = f.catalog.RetirePerson(ctx, former.PersonID, former.Revision)
	require.NoError(t, err)
	client := syntheticFaceClient(t, func(embedclient.FaceBox) []float32 {
		v := make([]float32, 512)
		v[0] = 1
		return v
	})
	_, err = CompareFaces(ctx, f.catalog, f.blobs, func(context.Context) (*embedclient.Client, error) { return client, nil },
		FaceReference{f.node.CurrentVersionID, target.FaceID}, []FaceReference{{f.node.CurrentVersionID, ref.FaceID}})
	require.ErrorIs(t, err, store.ErrNotFound, "a retired named target must not become an unknown face")
}

func syntheticFaceClient(t *testing.T, vector func(embedclient.FaceBox) []float32) *embedclient.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/faces", r.URL.Path)
		var request embedclient.FaceRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) || !assert.Len(t, request.Boxes, 1) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.NotEmpty(t, request.Image)
		response := embedclient.FaceResponse{Model: embedclient.FaceModel, Revision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe,
			Data: []embedclient.FaceResult{{Index: 0, Embedding: vector(request.Boxes[0])}}}
		assert.NoError(t, json.MarshalWrite(w, response, json.FormatNilSliceAsNull(true)))
	}))
	t.Cleanup(server.Close)
	client, err := embedclient.New(embedclient.Options{
		Model:      embedconfig.Model{Name: embedclient.FaceModel, Dimensions: 512, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
		Deployment: embedconfig.Deployment{BaseURL: server.URL + "/v1"},
	})
	require.NoError(t, err)
	return client
}
