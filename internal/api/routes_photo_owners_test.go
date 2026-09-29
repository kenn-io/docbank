package api_test

import (
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

func TestPhotoOwnerRoutesAndVisibility(t *testing.T) {
	ts, fixture := newTestServer(t, nil)
	created, body := do(t, ts, http.MethodPost, "/api/v1/photos/owners", nil, map[string]string{"name": "Alice"})
	require.Equal(t, http.StatusCreated, created.StatusCode, body)
	var owner api.PhotoOwner
	require.NoError(t, json.Unmarshal([]byte(body), &owner))
	require.NotEmpty(t, owner.ID)
	created, body = do(t, ts, http.MethodPost, "/api/v1/photos/owners", nil, map[string]string{"name": "Bob"})
	require.Equal(t, http.StatusCreated, created.StatusCode, body)
	var other api.PhotoOwner
	require.NoError(t, json.Unmarshal([]byte(body), &other))

	raw, size, err := fixture.Blobs.Write(strings.NewReader("owner photo"))
	require.NoError(t, err)
	image, err := fixture.CreateFile(store.WithPhotoOwner(t.Context(), owner.ID), fixture.RootID(), "alice.jpg", raw, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := fixture.PhotoAssetForNode(store.WithPhotoOwner(t.Context(), owner.ID), image.ID)
	require.NoError(t, err)

	resp, body := get(t, ts, "/api/v1/photos/assets/"+asset.ID, map[string]string{"X-Docbank-Owner": owner.ID})
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/photos/assets/"+asset.ID, map[string]string{"X-Docbank-Owner": other.ID})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/nodes/"+strconv.FormatInt(image.ID, 10), map[string]string{"X-Docbank-Owner": other.ID})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)
	resp, body = get(t, ts, "/api/v1/documents", map[string]string{"X-Docbank-Owner": other.ID})
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.NotContains(t, body, "alice.jpg")
	resp, body = get(t, ts, "/api/v1/documents", map[string]string{"X-Docbank-Owner": owner.ID})
	assert.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, "alice.jpg")
	resp, body = get(t, ts, "/api/v1/nodes/"+strconv.FormatInt(image.ID, 10)+"/content", map[string]string{"X-Docbank-Owner": other.ID})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, body)

	updated, body := do(t, ts, http.MethodPatch, "/api/v1/photos/owners/"+owner.ID,
		map[string]string{"If-Match": strconv.Quote("1")}, map[string]string{"name": "Alice Smith"})
	assert.Equal(t, http.StatusOK, updated.StatusCode, body)
	assert.Equal(t, strconv.Quote("2"), updated.Header.Get("ETag"))

	listed, body := get(t, ts, "/api/v1/photos/owners", nil)
	assert.Equal(t, http.StatusOK, listed.StatusCode, body)
	assert.Contains(t, body, "Alice Smith")
}
