package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestMailboxChunkUsesPinnedBrowserStream(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	raw := []byte("synthetic source")
	h := sha256.Sum256(raw)
	hash := hex.EncodeToString(h[:])
	_, err := s.BeginMailboxContainer(ctx, store.MailboxContainerRequest{ID: "browser-container", Owner: "vault:" + s.VaultID(), SHA256: hash, Size: int64(len(raw)), Format: "mbox"})
	require.NoError(t, err)
	conn := browserUploadSocket(t, ts)
	var message struct {
		Type  string              `json:"type"`
		Chunk *store.MailboxChunk `json:"chunk_receipt"`
	}
	require.NoError(t, wsjson.Write(ctx, conn, map[string]any{"type": "begin_mailbox_chunk", "request_id": "chunk", "container_id": "browser-container", "chunk_index": 0, "expected_hash": hash, "expected_size": len(raw)}))
	require.NoError(t, wsjson.Read(ctx, conn, &message))
	require.Equal(t, "ready", message.Type)
	require.NoError(t, conn.Write(ctx, websocket.MessageBinary, raw))
	require.NoError(t, wsjson.Write(ctx, conn, map[string]string{"type": "end", "request_id": "chunk"}))
	require.NoError(t, wsjson.Read(ctx, conn, &message))
	require.Equal(t, "mailbox_chunk_receipt", message.Type)
	require.NotNil(t, message.Chunk)
	require.Equal(t, hash, message.Chunk.SHA256)
}

func TestBrowserUploadUsesAuthenticatedPinnedChannel(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Gate = gate
	})
	destination, err := s.Mkdir(t.Context(), s.RootID(), "Reports")
	require.NoError(t, err)

	conn := browserUploadSocket(t, ts)

	var message struct {
		Type      string            `json:"type"`
		RequestID string            `json:"request_id"`
		Proof     string            `json:"proof"`
		Code      string            `json:"code"`
		Receipt   api.UploadReceipt `json:"receipt"`
	}
	content := []byte("quarterly")
	digest := sha256.Sum256(content)
	maintenanceHeld := make(chan struct{})
	releaseMaintenance := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- gate.MaintainContext(t.Context(), func() error {
			close(maintenanceHeld)
			<-releaseMaintenance
			return nil
		})
	}()
	<-maintenanceHeld
	t.Cleanup(func() {
		select {
		case <-releaseMaintenance:
		default:
			close(releaseMaintenance)
		}
	})
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type":          "begin",
		"request_id":    "busy-browser-test",
		"parent_id":     destination.ID,
		"name":          "quarterly.txt",
		"mime_type":     "text/plain",
		"expected_hash": hex.EncodeToString(digest[:]),
		"expected_size": len(content),
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "error", message.Type)
	require.Equal(t, "maintenance_busy", message.Code)
	close(releaseMaintenance)
	require.NoError(t, <-maintenanceDone)

	const requestID = "browser-test"
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type":          "begin",
		"request_id":    requestID,
		"parent_id":     destination.ID,
		"name":          "quarterly.txt",
		"mime_type":     "text/plain",
		"expected_hash": hex.EncodeToString(digest[:]),
		"expected_size": len(content),
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "ready", message.Type)
	require.Equal(t, requestID, message.RequestID)
	require.NoError(t, conn.Write(
		t.Context(), websocket.MessageBinary, content,
	))
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type": "end", "request_id": requestID,
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "receipt", message.Type)
	require.Equal(t, requestID, message.RequestID)
	assert.Equal(t, "added", message.Receipt.Status)
	require.NotNil(t, message.Receipt.Node.ParentID)
	assert.Equal(t, destination.ID, *message.Receipt.Node.ParentID)
	assert.Equal(t, "quarterly.txt", message.Receipt.Node.Name)
	reader, err := s.Blobs.OpenContext(t.Context(), message.Receipt.Node.BlobHash)
	require.NoError(t, err)
	stored, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, content, stored)
}

func TestServerShutdownDrainsActiveBrowserUpload(t *testing.T) {
	t.Parallel()
	gate := api.NewOperationGate()
	ts, s := newTestServer(t, func(d *api.Deps) {
		d.Gate = gate
	})
	destination, err := s.Mkdir(t.Context(), s.RootID(), "Reports")
	require.NoError(t, err)

	conn := browserUploadSocket(t, ts)
	var message struct {
		Type  string `json:"type"`
		Proof string `json:"proof"`
	}
	content := []byte("incomplete")
	digest := sha256.Sum256(content)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type":          "begin",
		"request_id":    "shutdown-test",
		"parent_id":     destination.ID,
		"name":          "incomplete.txt",
		"mime_type":     "text/plain",
		"expected_hash": hex.EncodeToString(digest[:]),
		"expected_size": len(content),
	}))
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	require.Equal(t, "ready", message.Type)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, s.Server.Shutdown(shutdownCtx))
	require.NoError(t, gate.MaintainContext(t.Context(), func() error { return nil }))
}

