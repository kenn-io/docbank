package api_test

import (
	"bytes"
	"encoding/json/v2"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func notePublicationJSON(t *testing.T, s *testStore, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	var err error
	if value != nil {
		body, err = json.Marshal(value)
		require.NoError(t, err)
	}
	return serveLoopback(t.Context(), s.Server.Handler(), method, path,
		map[string]string{"X-Api-Key": testAPIKey, "Content-Type": "application/json"}, bytes.NewReader(body))
}

func notePublicationUpload(t *testing.T, s *testStore, scan string, revision int64, operation, content, hash string, extra bool, overrides ...url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", "alpha.md")
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	if extra {
		require.NoError(t, w.WriteField("extra", "rejected"))
	}
	require.NoError(t, w.Close())
	query := url.Values{"scan_id": {scan}, "source_ref": {"alpha.md"}, "name": {"alpha.md"},
		"parent_path": {"/notes"}, "operation_id": {operation}, "expected_source_revision": {strconv.FormatInt(revision, 10)}}
	for _, override := range overrides {
		maps.Copy(query, override)
	}
	return serveLoopback(t.Context(), s.Server.Handler(), http.MethodPost,
		"/api/v1/note-publications/research/uploads?"+query.Encode(), map[string]string{
			"X-Api-Key": testAPIKey, "Content-Type": w.FormDataContentType(),
			api.BlobHashHeader: hash, api.BlobSizeHeader: strconv.Itoa(len(content)),
		}, &body)
}

func TestNotePublicationRoutesRejectBadPlacement(t *testing.T) {
	t.Parallel()
	s, _ := newTestAPIServer(t, nil)
	scan, err := s.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
	response := notePublicationUpload(t, s, scan.ScanID, 0, uuid.NewV4().String(), "# Alpha\n", uploadIdentity([]byte("# Alpha\n")), false, url.Values{"parent_path": {"/elsewhere"}})
	require.Equal(t, 422, response.Code, response.Body.String())
	var problem api.Error
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	require.Equal(t, "validation", problem.Code)
	_, err = s.NodeByPath(t.Context(), "/elsewhere/alpha.md")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// Missing the managed route, digest check, envelope check, CAS or scan fence
// must fail this test through HTTP and the resulting durable source binding.
func TestNotePublicationRoutesVerifyAndFence(t *testing.T) {
	t.Parallel()
	s, _ := newTestAPIServer(t, nil)
	base := "/api/v1/note-publications/research"
	policy := strings.Repeat("a", 64)
	begin := func() api.NotePublicationScan {
		response := notePublicationJSON(t, s, http.MethodPost, base+"/scans", map[string]any{
			"destination": "/notes", "policy_fingerprint": policy, "operation_id": uuid.NewV4().String(),
		})
		require.Equal(t, 200, response.Code, response.Body.String())
		var result api.NotePublicationScan
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.NotEmpty(t, result.DatabaseEpoch)
		return result
	}
	scan := begin()
	content := "# Alpha\n"
	hash := uploadIdentity([]byte(content))
	for _, bad := range []struct {
		hash  string
		extra bool
	}{{strings.Repeat("0", 64), false}, {hash, true}} {
		response := notePublicationUpload(t, s, scan.ScanID, 0, uuid.NewV4().String(), content, bad.hash, bad.extra)
		require.Equal(t, 422, response.Code, response.Body.String())
		page, err := s.NotePublicationSources(t.Context(), store.NotePublicationSourcesRequest{Name: "research", ScanID: scan.ScanID, Limit: 1000})
		require.NoError(t, err)
		require.Empty(t, page.Items, "rejected bytes must grant no source authority")
		_, err = s.NodeByPath(t.Context(), "/notes/alpha.md")
		require.Error(t, err, "rejected bytes must create no archive node")
	}
	op := uuid.NewV4().String()
	response := notePublicationUpload(t, s, scan.ScanID, 0, op, content, hash, false)
	require.Equal(t, 201, response.Code, response.Body.String())
	var receipt api.NotePublicationReceipt
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &receipt))
	require.Equal(t, hash, receipt.Source.Identity.Hash)
	require.Equal(t, int64(1), receipt.Source.Identity.SourceRevision)
	checkProblem := func(response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, response.Code, response.Body.String())
		var problem api.Error
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
		require.Equal(t, code, problem.Code)
	}
	checkProblem(notePublicationUpload(t, s, scan.ScanID, 0, uuid.NewV4().String(), content, hash, false), 409, "publication_source_stale")
	checkProblem(notePublicationUpload(t, s, scan.ScanID, 0, op, "different", uploadIdentity([]byte("different")), false), 409, "publication_operation_mismatch")
	seenPath := base + "/scans/" + scan.ScanID + "/seen"
	response = notePublicationJSON(t, s, http.MethodPost, seenPath, map[string]any{"bindings": []api.PublishedNoteIdentity{receipt.Source.Identity}})
	require.Equal(t, 200, response.Code, response.Body.String())
	complete := map[string]any{"policy_fingerprint": policy, "inventory_complete": true}
	response = notePublicationJSON(t, s, http.MethodPost, base+"/scans/"+scan.ScanID+"/complete", complete)
	require.Equal(t, 200, response.Code, response.Body.String())
	checkProblem(notePublicationJSON(t, s, http.MethodPost, seenPath, map[string]any{"bindings": []api.PublishedNoteIdentity{receipt.Source.Identity}}), 409, "publication_scan_sealed")
	response = notePublicationUpload(t, s, scan.ScanID, 0, op, content, hash, false)
	require.Equal(t, 201, response.Code, response.Body.String())
	var replay api.NotePublicationReceipt
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &replay))
	require.Equal(t, receipt, replay)
	newScan := begin()
	require.NotEqual(t, scan.ScanID, newScan.ScanID)
	checkProblem(notePublicationUpload(t, s, scan.ScanID, 1, uuid.NewV4().String(), content, hash, false), 409, "publication_scan_superseded")
	response = serveLoopback(t.Context(), s.Server.Handler(), http.MethodPost, base+"/scans/"+newScan.ScanID+"/seen",
		map[string]string{"X-Api-Key": testAPIKey, "Content-Type": "application/json"}, strings.NewReader(`{"bindings":[],"padding":"`+strings.Repeat("x", 1<<20)+`"}`))
	require.Equal(t, 413, response.Code, response.Body.String())
	response = notePublicationJSON(t, s, http.MethodGet, base+"/sources?scan_id="+newScan.ScanID, nil)
	require.Equal(t, 200, response.Code, response.Body.String())
	var page api.NotePublicationSourcesPage
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Equal(t, receipt.Source.Identity.ContentVersionID, page.Items[0].Identity.ContentVersionID)
}

