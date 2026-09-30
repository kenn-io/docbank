package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoOwnerRoutes(t *testing.T) {
	ts, s := newTestServer(t, nil)
	alice, err := s.CreatePerson(t.Context(), "Alice", "operator")
	require.NoError(t, err)
	bob, err := s.CreatePerson(t.Context(), "Bob", "operator")
	require.NoError(t, err)
	ifMatch := func(revision int64) map[string]string {
		return map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(revision, 10))}
	}
	enroll := func(personID string, headers map[string]string) (*http.Response, string) {
		return do(t, ts, http.MethodPost, "/api/v1/photos/owners", headers, map[string]string{"person_id": personID})
	}
	for _, person := range []store.Person{alice, bob} {
		response, body := enroll(person.PersonID, ifMatch(person.Revision))
		require.Equal(t, http.StatusCreated, response.StatusCode, body)
		folder, err := s.NodeByPath(t.Context(), "/photos/"+person.PersonID)
		require.NoError(t, err, "enrollment creates the owner's folder")
		assert.True(t, folder.IsDir())
	}
	carol, err := s.CreatePerson(t.Context(), "Carol", "operator")
	require.NoError(t, err)
	for _, test := range []struct {
		personID string
		headers  map[string]string
		status   int
	}{
		{alice.PersonID, ifMatch(alice.Revision), http.StatusConflict},
		{alice.PersonID, nil, http.StatusPreconditionRequired},
		{carol.PersonID, ifMatch(carol.Revision + 1), http.StatusPreconditionFailed},
		{"00000000-0000-4000-8000-000000000000", ifMatch(1), http.StatusNotFound},
	} {
		response, body := enroll(test.personID, test.headers)
		assert.Equal(t, test.status, response.StatusCode, body)
	}

	// Renaming and retiring go through the person routes.
	person := "/api/v1/people/by-id/" + alice.PersonID
	response, body := do(t, ts, http.MethodPatch, person, ifMatch(alice.Revision), map[string]string{"display_name": "Alice Smith"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, person+"/retire", ifMatch(alice.Revision+1), nil)
	assert.Equal(t, http.StatusConflict, response.StatusCode, body)
	response, body = get(t, ts, "/api/v1/photos/owners", nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var owners []api.PhotoOwner
	require.NoError(t, json.Unmarshal([]byte(body), &owners))
	require.Len(t, owners, 2)
	assert.Equal(t, "Alice Smith", owners[0].Name, "the owner's name follows the person")

	hash, size, err := s.Blobs.Write(strings.NewReader("alice photo"))
	require.NoError(t, err)
	aliceFolder, err := s.NodeByPath(t.Context(), "/photos/"+alice.PersonID)
	require.NoError(t, err)
	photo, err := s.CreateFile(t.Context(), aliceFolder.ID, "alice.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	remove := func(personID string, headers map[string]string) (*http.Response, string) {
		return do(t, ts, http.MethodDelete, "/api/v1/photos/owners/"+personID, headers, nil)
	}
	response, body = remove(alice.PersonID, ifMatch(alice.Revision))
	assert.Equal(t, http.StatusPreconditionFailed, response.StatusCode, body)
	response, body = remove(alice.PersonID, ifMatch(alice.Revision+1))
	assert.Equal(t, http.StatusConflict, response.StatusCode, body)
	_, _, err = s.Trash(t.Context(), photo.ID, photo.Revision)
	require.NoError(t, err)
	response, body = remove(alice.PersonID, ifMatch(alice.Revision+1))
	assert.Equal(t, http.StatusConflict, response.StatusCode, "a trashed node still holds the folder: "+body)
	response, body = get(t, ts, "/api/v1/photos/owners", issuePhotoOwnerSession(t, ts, map[string]string{api.PhotoOwnerHeader: alice.PersonID}))
	assert.NotEqual(t, http.StatusOK, response.StatusCode, "owner administration is master-only: "+body)

	// Scoped consent binds to the request's owner, not a principal the body names.
	digest := func(seed string) string { sum := sha256.Sum256([]byte(seed)); return hex.EncodeToString(sum[:]) }
	consent := func(principal string) map[string]any {
		return map[string]any{"principal": principal, "scope": "attachment:synthetic",
			"profile_fingerprint": digest("profile"), "disclosure_fingerprint": digest("disclosure"),
			"input_classes": []string{"original_source"}}
	}
	asAlice := map[string]string{api.PhotoOwnerHeader: alice.PersonID}
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consents", asAlice, consent("operator:synthetic"))
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consents", asAlice, consent("owner:"+alice.PersonID))
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consents/revoke", map[string]string{api.PhotoOwnerHeader: bob.PersonID},
		map[string]string{"principal": "owner:" + alice.PersonID, "scope": "attachment:synthetic"})
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, "another owner cannot revoke Alice's grant: "+body)

	response, body = remove(bob.PersonID, ifMatch(bob.Revision))
	assert.Equal(t, http.StatusNoContent, response.StatusCode, body)
	_, err = s.NodeByPath(t.Context(), "/photos/"+bob.PersonID)
	assert.NoError(t, err, "the empty folder stays as an ordinary folder")
}