func browserUploadSocket(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	token := issueWebSession(t, ts)
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}}})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	conn.SetReadLimit((1 << 20) + 4096)
	t.Cleanup(func() { _ = conn.CloseNow() })
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "authenticate", "token": token, "nonce": "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"}))
	var auth map[string]any
	require.NoError(t, wsjson.Read(t.Context(), conn, &auth))
	require.Equal(t, "authenticated", auth["type"])
	require.NotEmpty(t, auth["proof"])
	return conn
}

type photoSocketResponse struct {
	ImportID    string         `json:"import_id"`
	Type        string         `json:"type"`
	Status      int            `json:"status"`
	Groups      [][]int        `json:"groups"`
	Unsupported []int          `json:"unsupported"`
	SkipReasons map[int]string `json:"skip_reasons"`
	Code        string         `json:"code"`
	Detail      string         `json:"detail"`
	Receipt     struct {
		Status  string `json:"status"`
		AssetID string `json:"asset_id"`
		Reason  string `json:"reason"`
	} `json:"photo_group_receipt"`
}

func readPhotoResponse(t *testing.T, conn *websocket.Conn) photoSocketResponse {
	t.Helper()
	var message photoSocketResponse
	require.NoError(t, wsjson.Read(t.Context(), conn, &message))
	return message
}

func TestBrowserPhotoPlan(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	names := []string{"Trip/IMG_0001.ARW", "Trip/IMG_0001.JPG", "Trip/IMG_0001.xmp", "Trip/IMG_0001.MP4", "Trip/notes.txt"}
	files := make([]map[string]any, len(names))
	for i, name := range names {
		files[i] = map[string]any{"relative_path": name, "size": 3}
		if name != "Trip/notes.txt" {
			files[i]["expected_hash"] = testHash(name)
		}
	}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "plan", "files": files}))
	message := readPhotoResponse(t, conn)
	require.Equal(t, "photo_import_plan", message.Type)
	assert.Equal(t, [][]int{{0, 1, 2}, {3}}, message.Groups)
	assert.Equal(t, []int{4}, message.Unsupported)
	assert.Equal(t, "Unsupported type", message.SkipReasons[4])
	for _, name := range []string{"/absolute.JPG", "../capture.JPG", "Trip/../capture.JPG", `Trip\capture.JPG`, "Trip//capture.JPG", "Trip/./capture.JPG", ".", "Trip/", ""} {
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "odd", "files": []map[string]any{{"relative_path": "valid.JPG", "expected_hash": testHash("synthetic photo"), "size": 3}, {"relative_path": name, "expected_hash": testHash(name), "size": 3}}}))
		message := readPhotoResponse(t, conn)
		require.Equal(t, "photo_import_plan", message.Type, name)
		assert.Equal(t, [][]int{{0}}, message.Groups, name)
		assert.Equal(t, []int{1}, message.Unsupported, name)
		assert.Equal(t, "Invalid name", message.SkipReasons[1], name)
	}
	for _, name := range []string{"Trip:2026/capture.JPG", "odd:name.JPG", strings.Repeat("folder/", 200) + "capture.JPG", strings.Repeat("a", 1025) + ".JPG", "Cafe\u0301/capture.JPG"} {
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "valid", "files": []map[string]any{{"relative_path": name, "expected_hash": testHash(name), "size": 3}}}))
		message := readPhotoResponse(t, conn)
		require.Equal(t, "photo_import_plan", message.Type, name)
		assert.Equal(t, [][]int{{0}}, message.Groups, name)
		assert.Empty(t, message.Unsupported, name)
	}
	for _, size := range []int64{-1, 1 << 50} {
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "size", "files": []map[string]any{{"relative_path": "a.JPG", "expected_hash": testHash("synthetic photo"), "size": size}}}))
		message := readPhotoResponse(t, conn)
		assert.Equal(t, []int{0}, message.Unsupported)
		assert.Equal(t, "Invalid size", message.SkipReasons[0])
	}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "duplicates", "files": []map[string]any{{"relative_path": "a.JPG", "expected_hash": testHash("synthetic photo"), "size": 3}, {"relative_path": "a.JPG", "expected_hash": testHash("synthetic photo"), "size": 3}}}))
	message = readPhotoResponse(t, conn)
	assert.Equal(t, []int{1}, message.Unsupported)
	assert.Equal(t, "Duplicate path", message.SkipReasons[1])
	for _, hash := range []string{"", strings.Repeat("A", 64), "invalid"} {
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "request_id": "invalid-hash", "destination": "/photos", "files": []map[string]any{{"relative_path": "photo.JPG", "size": 3, "expected_hash": hash}}}))
		assert.Equal(t, http.StatusUnprocessableEntity, readPhotoResponse(t, conn).Status)
	}
	card := make([]map[string]any, 16384)
	for i := range card {
		card[i] = map[string]any{"relative_path": fmt.Sprintf("DCIM/%sIMG_%05d.JPG", strings.Repeat("long-folder/", 30), i), "expected_hash": testHash("synthetic photo"), "size": 25000000}
	}
	cardFields := map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "card", "files": card}
	encoded, err := json.Marshal(cardFields)
	require.NoError(t, err)
	require.Greater(t, len(encoded), 7<<20)
	require.Less(t, len(encoded), 8<<20)
	require.NoError(t, wsjson.Write(t.Context(), conn, cardFields))
	assert.Len(t, readPhotoResponse(t, conn).Groups, len(card))
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "too-many", "files": make([]map[string]any, 32769)}))
	assert.Equal(t, http.StatusUnprocessableEntity, readPhotoResponse(t, conn).Status)
	for _, name := range []string{"only.JPG", "only.txt"} {
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "single", "files": []map[string]any{{"relative_path": name, "expected_hash": testHash(name), "size": 3}}}))
		var reply map[string]any
		require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
		require.Equal(t, "photo_import_plan", reply["type"])
		require.IsType(t, []any{}, reply["groups"])
		require.IsType(t, []any{}, reply["unsupported"])
		require.NotEmpty(t, reply["import_id"])
	}
}

