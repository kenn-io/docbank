package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestPackageContainerHTTPUploadAndSeal(t *testing.T) {
	srv, db := newPackageTestServer(t)
	raw := []byte("synthetic ZIP bytes")
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	declared := map[string]any{"container_id": "pkg-1", "sha256": digest, "size": len(raw)}
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, declared), nil)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var container api.PackageContainer
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &container))
	require.Equal(t, "pkg-1", container.ContainerID)
	require.Equal(t, "zip", container.Format)
	require.Equal(t, "uploading", container.State)

	req, err := http.NewRequest(http.MethodPut, srv.ts.URL+"/api/v1/packages/containers/pkg-1/chunks/0", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set(api.BlobHashHeader, digest)
	req.Header.Set(api.BlobSizeHeader, strconv.Itoa(len(raw)))
	resp, err := srv.ts.Client().Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	sealed := srv.call(t, http.MethodPost, "/api/v1/packages/containers/pkg-1/seal", "", nil)
	require.Equal(t, http.StatusOK, sealed.Code, sealed.Body.String())
	require.NoError(t, json.Unmarshal(sealed.Body.Bytes(), &container))
	require.Equal(t, "sealed", container.State)
	stored, err := db.MailboxContainer(t.Context(), "vault:"+db.VaultID(), "pkg-1")
	require.NoError(t, err)
	require.Equal(t, store.MailboxChunk{Index: 0, SHA256: digest, Size: int64(len(raw))}, stored.Chunks[0])
	got := srv.get(t, "/api/v1/packages/containers/pkg-1")
	require.Equal(t, http.StatusOK, got.Code)
	require.JSONEq(t, sealed.Body.String(), got.Body.String())
}

func TestPackageContainerIncompleteSealAndBrowserPUT(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	raw := []byte("zip")
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{"container_id": "pkg-2", "sha256": digest, "size": len(raw)}), nil)
	require.Equal(t, 201, created.Code, created.Body.String())
	bad := srv.call(t, http.MethodPost, "/api/v1/packages/containers/pkg-2/seal", "", nil)
	require.Equal(t, 422, bad.Code, bad.Body.String())
	request, err := http.NewRequest(http.MethodPut, srv.ts.URL+"/api/v1/packages/containers/pkg-2/chunks/0", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("X-Api-Key", "")
	request.Header.Set(api.WebSessionHeader, packageBrowserToken(t, srv))
	request.Header.Set(api.BlobHashHeader, digest)
	request.Header.Set(api.BlobSizeHeader, "3")
	response, err := srv.ts.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	aborted := srv.call(t, http.MethodDelete, "/api/v1/packages/containers/pkg-2", "", nil)
	require.Equal(t, http.StatusNoContent, aborted.Code, aborted.Body.String())
	missing := srv.get(t, "/api/v1/packages/containers/pkg-2")
	require.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
}

func TestPackageContainerSealRejectsWrongFullHash(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	raw := []byte("declared ZIP bytes")
	actual := sha256.Sum256(raw)
	wrong := sha256.Sum256([]byte("different ZIP bytes"))
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{
		"container_id": "wrong-hash-zip", "sha256": hex.EncodeToString(wrong[:]), "size": len(raw),
	}), nil)
	require.Equal(t, 201, created.Code, created.Body.String())
	request, err := http.NewRequest(http.MethodPut, srv.ts.URL+"/api/v1/packages/containers/wrong-hash-zip/chunks/0", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set(api.BlobHashHeader, hex.EncodeToString(actual[:]))
	request.Header.Set(api.BlobSizeHeader, strconv.Itoa(len(raw)))
	response, err := srv.ts.Client().Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 200, response.StatusCode)
	seal := srv.call(t, http.MethodPost, "/api/v1/packages/containers/wrong-hash-zip/seal", "", nil)
	require.Equal(t, 409, seal.Code, seal.Body.String())
	require.Contains(t, seal.Body.String(), "mailbox_conflict")
}

func TestPackageContainerIDCollisionAcrossBrowserOwnersIsConflict(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	firstToken := packageBrowserToken(t, srv)
	secondToken := packageBrowserToken(t, srv)
	hash := sha256.Sum256([]byte("synthetic"))
	body := mustPackageJSON(t, map[string]any{"container_id": "shared-zip-id", "sha256": hex.EncodeToString(hash[:]), "size": 9})
	first := srv.call(t, http.MethodPost, "/api/v1/packages/containers", body,
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: firstToken})
	require.Equal(t, 201, first.Code, first.Body.String())
	second := srv.call(t, http.MethodPost, "/api/v1/packages/containers", body,
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: secondToken})
	require.Equal(t, 409, second.Code, second.Body.String())
	require.Contains(t, second.Body.String(), "mailbox_conflict")
	foreign := srv.call(t, http.MethodGet, "/api/v1/packages/containers/shared-zip-id", "",
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: secondToken})
	require.Equal(t, 404, foreign.Code, foreign.Body.String())
}

func mustPackageJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	return string(b)
}

func packageBrowserToken(t *testing.T, srv *testServer) string {
	t.Helper()
	response := srv.call(t, http.MethodPost, "/api/daemon/web-session", "", nil)
	require.Equal(t, 201, response.Code, response.Body.String())
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &session))
	return session.Token
}
