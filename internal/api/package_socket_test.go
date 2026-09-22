package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestBrowserPackageBytesUseOwnedSocketAndSeal(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	token := packageBrowserToken(t, srv)
	other := packageBrowserToken(t, srv)
	raw := []byte("synthetic ZIP payload")
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	body := mustPackageJSON(t, map[string]any{"container_id": "browser-zip", "sha256": digest, "size": len(raw)})
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", body, map[string]string{"X-Api-Key": "", api.WebSessionHeader: token})
	require.Equal(t, 201, created.Code, created.Body.String())
	foreign := srv.call(t, http.MethodGet, "/api/v1/packages/containers/browser-zip", "", map[string]string{"X-Api-Key": "", api.WebSessionHeader: other})
	require.Equal(t, 404, foreign.Code, foreign.Body.String())

	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}}})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "authenticate", "token": token, "nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}))
	var reply struct {
		Type        string `json:"type"`
		Code        string `json:"code"`
		ContainerID string `json:"container_id"`
	}
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "authenticated", reply.Type)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin", "request_id": "zip", "container_id": "browser-zip", "expected_hash": digest, "expected_size": len(raw)}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "ready", reply.Type, reply.Code)
	require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, raw[:7]))
	require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, raw[7:]))
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "zip"}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "package_container_receipt", reply.Type, reply.Code)
	require.Equal(t, "browser-zip", reply.ContainerID)
	sealed := srv.call(t, http.MethodPost, "/api/v1/packages/containers/browser-zip/seal", "", map[string]string{"X-Api-Key": "", api.WebSessionHeader: token})
	require.Equal(t, 200, sealed.Code, sealed.Body.String())
	var container api.PackageContainer
	require.NoError(t, json.Unmarshal(sealed.Body.Bytes(), &container))
	require.Equal(t, "sealed", container.State)
}

func TestBrowserPackageSocketRefusesDifferentOwner(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	owner := packageBrowserToken(t, srv)
	other := packageBrowserToken(t, srv)
	raw := []byte("zip")
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{"container_id": "private-zip", "sha256": digest, "size": len(raw)}), map[string]string{"X-Api-Key": "", api.WebSessionHeader: owner})
	require.Equal(t, 201, created.Code, created.Body.String())
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}}})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "authenticate", "token": other, "nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}))
	var reply struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "authenticated", reply.Type)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin", "request_id": "foreign", "container_id": "private-zip", "expected_hash": digest, "expected_size": len(raw)}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "error", reply.Type)
	require.Equal(t, "not_found", reply.Code)
}

func TestBrowserPackageSocketSplitsOneMiBFramesAt64MiBBoundary(t *testing.T) {
	srv, db := newPackageTestServer(t)
	token := packageBrowserToken(t, srv)
	raw := bytes.Repeat([]byte("Z"), int(store.MailboxChunkBytes)+17)
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{"container_id": "split-zip", "sha256": digest, "size": len(raw)}), map[string]string{"X-Api-Key": "", api.WebSessionHeader: token})
	require.Equal(t, 201, created.Code, created.Body.String())
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}}})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "authenticate", "token": token, "nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}))
	var reply struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "authenticated", reply.Type)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin", "request_id": "split", "container_id": "split-zip", "expected_hash": digest, "expected_size": len(raw)}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "ready", reply.Type, reply.Code)
	for offset := 0; offset < len(raw); offset += 1 << 20 {
		require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, raw[offset:min(offset+(1<<20), len(raw))]))
	}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "split"}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "package_container_receipt", reply.Type, reply.Code)
	owner := sha256.Sum256([]byte(token))
	container, err := db.MailboxContainer(t.Context(), hex.EncodeToString(owner[:]), "split-zip")
	require.NoError(t, err)
	require.Len(t, container.Chunks, 2)
	require.Equal(t, store.MailboxChunkBytes, container.Chunks[0].Size)
	require.EqualValues(t, 17, container.Chunks[1].Size)
}

func TestBrowserPackageSocketRejectsOversizedFrame(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	token := packageBrowserToken(t, srv)
	raw := bytes.Repeat([]byte("F"), (1<<20)+1)
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{"container_id": "frame-zip", "sha256": digest, "size": len(raw)}), map[string]string{"X-Api-Key": "", api.WebSessionHeader: token})
	require.Equal(t, 201, created.Code, created.Body.String())
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}}})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "authenticate", "token": token, "nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}))
	var reply struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "authenticated", reply.Type)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin", "request_id": "frame", "container_id": "frame-zip", "expected_hash": digest, "expected_size": len(raw)}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "ready", reply.Type)
	require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, raw))
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "frame"}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "error", reply.Type)
	require.Equal(t, "validation", reply.Code)
}

func TestBrowserPackageSocketRejectsChangedBytes(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	token := packageBrowserToken(t, srv)
	expected := []byte("expected zip")
	actual := []byte("altered  zip")
	hash := sha256.Sum256(expected)
	digest := hex.EncodeToString(hash[:])
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{"container_id": "changed-zip", "sha256": digest, "size": len(expected)}), map[string]string{"X-Api-Key": "", api.WebSessionHeader: token})
	require.Equal(t, 201, created.Code, created.Body.String())
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}}})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	defer func() { _ = conn.CloseNow() }()
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "authenticate", "token": token, "nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}))
	var reply struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "authenticated", reply.Type)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin", "request_id": "changed", "container_id": "changed-zip", "expected_hash": digest, "expected_size": len(expected)}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "ready", reply.Type)
	require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, actual))
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "changed"}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
	require.Equal(t, "error", reply.Type)
	require.Equal(t, "mailbox_conflict", reply.Code)
}