func photoGroupDeclaration(names []string, wrongHash bool) []map[string]any {
	members := make([]map[string]any, len(names))
	for i, name := range names {
		digest := sha256.Sum256([]byte(name))
		hash := hex.EncodeToString(digest[:])
		if wrongHash && i == 1 {
			hash = strings.Repeat("0", 64)
		}
		members[i] = map[string]any{"relative_path": name, "expected_hash": hash, "expected_size": len(name)}
	}
	return members
}

func TestBrowserPhotoGroupIngestMemberLimit(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	names := make([]string, store.PhotoMaxFiles+1)
	files := make([]map[string]any, len(names))
	names[0], names[1] = "capture.ARW", "capture.DNG"
	for i := range names {
		if i >= 2 {
			variant := i - 2
			extension := []byte([]string{"arw", "dng", "raf", "nef"}[variant/64] + "xmp")
			for bit := range extension {
				if variant&(1<<bit) != 0 {
					extension[bit] -= 'a' - 'A'
				}
			}
			names[i] = "capture." + string(extension[:3]) + "." + string(extension[3:])
		}
		files[i] = map[string]any{"relative_path": names[i], "expected_hash": testHash(names[i]), "size": len(names[i])}
	}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "plan", "files": files}))
	plan := readPhotoResponse(t, conn)
	require.Equal(t, "photo_import_plan", plan.Type)
	require.Empty(t, plan.Unsupported)
	require.Len(t, plan.Groups, 1)
	require.Len(t, plan.Groups[0], len(names))
	members := photoGroupDeclaration(names, false)
	hashes := make([]string, len(names))
	for i, index := range plan.Groups[0] {
		var ok bool
		hashes[i], ok = members[index]["expected_hash"].(string)
		require.True(t, ok)
	}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "group", "import_id": plan.ImportID, "group": 0, "hashes": hashes}))
	for _, index := range plan.Groups[0] {
		require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
		require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, []byte(names[index])))
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "end", "request_id": "group"}))
	}
	reply := readPhotoResponse(t, conn)
	require.Equal(t, "photo_group_receipt", reply.Type)
	require.Equal(t, "ambiguous", reply.Receipt.Status)
	assert.Equal(t, store.PhotoImportMultipleRAW, reply.Receipt.Reason)
	assets := make(map[string]bool)
	for _, name := range names {
		node, err := catalog.NodeByPath(t.Context(), "/photos/"+name)
		require.NoError(t, err)
		asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
		if strings.EqualFold(name[len(name)-4:], ".xmp") {
			require.ErrorIs(t, err, store.ErrNotFound)
			continue
		}
		require.NoError(t, err)
		require.Len(t, asset.Files, 1)
		assets[asset.ID] = true
	}
	assert.Len(t, assets, 2)
}

