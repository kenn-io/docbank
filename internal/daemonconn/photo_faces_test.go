package daemonconn

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoFaceClientValidatesResponses(t *testing.T) {
	t.Parallel()
	const versionID = "11111111-1111-4111-8111-111111111111"
	box := document.FaceBox{Width: 0.2, Height: 0.2}
	face := api.PhotoFace{FaceID: "22222222-2222-4222-8222-222222222222", ContentVersionID: versionID, Revision: 1, DetectorFingerprint: "manual", Box: box, AssertionID: "33333333-3333-4333-8333-333333333333", CreatedAt: "2000-01-01T00:00:00Z"}
	assertion := api.PhotoPersonAssertion{AssertionID: face.AssertionID, ContentVersionID: versionID, PersonID: "44444444-4444-4444-8444-444444444444", Role: "depicted", Action: "suppress", Revision: 2, RecordedAt: "2000-01-01T00:00:00Z"}
	overBound := make([]api.PhotoPersonAssertion, document.MaxPhotoRecordsPerVersion+1)
	for i := range overBound {
		overBound[i] = api.PhotoPersonAssertion{
			AssertionID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1), ContentVersionID: versionID,
			PersonID: fmt.Sprintf("00000000-0000-4000-8001-%012x", i+1), Role: "depicted", Action: "assert",
			Revision: 1, RecordedAt: "2000-01-01T00:00:00Z",
		}
	}
	wrongVersion := face
	wrongVersion.ContentVersionID = "55555555-5555-4555-8555-555555555555"
	wrongVersion.AssertionID = ""
	faceWithoutAssertion := face
	faceWithoutAssertion.AssertionID = ""
	cases := []struct {
		name   string
		status int
		body   any
		valid  bool
		create bool
	}{
		{name: "wrong version", body: api.PhotoFacePage{Faces: []api.PhotoFace{wrongVersion}, Assertions: []api.PhotoPersonAssertion{}}},
		{name: "malformed success", body: map[string]string{}},
		{name: "suppressed reference", body: api.PhotoFacePage{Faces: []api.PhotoFace{face}, Assertions: []api.PhotoPersonAssertion{assertion}}, valid: true},
		{name: "missing reference", body: api.PhotoFacePage{Faces: []api.PhotoFace{face}, Assertions: []api.PhotoPersonAssertion{}}},
		{name: "over bound", body: api.PhotoFacePage{Faces: []api.PhotoFace{faceWithoutAssertion}, Assertions: overBound}},
		{name: "invalid create response", status: http.StatusCreated, body: api.PhotoFaceResult{Face: &wrongVersion}, create: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				assert.NoError(t, json.MarshalWrite(w, tc.body))
			}))
			t.Cleanup(server.Close)
			client := New(server.URL, "synthetic-key")
			if tc.create {
				_, err := client.CreatePhotoFace(t.Context(), versionID, api.CreatePhotoFaceRequest{FaceID: face.FaceID, Box: &box})
				var decode *responseDecodeError
				require.ErrorAs(t, err, &decode)
				return
			}
			_, err := client.PhotoFaces(t.Context(), versionID)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
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
	require.EqualError(t, err, "error executing request: invalid photo face")
}