func TestNotePublicationFirstAdoptionConflict(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"trashed_push", "occupied_name", "blocked_parent"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := newTestAPIServer(t, nil)
			body := "# Alpha\n"
			hash := uploadIdentity([]byte(body))
			switch kind {
			case "trashed_push":
				node, _, err := s.AcceptPush(t.Context(), store.PushSource{Name: "research", Ref: "alpha.md"}, store.PushContent{RunID: uuid.NewV4().String(), ParentPath: "/notes", Name: "alpha.md", Hash: hash, Size: int64(len(body)), MIMEType: "text/markdown"}, store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(body)), PackEligible: true, Created: true})
				require.NoError(t, err)
				_, _, err = s.Trash(t.Context(), node.ID, node.Revision)
				require.NoError(t, err)
			case "occupied_name":
				parent, err := s.MkdirAll(t.Context(), "/notes")
				require.NoError(t, err)
				_, err = s.CreateFile(t.Context(), parent.ID, "alpha.md", hash, int64(len(body)), "text/markdown", store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(body)), PackEligible: true, Created: true})
				require.NoError(t, err)
			case "blocked_parent":
				_, err := s.CreateFile(t.Context(), s.RootID(), "notes", hash, int64(len(body)), "text/markdown", store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(body)), PackEligible: true, Created: true})
				require.NoError(t, err)
			}
			scan, err := s.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String()})
			require.NoError(t, err)
			response := notePublicationUpload(t, s, scan.ScanID, 0, uuid.NewV4().String(), body, hash, false)
			require.Equal(t, 409, response.Code, response.Body.String())
			var problem api.Error
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			require.Equal(t, "published_source_diverged", problem.Code)
			page, err := s.NotePublicationSources(t.Context(), store.NotePublicationSourcesRequest{Name: "research", ScanID: scan.ScanID, Limit: 100})
			require.NoError(t, err)
			require.Empty(t, page.Items, "failed adoption never grants source authority")
		})
	}
}