func TestBrowserUploadControlMessageLimit(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"begin", "begin_mailbox_chunk", "begin_photo_group"} {
		t.Run(kind, func(t *testing.T) {
			ts, _ := newTestServer(t, nil)
			conn := browserUploadSocket(t, ts)
			var importID string
			if kind == "begin_photo_group" {
				importID = planBrowserPhotos(t, conn, photoGroupDeclaration([]string{"capture.ARW"}, false))
			}
			limit := 4096
			if kind == "begin_photo_group" {
				limit = 4096 + 67*32768
			}
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": kind, "request_id": "large", "import_id": importID, "name": strings.Repeat("a", limit+1), "files": "invalid file list"}))
			_, _, err := conn.Read(t.Context())
			require.Error(t, err)
			assert.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
		})
	}
	t.Run("type after payload", func(t *testing.T) {
		ts, _ := newTestServer(t, nil)
		conn := browserUploadSocket(t, ts)
		data := []byte(`{"files": [` + strings.Repeat(" ", 5000) + `], "type" : "begin"}`)
		require.NoError(t, conn.Write(t.Context(), websocket.MessageText, data))
		_, _, err := conn.Read(t.Context())
		require.Error(t, err)
		assert.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
	})
	t.Run("duplicate type closes", func(t *testing.T) {
		ts, _ := newTestServer(t, nil)
		conn := browserUploadSocket(t, ts)
		data := []byte(`{"type":"plan_photo_import","request_id":"plan","destination":"/",` +
			`"files":[{"relative_path":"a.JPG","size":3}],"type":"begin"}`)
		require.NoError(t, conn.Write(t.Context(), websocket.MessageText, data))
		_, _, err := conn.Read(t.Context())
		require.Error(t, err)
	})
}

func planBrowserPhotos(t *testing.T, conn *websocket.Conn, members []map[string]any) string {
	t.Helper()
	files := make([]map[string]any, len(members))
	for i, member := range members {
		files[i] = map[string]any{"relative_path": member["relative_path"], "expected_hash": member["expected_hash"], "size": member["expected_size"]}
	}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "plan", "files": files}))
	reply := readPhotoResponse(t, conn)
	require.Equal(t, "photo_import_plan", reply.Type)
	require.NotEmpty(t, reply.ImportID)
	return reply.ImportID
}

func TestBrowserPhotoGroup(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	names := []string{"Trip/capture.ARW", "Trip/capture.JPG", "Trip/capture.xmp"}
	var assetID string
	for _, status := range []string{"added", "skipped"} {
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "group", "destination": "/photos", "import_id": planBrowserPhotos(t, conn, photoGroupDeclaration(names, false)), "group": 0, "hashes": photoGroupHashes(t, photoGroupDeclaration(names, false))}))
		for _, name := range names {
			require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
			require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, []byte(name)))
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "group"}))
		}
		message := readPhotoResponse(t, conn)
		require.Equal(t, "photo_group_receipt", message.Type)
		assert.Equal(t, status, message.Receipt.Status)
		if assetID == "" {
			assetID = message.Receipt.AssetID
		}
		assert.Equal(t, assetID, message.Receipt.AssetID)
	}
	asset, err := catalog.PhotoAssetByID(t.Context(), assetID)
	require.NoError(t, err)
	roles := []string{}
	for _, file := range asset.Files {
		roles = append(roles, file.Role)
	}
	assert.ElementsMatch(t, []string{"raw", "image", "sidecar"}, roles)
	names = []string{"a.JPG", "b.ARW"}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "mixed", "import_id": planBrowserPhotos(t, conn, photoGroupDeclaration(names, false)), "group": 0, "hashes": photoGroupHashes(t, photoGroupDeclaration(names, false))}))
	assert.Equal(t, http.StatusUnprocessableEntity, readPhotoResponse(t, conn).Status)
}

