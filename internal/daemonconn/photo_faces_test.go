package daemonconn

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoFaceClientRejectsWrongVersionAndMalformedSuccess(t *testing.T) {
	t.Parallel()
	const version = "11111111-1111-4111-8111-111111111111"
	face := api.PhotoFace{FaceID: "22222222-2222-4222-8222-222222222222", ContentVersionID: "33333333-3333-4333-8333-333333333333", Revision: 1, DetectorFingerprint: "manual", Box: document.FaceBox{Width: 0.2, Height: 0.2}}
	for _, response := range []any{api.PhotoFacePage{Faces: []api.PhotoFace{face}}, map[string]string{}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, response))
		}))
		t.Cleanup(server.Close)
		_, err := New(server.URL, "synthetic-key").PhotoFaces(t.Context(), version)
		require.Error(t, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.MarshalWrite(w, api.PhotoFaceResult{Face: &face}))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "synthetic-key").CreatePhotoFace(t.Context(), version, api.CreatePhotoFaceRequest{Box: &face.Box})
	var decode *responseDecodeError
	require.ErrorAs(t, err, &decode)
}

func TestPhotoFaceClientMapsValidationError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		assert.NoError(t, json.MarshalWrite(w, api.Error{Status: http.StatusUnprocessableEntity, Code: "invalid_photo_face", Detail: "invalid photo face"}))
	}))
	t.Cleanup(server.Close)
	_, err := New(server.URL, "synthetic-key").PhotoFaces(t.Context(), "11111111-1111-4111-8111-111111111111")
	require.ErrorIs(t, err, store.ErrInvalidPhotoFace)
}
