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
	for _, response := range []any{api.PhotoFacePage{Faces: []api.PhotoFace{face}, Assertions: []api.PhotoPersonAssertion{}}, map[string]string{}} {
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
	_, err := New(server.URL, "synthetic-key").CreatePhotoFace(t.Context(), version, api.CreatePhotoFaceRequest{FaceID: face.FaceID, Box: &face.Box})
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

func TestPhotoFaceClientAssertionBoundsAndReferences(t *testing.T) {
	t.Parallel()
	const versionID = "11111111-1111-4111-8111-111111111111"
	face := api.PhotoFace{FaceID: "22222222-2222-4222-8222-222222222222", ContentVersionID: versionID, Revision: 1, DetectorFingerprint: "manual", Box: document.FaceBox{Width: 0.2, Height: 0.2}, AssertionID: "33333333-3333-4333-8333-333333333333"}
	assertion := api.PhotoPersonAssertion{AssertionID: face.AssertionID, ContentVersionID: versionID, PersonID: "44444444-4444-4444-8444-444444444444", Role: "depicted", Action: "suppress", Revision: 2, RecordedAt: "2000-01-01T00:00:00.000000000Z"}
	for _, tc := range []struct {
		name       string
		assertions []api.PhotoPersonAssertion
		valid      bool
	}{
		{"suppressed reference", []api.PhotoPersonAssertion{assertion}, true},
		{"missing reference", []api.PhotoPersonAssertion{}, false},
		{"over bound", make([]api.PhotoPersonAssertion, 1001), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, api.PhotoFacePage{Faces: []api.PhotoFace{face}, Assertions: tc.assertions}))
			}))
			t.Cleanup(server.Close)
			_, err := New(server.URL, "synthetic-key").PhotoFaces(t.Context(), versionID)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