func TestBrowserPhotoGroupRejectsMemberWithoutAuthority(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"hash", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ts, catalog := newTestServer(t, nil)
			conn := browserUploadSocket(t, ts)
			names := []string{"capture.ARW", "capture.JPG"}
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "group", "destination": "/photos", "import_id": planBrowserPhotos(t, conn, photoGroupDeclaration(names, failure == "hash")), "group": 0, "hashes": photoGroupHashes(t, photoGroupDeclaration(names, failure == "hash"))}))
			require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
			require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, []byte(names[0])))
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "group"}))
			require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
			if failure == "cancel" {
				require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "cancel", "request_id": "group"}))
			} else {
				require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, []byte(names[1])))
				require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "group"}))
			}
			message := readPhotoResponse(t, conn)
			assert.Equal(t, "error", message.Type)
			children, err := catalog.Children(t.Context(), catalog.RootID())
			require.NoError(t, err)
			assert.Empty(t, children)
			response, body := do(t, ts, http.MethodPost, "/api/v1/photos/assets/query", nil, api.PhotoBrowseRequest{Query: api.QueryPayload(`{}`)})
			require.Equal(t, http.StatusOK, response.StatusCode, body)
			var page struct {
				Total int `json:"total"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &page))
			assert.Zero(t, page.Total)
		})
	}
}

func TestBrowserPhotoImportsScopeSourcesAndReuseRun(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	first := mediatest.JPEG(2, 2, color.RGBA{R: 255, A: 255})
	second := mediatest.JPEG(2, 2, color.RGBA{B: 255, A: 255})
	declaration := func(name string, data []byte) map[string]any {
		hash := sha256.Sum256(data)
		return map[string]any{"relative_path": name, "expected_hash": hex.EncodeToString(hash[:]), "expected_size": len(data)}
	}
	members := []map[string]any{declaration("Card/IMG_0001.JPG", first), declaration("Card/IMG_0002.JPG", second)}
	importID := planBrowserPhotos(t, conn, members)
	a := uploadBrowserPhoto(t, conn, importID, 0, members[0], first)
	b := uploadBrowserPhoto(t, conn, importID, 1, members[1], second)
	require.Equal(t, "photo_group_receipt", a.Type)
	require.Equal(t, "photo_group_receipt", b.Type)
	require.Equal(t, "added", a.Receipt.Status)
	require.Equal(t, "added", b.Receipt.Status)
	collections, total, err := catalog.Collections(t.Context(), 100, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	assert.Equal(t, int64(2), collections[0].FileCount)
	assert.Equal(t, "browser: Card", collections[0].SourceDescription)
	changedData := mediatest.JPEG(2, 2, color.RGBA{G: 255, A: 255})
	changed := declaration("Card/IMG_0001.JPG", changedData)
	nextID := planBrowserPhotos(t, conn, []map[string]any{changed})
	require.NotEqual(t, importID, nextID)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "old-import", "import_id": importID, "destination": "/photos", "group": 0, "hashes": photoGroupHashes(t, []map[string]any{changed})}))
	assert.Equal(t, http.StatusUnprocessableEntity, readPhotoResponse(t, conn).Status)
	c := uploadBrowserPhoto(t, conn, nextID, 0, changed, changedData)
	require.Equal(t, "added", c.Receipt.Status)
}

func TestBrowserPhotoImportRecordsOriginalMtime(t *testing.T) {
	t.Parallel()
	epoch := time.Unix(0, 0).UTC()
	for _, tc := range []struct {
		name     string
		modified *time.Time
	}{
		{"ordinary time", new(time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.UTC))},
		{"at the epoch", new(epoch)},
		{"before the epoch", new(epoch.Add(-24 * time.Hour))},
		{"omitted", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts, catalog := newTestServer(t, nil)
			conn := browserUploadSocket(t, ts)
			data := mediatest.JPEG(2, 2, color.RGBA{R: 255, A: 255})
			hash := sha256.Sum256(data)
			file := map[string]any{"relative_path": "Card/IMG_0001.JPG", "expected_hash": hex.EncodeToString(hash[:]), "size": len(data)}
			if tc.modified != nil {
				file["modified_time"] = tc.modified.UnixMilli()
			}
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "plan", "files": []map[string]any{file}}))
			plan := readPhotoResponse(t, conn)
			require.Equal(t, "photo_import_plan", plan.Type)
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "group", "import_id": plan.ImportID, "destination": "/photos", "group": 0,
				"hashes": []string{hex.EncodeToString(hash[:])}}))
			require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
			require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, data))
			require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "group"}))
			reply := readPhotoResponse(t, conn)
			require.Equal(t, "added", reply.Receipt.Status)
			asset, err := catalog.PhotoAssetByID(t.Context(), reply.Receipt.AssetID)
			require.NoError(t, err)
			require.Len(t, asset.Files, 1)
			resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/provenance?limit=1&offset=0", asset.Files[0].NodeID), nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var page api.ProvenancePage
			require.NoError(t, json.Unmarshal([]byte(body), &page))
			require.Len(t, page.Items, 1)
			if tc.modified == nil {
				assert.Nil(t, page.Items[0].OriginalMTime)
				return
			}
			require.NotNil(t, page.Items[0].OriginalMTime)
			assert.Equal(t, tc.modified.Format(time.RFC3339Nano), *page.Items[0].OriginalMTime)
		})
	}
}

func TestBrowserPhotoAmbiguityReceipt(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	names := []string{"capture.ARW", "capture.CR2"}
	members := photoGroupDeclaration(names, false)
	importID := planBrowserPhotos(t, conn, members)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "ambiguous", "import_id": importID, "destination": "/photos", "group": 0, "hashes": photoGroupHashes(t, members)}))
	for _, name := range names {
		require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
		require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, []byte(name)))
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "ambiguous"}))
	}
	reply := readPhotoResponse(t, conn)
	require.Equal(t, "photo_group_receipt", reply.Type)
	assert.Equal(t, "ambiguous", reply.Receipt.Status)
	assert.Equal(t, store.PhotoImportMultipleRAW, reply.Receipt.Reason)
}

func photoGroupHashes(t *testing.T, members []map[string]any) []string {
	t.Helper()
	hashes := make([]string, len(members))
	for i, member := range members {
		var ok bool
		hashes[i], ok = member["expected_hash"].(string)
		require.True(t, ok)
	}
	return hashes
}

func TestBrowserPhotoPlanDestination(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	for _, tc := range []struct{ destination, code string }{{"relative", "validation"}, {"/missing/.", "invalid_name"}} {
		importID := planBrowserPhotos(t, conn, photoGroupDeclaration([]string{"capture.JPG"}, false))
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "request_id": "plan", "destination": tc.destination, "files": []map[string]any{{"relative_path": "capture.JPG", "expected_hash": testHash("synthetic photo"), "size": 3}}}))
		var reply struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		}
		require.NoError(t, wsjson.Read(t.Context(), conn, &reply))
		assert.Equal(t, 422, reply.Status)
		assert.Equal(t, tc.code, reply.Code)
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "stale", "import_id": importID, "group": 0, "hashes": photoGroupHashes(t, photoGroupDeclaration([]string{"capture.JPG"}, false))}))
		stale := readPhotoResponse(t, conn)
		assert.Equal(t, "error", stale.Type)
		assert.Equal(t, "validation", stale.Code)
	}
}

func TestBrowserPhotoGroupFreezesHashes(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	members := photoGroupDeclaration([]string{"capture.JPG"}, false)
	importID := planBrowserPhotos(t, conn, members)
	begin := map[string]any{"type": "begin_photo_group", "request_id": "group", "import_id": importID, "group": 0, "hashes": photoGroupHashes(t, members)}
	for _, invalid := range []any{[]string{}, []string{"INVALID"}} {
		begin["hashes"] = invalid
		require.NoError(t, wsjson.Write(t.Context(), conn, begin))
		assert.Equal(t, 422, readPhotoResponse(t, conn).Status)
	}
	begin["hashes"] = photoGroupHashes(t, members)
	require.NoError(t, wsjson.Write(t.Context(), conn, begin))
	require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "cancel", "request_id": "group"}))
	assert.Equal(t, 499, readPhotoResponse(t, conn).Status)
	begin["hashes"] = []string{strings.Repeat("0", 64)}
	require.NoError(t, wsjson.Write(t.Context(), conn, begin))
	reply := readPhotoResponse(t, conn)
	assert.Equal(t, 422, reply.Status)
	assert.Contains(t, reply.Detail, "Choose the folder again")
	begin["hashes"] = photoGroupHashes(t, members)
	require.NoError(t, wsjson.Write(t.Context(), conn, begin))
	require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "cancel", "request_id": "group"}))
	assert.Equal(t, 499, readPhotoResponse(t, conn).Status)
}

func TestBrowserPhotoDestinationIdentity(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"rename", "trash", "remove", "rename after plan"} {
		t.Run(change, func(t *testing.T) {
			ts, catalog := newTestServer(t, nil)
			if change == "rename after plan" {
				_, err := catalog.EnsurePhotoImportDestination(t.Context(), "/photos")
				require.NoError(t, err)
			}
			conn := browserUploadSocket(t, ts)
			members := photoGroupDeclaration([]string{"first.ARW", "second.ARW"}, false)
			importID := planBrowserPhotos(t, conn, members)
			if change != "rename after plan" {
				_, err := catalog.NodeByPath(t.Context(), "/photos")
				require.ErrorIs(t, err, store.ErrNotFound)
				uploadBrowserPhoto(t, conn, importID, 0, members[0], []byte("first.ARW"))
			}
			dest, err := catalog.NodeByPath(t.Context(), "/photos")
			require.NoError(t, err)
			if change == "rename" || change == "rename after plan" {
				_, _, err = catalog.Move(t.Context(), dest.ID, catalog.RootID(), "Trips", dest.Revision)
			} else {
				_, _, err = catalog.Trash(t.Context(), dest.ID, dest.Revision)
				if change == "remove" {
					require.NoError(t, err)
					_, err = catalog.TrashEmpty(t.Context(), 0, true)
				}
			}
			require.NoError(t, err)
			reply := uploadBrowserPhoto(t, conn, importID, 1, members[1], []byte("second.ARW"))
			if change == "rename" || change == "rename after plan" {
				require.Equal(t, "photo_group_receipt", reply.Type)
				children, err := catalog.Children(t.Context(), dest.ID)
				require.NoError(t, err)
				if change == "rename after plan" {
					require.Len(t, children, 1)
				} else {
					require.Len(t, children, 2)
				}
			} else {
				assert.Equal(t, "not_found", reply.Code)
				assert.Contains(t, reply.Detail, "trashed or removed")
			}
			_, err = catalog.NodeByPath(t.Context(), "/photos")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func uploadBrowserPhoto(t *testing.T, conn *websocket.Conn, importID string, index int, member map[string]any, data []byte) photoSocketResponse {
	t.Helper()
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "begin_photo_group", "request_id": "group", "import_id": importID, "group": index, "hashes": photoGroupHashes(t, []map[string]any{member})}))
	require.Equal(t, "ready", readPhotoResponse(t, conn).Type)
	require.NoError(t, conn.Write(t.Context(), websocket.MessageBinary, data))
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "end", "request_id": "group"}))
	return readPhotoResponse(t, conn)
}

func TestBrowserPhotoPlanRejectsAudit(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	plan, err := catalog.PreviewInitialAudit(t.Context(), catalog.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = catalog.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	conn := browserUploadSocket(t, ts)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "request_id": "plan", "destination": "/photos", "files": []map[string]any{{"relative_path": "capture.ARW", "expected_hash": testHash("synthetic photo"), "size": 3}}}))
	reply := readPhotoResponse(t, conn)
	assert.Equal(t, "error", reply.Type)
	assert.Equal(t, "audit_mutation_unsupported", reply.Code)
	children, err := catalog.Children(t.Context(), catalog.RootID())
	require.NoError(t, err)
	assert.Empty(t, children)
}

func TestBrowserPhotoLoneSidecarSelections(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	member := photoGroupDeclaration([]string{"capture.xmp"}, false)[0]
	for range 2 {
		importID := planBrowserPhotos(t, conn, []map[string]any{member})
		reply := uploadBrowserPhoto(t, conn, importID, 0, member, []byte("capture.xmp"))
		require.Equal(t, "photo_group_receipt", reply.Type)
		assert.Equal(t, "added", reply.Receipt.Status)
		assert.Empty(t, reply.Receipt.AssetID)
	}
	dest, err := catalog.NodeByPath(t.Context(), "/photos")
	require.NoError(t, err)
	children, err := catalog.Children(t.Context(), dest.ID)
	require.NoError(t, err)
	assert.Len(t, children, 2)
	assert.Equal(t, children[0].BlobHash, children[1].BlobHash)
}

func TestBrowserUploadBoundsAuthentication(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	conn, response, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/daemon/web-upload", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {strings.TrimSuffix(testWebURL, "/")}}})
	require.NoError(t, err)
	if response != nil && response.Body != nil {
		require.NoError(t, response.Body.Close())
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]string{"type": "authenticate", "token": strings.Repeat("a", 8192)}))
	_, _, err = conn.Read(t.Context())
	require.Error(t, err)
	assert.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
}

func TestBrowserPhotoFolderContinuationKeepsImportedSource(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	data := [][]byte{mediatest.JPEG(2, 2, color.RGBA{R: 255, A: 255}), mediatest.JPEG(2, 2, color.RGBA{G: 255, A: 255})}
	files := []map[string]any{{"relative_path": "folder/IMG_001.jpg", "size": len(data[0])}, {"relative_path": "folder/IMG_002.jpg", "size": len(data[1])}}
	for i := range files {
		hash := sha256.Sum256(data[i])
		files[i]["expected_hash"] = hex.EncodeToString(hash[:])
	}
	sources := []string{"20000000-0000-4000-8000-000000000001", "20000000-0000-4000-8000-000000000002"}
	var ids []string
	for selection, source := range sources {
		selected := files[:selection+1]
		if selection == 0 {
			selected = []map[string]any{{"relative_path": "preview.JPG", "size": len(data[0]), "expected_hash": files[0]["expected_hash"]}}
		}
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "plan", "source_id": source, "files": selected}))
		plan := readPhotoResponse(t, conn)
		require.Equal(t, "photo_import_plan", plan.Type)
		for i, file := range selected {
			hash := sha256.Sum256(data[i])
			member := map[string]any{"relative_path": file["relative_path"], "expected_hash": hex.EncodeToString(hash[:]), "expected_size": len(data[i])}
			reply := uploadBrowserPhoto(t, conn, plan.ImportID, i, member, data[i])
			require.Equal(t, "photo_group_receipt", reply.Type)
			if selection == 0 {
				require.Equal(t, "added", reply.Receipt.Status)
				ids = append(ids, reply.Receipt.AssetID)
			} else if i == 0 {
				require.Equal(t, "skipped", reply.Receipt.Status)
				require.Equal(t, ids[0], reply.Receipt.AssetID)
			} else {
				require.Equal(t, "added", reply.Receipt.Status)
				ids = append(ids, reply.Receipt.AssetID)
			}
			asset, err := catalog.PhotoAssetByID(t.Context(), reply.Receipt.AssetID)
			require.NoError(t, err)
			require.Len(t, asset.Files, 1)
			resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/provenance?limit=1&offset=0", asset.Files[0].NodeID), nil)
			require.Equal(t, http.StatusOK, resp.StatusCode, body)
			var provenance api.ProvenancePage
			require.NoError(t, json.Unmarshal([]byte(body), &provenance))
			require.Len(t, provenance.Items, 1)
			expectedPath := "browser:" + source + "/" + fmt.Sprint(file["relative_path"])
			if selection == 1 && i == 0 {
				expectedPath = "browser:" + sources[0] + "/IMG_001.jpg"
			} else if selection == 1 {
				expectedPath = "browser:" + sources[0] + "/IMG_002.jpg"
			}
			assert.Equal(t, expectedPath, provenance.Items[0].OriginalPath)
		}
	}
}

func TestBrowserPhotoPlanSkipsLeadingUnsupportedFilesWithoutHashes(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	selected := []struct {
		name string
		data []byte
	}{
		{"Selected/IMG_001.bak", []byte("unsupported copy one")},
		{"Selected/IMG_002.bak", []byte("unsupported copy two")},
		{"Selected/IMG_003.jpg", mediatest.JPEG(2, 2, color.RGBA{B: 255, A: 255})},
	}
	files := make([]map[string]any, len(selected))
	for i, file := range selected {
		files[i] = map[string]any{"relative_path": file.name, "size": len(file.data)}
		if strings.HasSuffix(file.name, ".jpg") {
			digest := sha256.Sum256(file.data)
			files[i]["expected_hash"] = hex.EncodeToString(digest[:])
		}
	}
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type": "plan_photo_import", "request_id": "plan", "destination": "/photos",
		"source_id": "20000000-0000-4000-8000-000000000013", "files": files,
	}))
	plan := readPhotoResponse(t, conn)
	require.Equal(t, "photo_import_plan", plan.Type, "%s: %s", plan.Code, plan.Detail)
	require.Equal(t, []int{0, 1}, plan.Unsupported)
	require.Equal(t, [][]int{{2}}, plan.Groups)
	receipt := uploadBrowserPhoto(t, conn, plan.ImportID, 0, files[2], selected[2].data)
	require.Equal(t, "photo_group_receipt", receipt.Type)
	require.Equal(t, "added", receipt.Receipt.Status)
	require.NotEmpty(t, receipt.Receipt.AssetID)
}

func TestBrowserPhotoResumeRetainsSource(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	const source = "20000000-0000-4000-8000-000000000001"
	for i, shade := range []color.RGBA{{R: 255, A: 255}, {G: 255, A: 255}} {
		if i == 1 {
			require.NoError(t, conn.Close(websocket.StatusNormalClosure, "interrupted"))
			conn = browserUploadSocket(t, ts)
		}
		data := mediatest.JPEG(2, 2, shade)
		name := fmt.Sprintf("Card/IMG_%04d.JPG", i)
		hash := sha256.Sum256(data)
		member := map[string]any{"relative_path": name, "expected_hash": hex.EncodeToString(hash[:]), "expected_size": len(data)}
		require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{"type": "plan_photo_import", "destination": "/photos", "request_id": "resume", "source_id": source, "files": []map[string]any{{"relative_path": name, "expected_hash": hex.EncodeToString(hash[:]), "size": len(data)}}}))
		plan := readPhotoResponse(t, conn)
		require.Equal(t, "photo_import_plan", plan.Type)
		reply := uploadBrowserPhoto(t, conn, plan.ImportID, 0, member, data)
		require.Equal(t, "added", reply.Receipt.Status)
		asset, err := catalog.PhotoAssetByID(t.Context(), reply.Receipt.AssetID)
		require.NoError(t, err)
		require.Len(t, asset.Files, 1)
		resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/provenance?limit=1&offset=0", asset.Files[0].NodeID), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, body)
		var page api.ProvenancePage
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Items, 1)
		require.Equal(t, "browser:"+source+"/"+name, page.Items[0].OriginalPath)
	}
}

func TestBrowserPhotoPlanRejectsZeroSource(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServer(t, nil)
	conn := browserUploadSocket(t, ts)
	require.NoError(t, wsjson.Write(t.Context(), conn, map[string]any{
		"type": "plan_photo_import", "destination": "/photos", "request_id": "zero-source",
		"source_id": "00000000-0000-0000-0000-000000000000",
		"files":     []map[string]any{{"relative_path": "capture.JPG", "expected_hash": testHash("synthetic photo"), "size": 3}},
	}))
	reply := readPhotoResponse(t, conn)
	require.Equal(t, "error", reply.Type)
	require.Equal(t, http.StatusUnprocessableEntity, reply.Status)
	require.Equal(t, "validation", reply.Code)
}
