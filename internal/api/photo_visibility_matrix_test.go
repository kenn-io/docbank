package api_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

type photoRouteFixture struct {
	ts                    *httptest.Server
	s                     *testStore
	first, second         store.PhotoOwner
	firstNode, secondNode store.Node
	firstAsset            store.PhotoAsset
	firstHeaders          map[string]string
	secondHeaders         map[string]string
}

func newPhotoRouteFixture(t *testing.T) photoRouteFixture {
	t.Helper()
	ts, s := newTestServer(t, nil)
	first, err := s.CreatePhotoOwner(t.Context(), "First")
	require.NoError(t, err)
	second, err := s.CreatePhotoOwner(t.Context(), "Second")
	require.NoError(t, err)
	firstNode, err := s.CreateFile(store.WithPhotoOwner(t.Context(), first.ID), s.RootID(), "first.jpg", testHash("matrix-first"), 11, "image/jpeg")
	require.NoError(t, err)
	secondNode, err := s.CreateFile(store.WithPhotoOwner(t.Context(), second.ID), s.RootID(), "second.jpg", testHash("matrix-second"), 12, "image/jpeg")
	require.NoError(t, err)
	firstAsset, err := s.PhotoAssetForNode(store.WithPhotoOwner(t.Context(), first.ID), firstNode.ID)
	require.NoError(t, err)
	return photoRouteFixture{
		ts: ts, s: s, first: first, second: second, firstNode: firstNode, secondNode: secondNode,
		firstAsset:    firstAsset,
		firstHeaders:  map[string]string{"X-Docbank-Owner": first.ID},
		secondHeaders: map[string]string{"X-Docbank-Owner": second.ID},
	}
}

func issuePhotoOwnerSession(t *testing.T, ts *httptest.Server, owner string) string {
	t.Helper()
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", map[string]string{
		"X-Docbank-Owner": owner,
	}, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	return session.Token
}

func TestWebSessionPhotoOwner(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := do(t, f.ts, http.MethodPost, "/api/daemon/web-session", map[string]string{"X-Docbank-Owner": f.first.ID}, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	headers := map[string]string{api.WebSessionHeader: session.Token, "X-Api-Key": ""}
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", headers, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "first.jpg")
	assert.NotContains(t, body, "second.jpg")
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", map[string]string{api.WebSessionHeader: session.Token, "X-Api-Key": "", "X-Docbank-Owner": f.second.ID}, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusForbidden, response.StatusCode, body)
	assert.Contains(t, body, "browser sessions cannot override")
}

func TestWebSessionAuditedVaultWithoutPhotoOwner(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/ordinary.txt", "ordinary")
	owner, err := s.CreatePhotoOwner(t.Context(), "Audited photo owner")
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader("audited photo"))
	require.NoError(t, err)
	image, err := s.CreateFile(store.WithPhotoOwner(t.Context(), owner.ID), s.RootID(), "photo.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	token := issueWebSession(t, ts)
	response, body := do(t, ts, http.MethodPost, "/api/v1/workspace/queries", map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}, map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "ordinary.txt")
	assert.NotContains(t, body, "photo.jpg")
	response, body = get(t, ts, "/api/v1/nodes/"+strconv.FormatInt(image.ID, 10), map[string]string{"X-Api-Key": "", api.WebSessionHeader: token})
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consents", map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}, map[string]any{"principal": "daemon:operator"})
	assert.Equal(t, http.StatusForbidden, response.StatusCode, body)
}

func TestPhotoOwnerGlobalOperationalSummaries(t *testing.T) {
	ts, s := newTestServer(t, func(d *api.Deps) { d.Cfg.Backup.Repo = filepath.Join(t.TempDir(), "backup") })
	createFileWithContent(t, ts, s, "/backup.txt", "backup")
	first, err := s.CreatePhotoOwner(t.Context(), "Summary first")
	require.NoError(t, err)
	second, err := s.CreatePhotoOwner(t.Context(), "Summary second")
	require.NoError(t, err)
	firstHash, firstSize, err := s.Blobs.Write(strings.NewReader("summary first photo"))
	require.NoError(t, err)
	_, err = s.CreateFile(store.WithPhotoOwner(t.Context(), first.ID), s.RootID(), "summary-first.jpg", firstHash, firstSize, "image/jpeg")
	require.NoError(t, err)
	secondHash, secondSize, err := s.Blobs.Write(strings.NewReader("summary second photo"))
	require.NoError(t, err)
	_, err = s.CreateFile(store.WithPhotoOwner(t.Context(), second.ID), s.RootID(), "summary-second.jpg", secondHash, secondSize, "image/jpeg")
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodPost, "/api/v1/backup/init", nil, map[string]any{})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/backup/snapshots", nil, map[string]any{"tag": "matrix", "jobs": 1})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
	masterResponse, masterBody := get(t, ts, "/api/v1/storage", nil)
	require.Equal(t, http.StatusOK, masterResponse.StatusCode, masterBody)
	response, body = get(t, ts, "/api/v1/storage", headers)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	var masterStorage, browserStorage api.StorageStatus
	require.NoError(t, json.Unmarshal([]byte(masterBody), &masterStorage))
	require.NoError(t, json.Unmarshal([]byte(body), &browserStorage))
	assert.Equal(t, masterStorage.LooseBlobs, browserStorage.LooseBlobs)
	assert.Equal(t, masterStorage.LooseBytes, browserStorage.LooseBytes)
	assert.NotContains(t, body, first.ID)
	assert.NotContains(t, body, second.ID)
	assert.NotContains(t, body, "summary-first.jpg")
	assert.NotContains(t, body, "summary-second.jpg")
	masterResponse, masterBody = get(t, ts, "/api/v1/backup/snapshots", nil)
	require.Equal(t, http.StatusOK, masterResponse.StatusCode, masterBody)
	response, body = get(t, ts, "/api/v1/backup/snapshots", headers)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	var masterBackups, browserBackups api.BackupSnapshotList
	require.NoError(t, json.Unmarshal([]byte(masterBody), &masterBackups))
	require.NoError(t, json.Unmarshal([]byte(body), &browserBackups))
	assert.Equal(t, masterBackups.Items, browserBackups.Items)
	assert.NotEmpty(t, browserBackups.Items)
	assert.NotContains(t, body, first.ID)
	assert.NotContains(t, body, second.ID)
}

func TestWebSessionPhotoOwnerResources(t *testing.T) {
	f := newPhotoRouteFixture(t)
	for index := range 50 {
		createFileWithContent(t, f.ts, f.s, fmt.Sprintf("/ordinary-%02d.txt", index), "ordinary")
	}
	first, second := issuePhotoOwnerSession(t, f.ts, f.first.ID), issuePhotoOwnerSession(t, f.ts, f.first.ID)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: first}
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", headers, map[string]any{"query": map[string]any{}, "page_size": 50})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotEmpty(t, page.NextCursor)
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries/"+page.SnapshotID+"/pages", map[string]string{"X-Api-Key": "", api.WebSessionHeader: second}, map[string]string{"cursor": page.NextCursor})
	assert.Equal(t, http.StatusGone, response.StatusCode, body)
}

func TestPhotoOwnerRoutes(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	c := daemonconn.New(ts.URL, testAPIKey)
	owner, err := c.CreatePhotoOwner(t.Context(), "CLI owner")
	require.NoError(t, err)
	owners, err := c.PhotoOwners(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, owners)
	renamed, err := c.RenamePhotoOwner(t.Context(), owner.ID, owner.Revision, "Renamed")
	require.NoError(t, err)
	assert.Equal(t, "Renamed", renamed.Name)
	require.NoError(t, c.RemovePhotoOwner(t.Context(), owner.ID, renamed.Revision))
}

func TestPhotoVisibilityReadRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10), f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/content", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/versions", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/versions/"+f.firstNode.CurrentVersionID, f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/versions/"+f.firstNode.CurrentVersionID+"/content", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	verifyHeaders := map[string]string{"X-Docbank-Owner": f.second.ID, "If-Match": strconv.Quote(strconv.FormatInt(f.firstNode.Revision, 10))}
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/verify", verifyHeaders, nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/path?path=/first.jpg", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/content-references?sha256="+f.firstNode.BlobHash, f.secondHeaders)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, f.firstNode.CurrentVersionID)
	response, body = get(t, f.ts, "/api/v1/search?q=first", f.secondHeaders)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, "first.jpg")
}

func TestPhotoVisibilityPopulations(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/documents?page_size=1", f.firstHeaders)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "first.jpg")
	assert.NotContains(t, body, "second.jpg")
	var page api.DocumentPage
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.LessOrEqual(t, len(page.Items), 1)
}

func TestPhotoVisibilityAggregateRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/documents", f.secondHeaders)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, "first.jpg")
}

func TestPhotoVisibilityCachedResources(t *testing.T) {
	f := newPhotoRouteFixture(t)
	for index := range 50 {
		createFileWithContent(t, f.ts, f.s, fmt.Sprintf("/cached-%02d.txt", index), "cached resource")
	}
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", f.firstHeaders, map[string]any{"query": map[string]any{}, "page_size": 50})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var page api.WorkspaceQueryResponse
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.NotEmpty(t, page.SnapshotID)
	require.NotEmpty(t, page.NextCursor)
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries/"+page.SnapshotID+"/pages", f.secondHeaders, map[string]string{"cursor": page.NextCursor})
	assert.Equal(t, http.StatusGone, response.StatusCode, body)
	_, _, err := f.s.Trash(store.WithPhotoOwner(t.Context(), f.first.ID), f.firstNode.ID, f.firstNode.Revision)
	require.NoError(t, err)
	_, err = f.s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries/"+page.SnapshotID+"/pages", f.firstHeaders, map[string]string{"cursor": page.NextCursor})
	assert.Equal(t, http.StatusGone, response.StatusCode, body)
}

func TestPhotoVisibilityDerivedRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/content", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/versions/"+f.firstNode.CurrentVersionID+"/content", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	sourceBytes := []byte("synthetic photo source")
	sourceHash, sourceSize, err := f.s.Blobs.Write(bytes.NewReader(sourceBytes))
	require.NoError(t, err)
	pageNode, err := f.s.CreateFile(store.WithPhotoOwner(t.Context(), f.first.ID), f.s.RootID(), "derived.jpg", sourceHash, sourceSize, "image/jpeg")
	require.NoError(t, err)
	selected := document.PageSource{VersionID: pageNode.CurrentVersionID, SHA256: pageNode.BlobHash, Size: pageNode.Size}
	pageJobID := uuid.New().String()
	_, err = f.s.QueuePageJob(t.Context(), pageJobID, store.PageJobRequest{NodeID: pageNode.ID, Revision: pageNode.Revision, Source: selected, Pages: []int{1}, DPI: 72, RuntimeFingerprint: testHash("derived-page-runtime")})
	require.NoError(t, err)
	pageClaim, err := f.s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	frame, err := document.NewPDFPageFrame(selected, 1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
	require.NoError(t, err)
	require.NoError(t, f.s.PublishPageFrames(t.Context(), pageClaim, []document.PageFrameV1{frame}))
	recipe := document.PageRecipeV1{Contract: document.PageImageContractV1, DPI: 72, Format: "png", RendererIdentity: document.PageRendererIdentity{Executable: "synthetic-renderer", Version: "1", Options: []string{"crop-visible"}}}
	_, frameHash, err := document.MarshalPageFrameV1(frame)
	require.NoError(t, err)
	_, recipeHash, err := document.MarshalPageRecipeV1(recipe)
	require.NoError(t, err)
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 72, 72))))
	imageHash, imageSize, err := f.s.Blobs.Write(bytes.NewReader(imageBytes.Bytes()))
	require.NoError(t, err)
	require.NoError(t, f.s.PublishPageImage(t.Context(), pageClaim, document.PageImageV1{Contract: document.PageImageContractV1, Source: selected, Page: 1, FrameSHA256: frameHash, RecipeSHA256: recipeHash, SHA256: imageHash, Size: imageSize, Width: 72, Height: 72}, recipe, &store.BlobPhysical{Encoding: "raw", StoredBytes: imageSize}))
	require.NoError(t, f.s.FinishPageJob(t.Context(), pageClaim, store.PageJobCompleted, ""))
	pageURL := fmt.Sprintf("/api/v1/pages/image?node_id=%d&revision=%d&version_id=%s&source_sha256=%s&source_size=%d&page=1&recipe_sha256=%s&frame_sha256=%s&image_sha256=%s",
		pageNode.ID, pageNode.Revision, pageNode.CurrentVersionID, pageNode.BlobHash, pageNode.Size, recipeHash, frameHash, imageHash)
	response, body = get(t, f.ts, pageURL, f.firstHeaders)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = get(t, f.ts, pageURL, f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Helvetica", "", 12)
	pdf.Text(20, 20, "Synthetic matrix PDF")
	var pdfBytes bytes.Buffer
	require.NoError(t, pdf.Output(&pdfBytes))
	var runtime *processing.EmailPDFRuntime
	gate := api.NewOperationGate()
	emailTS, emailS := newTestServer(t, func(d *api.Deps) {
		runtime = &processing.EmailPDFRuntime{
			Catalog: d.Store, Blobs: d.Blobs, Renderer: emailPDFTestRenderer(pdfBytes.Bytes()), Spool: t.TempDir(),
			Recipe: document.EmailPDFRecipeV1{Contract: document.EmailPDFContract, RendererVersion: "synthetic", RendererSHA256: strings.Repeat("a", 64), WorkerSHA256: strings.Repeat("b", 64), BubblewrapSHA256: strings.Repeat("d", 64), FontsSHA256: strings.Repeat("c", 64), Paper: "A4"},
		}
		d.Gate = gate
		d.RequestEmailPDF = runtime.Submit
	})
	emailOwner, err := emailS.CreatePhotoOwner(t.Context(), "Email matrix owner")
	require.NoError(t, err)
	emailHash, emailSize, err := emailS.Blobs.Write(strings.NewReader("Subject: Matrix\r\nContent-Type: text/plain\r\n\r\nbody"))
	require.NoError(t, err)
	emailNode, err := emailS.CreateFile(store.WithPhotoOwner(t.Context(), emailOwner.ID), emailS.RootID(), "matrix.eml", emailHash, emailSize, "image/jpeg")
	require.NoError(t, err)
	response, body = do(t, emailTS, http.MethodPost, "/api/v1/email-pdfs", map[string]string{"X-Docbank-Owner": emailOwner.ID}, document.EmailPDFRequest{VersionID: emailNode.CurrentVersionID, Paper: "A4"})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var emailJob document.EmailPDFJob
	require.NoError(t, json.Unmarshal([]byte(body), &emailJob))
	worker, err := processing.NewRenditionWorker(processing.RenditionWorkerConfig{Catalog: emailS.Store, Blobs: emailS.Blobs, Runtime: runtime, Gate: gate, Owner: "matrix-email-pdf", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	_, err = worker.RunJob(t.Context(), emailJob.JobID)
	require.NoError(t, err)
	emailReceipt, err := emailS.EmailPDFReceipt(t.Context(), emailNode.CurrentVersionID, emailJob.ProfileFingerprint)
	require.NoError(t, err)
	emailPath := "/api/v1/email-pdfs/" + emailNode.CurrentVersionID + "/" + emailJob.ProfileFingerprint + "/content"
	response, body = get(t, emailTS, emailPath, map[string]string{"X-Docbank-Owner": emailOwner.ID})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Equal(t, emailReceipt.Output.PDFSHA256, response.Header.Get(api.BlobHashHeader))
	otherEmailOwner, err := emailS.CreatePhotoOwner(t.Context(), "Email matrix other")
	require.NoError(t, err)
	response, body = get(t, emailTS, emailPath, map[string]string{"X-Docbank-Owner": otherEmailOwner.ID})
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoVisibilityHistoryRoutes(t *testing.T) {
	f := newPhotoRouteFixture(t)
	response, body := get(t, f.ts, "/api/v1/audit/status?node_id="+strconv.FormatInt(f.firstNode.ID, 10), f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/audit/history?node_id="+strconv.FormatInt(f.firstNode.ID, 10), f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/provenance", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/nodes/"+strconv.FormatInt(f.firstNode.ID, 10)+"/tags", f.secondHeaders)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func exercisePhotoVisibilityPruneVersions(t *testing.T) {
	t.Helper()
	f := newPhotoRouteFixture(t)
	oldVersionID := f.firstNode.CurrentVersionID
	updated, _, err := f.s.ReplaceContent(store.WithPhotoOwner(t.Context(), f.first.ID), f.firstNode.ID, f.firstNode.Revision,
		testHash("prune-current"), 14, "image/jpeg")
	require.NoError(t, err)
	path := "/api/v1/nodes/" + strconv.FormatInt(f.firstNode.ID, 10) + "/versions/prune"
	foreignBody := map[string]any{"version_ids": []string{oldVersionID}}
	foreignHeaders := map[string]string{"X-Docbank-Owner": f.second.ID, "If-Match": strconv.Quote(strconv.FormatInt(updated.Revision, 10))}
	ownerHeaders := map[string]string{"X-Docbank-Owner": f.first.ID, "If-Match": strconv.Quote(strconv.FormatInt(updated.Revision, 10))}
	response, body := do(t, f.ts, http.MethodPost, path, foreignHeaders, foreignBody)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = do(t, f.ts, http.MethodPost, path, foreignHeaders, map[string]any{"version_ids": []string{oldVersionID}, "run": true})
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	after, err := f.s.NodeByID(t.Context(), f.firstNode.ID)
	require.NoError(t, err)
	assert.Equal(t, updated.Revision, after.Revision)
	_, err = f.s.ContentVersionByID(t.Context(), oldVersionID)
	require.NoError(t, err)
	response, body = do(t, f.ts, http.MethodPost, path, ownerHeaders, foreignBody)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, f.ts, http.MethodPost, path, ownerHeaders, map[string]any{"version_ids": []string{oldVersionID}, "run": true})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	_, err = f.s.ContentVersionByID(t.Context(), oldVersionID)
	require.ErrorIs(t, err, store.ErrNotFound)
	ordinary, err := f.s.CreateFile(t.Context(), f.s.RootID(), "prune-ordinary.txt", testHash("prune-ordinary-old"), 19, "text/plain")
	require.NoError(t, err)
	ordinaryNext, _, err := f.s.ReplaceContent(t.Context(), ordinary.ID, ordinary.Revision, testHash("prune-ordinary-new"), 20, "text/plain")
	require.NoError(t, err)
	ordinaryPath := "/api/v1/nodes/" + strconv.FormatInt(ordinary.ID, 10) + "/versions/prune"
	ordinaryHeaders := map[string]string{"X-Docbank-Owner": f.first.ID, "If-Match": strconv.Quote(strconv.FormatInt(ordinaryNext.Revision, 10))}
	response, body = do(t, f.ts, http.MethodPost, ordinaryPath, ordinaryHeaders, map[string]any{"all_prior": true, "run": true})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, ordinaryNext.CurrentVersionID)
}

func TestPhotoVisibilityPruneVersions(t *testing.T) {
	exercisePhotoVisibilityPruneVersions(t)
}

func TestPhotoVisibilityMutationAtomicity(t *testing.T) {
	f := newPhotoRouteFixture(t)
	headers := map[string]string{"X-Docbank-Owner": f.second.ID, "If-Match": strconv.Quote("1")}
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/photos/assets/"+f.firstAsset.ID+"/exclude", headers, map[string]any{"excluded": true})
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}

func TestPhotoVisibilityDownloadTicket(t *testing.T) {
	f := newPhotoRouteFixture(t)
	hash, size, err := f.s.Blobs.Write(strings.NewReader("downloadable photo"))
	require.NoError(t, err)
	node, err := f.s.CreateFile(store.WithPhotoOwner(t.Context(), f.first.ID), f.s.RootID(), "downloadable.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	session := issuePhotoOwnerSession(t, f.ts, f.first.ID)
	response := prepareWebDownload(t, f.ts, session, map[string]any{
		"node_id": node.ID, "revision": node.Revision,
		"version_id": node.CurrentVersionID, "blob_hash": node.BlobHash,
		"size": node.Size,
	})
	readyURL := readyWebDownloadURL(t, response)
	response, err = f.ts.Client().Get(f.ts.URL + readyURL)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusOK, response.StatusCode, string(body))
	response, err = f.ts.Client().Get(f.ts.URL + readyURL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
	hash, size, err = f.s.Blobs.Write(strings.NewReader("revoked downloadable photo"))
	require.NoError(t, err)
	revokedNode, err := f.s.CreateFile(store.WithPhotoOwner(t.Context(), f.first.ID), f.s.RootID(), "revoked.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	revokedResponse := prepareWebDownload(t, f.ts, session, map[string]any{
		"node_id": revokedNode.ID, "revision": revokedNode.Revision,
		"version_id": revokedNode.CurrentVersionID, "blob_hash": revokedNode.BlobHash,
		"size": revokedNode.Size,
	})
	revokedURL := readyWebDownloadURL(t, revokedResponse)
	_, _, err = f.s.Trash(store.WithPhotoOwner(t.Context(), f.first.ID), revokedNode.ID, revokedNode.Revision)
	require.NoError(t, err)
	_, err = f.s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	response, err = f.ts.Client().Get(f.ts.URL + revokedURL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestProcessingPhotoOwnerConsent(t *testing.T) {
	ts, s := newTestServer(t, configureProcessingTestService(t))
	first, err := s.EnsureDefaultPhotoOwner(t.Context())
	require.NoError(t, err)
	second, err := s.CreatePhotoOwner(t.Context(), "Consent second")
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader("consent photo"))
	require.NoError(t, err)
	node, err := s.CreateFile(store.WithPhotoOwner(t.Context(), first.ID), s.RootID(), "consent-first.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	secondHash, secondSize, err := s.Blobs.Write(strings.NewReader("second consent photo"))
	require.NoError(t, err)
	secondNode, err := s.CreateFile(store.WithPhotoOwner(t.Context(), second.ID), s.RootID(), "consent-second.jpg", secondHash, secondSize, "image/jpeg")
	require.NoError(t, err)
	firstSelector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "private"}
	secondSelector := api.ProcessingSelector{NodeID: secondNode.ID, ContentVersionID: secondNode.CurrentVersionID, Profile: "private"}
	firstHeaders := map[string]string{"X-Docbank-Owner": first.ID}
	secondHeaders := map[string]string{"X-Docbank-Owner": second.ID}
	response, body := do(t, ts, http.MethodPost, "/api/v1/processing/plans", firstHeaders, api.ProcessingPlanRequest{Selector: firstSelector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	var firstPlan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(body), &firstPlan))
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consent/grants", firstHeaders,
		api.ProcessingConsentGrantRequest{Selector: firstSelector, PlanFingerprint: firstPlan.Fingerprint})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", firstHeaders, api.ProcessingPlanRequest{Selector: firstSelector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, `"consent_state":"active"`)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", secondHeaders, api.ProcessingPlanRequest{Selector: secondSelector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, `"consent_state":"required"`)
	var secondPlan api.ProcessingPlan
	require.NoError(t, json.Unmarshal([]byte(body), &secondPlan))
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/jobs", secondHeaders,
		api.StartProcessingRequest{Selector: secondSelector, PlanFingerprint: secondPlan.Fingerprint})
	assert.NotEqual(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", secondHeaders, api.ProcessingPlanRequest{Selector: secondSelector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consent/grants", secondHeaders,
		api.ProcessingConsentGrantRequest{Selector: secondSelector, PlanFingerprint: secondPlan.Fingerprint})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consent/revocations", firstHeaders, nil)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", firstHeaders, api.ProcessingPlanRequest{Selector: firstSelector})
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, `"consent_state":"revoked"`)
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/plans", headers, api.ProcessingPlanRequest{Selector: firstSelector})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consents", headers, map[string]any{
		"principal": "owner:00000000-0000-4000-8000-000000000099", "scope": "document-processing",
		"profile_fingerprint": "spoof", "disclosure_fingerprint": "spoof",
		"input_classes": []string{"original_file"}, "retained_artifact_classes": []string{"text"},
	})
	assert.NotEqual(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "web_session_read_only")
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/consents", firstHeaders, map[string]any{
		"principal": "owner:" + second.ID, "scope": "document-processing",
		"profile_fingerprint": strings.Repeat("a", 64), "disclosure_fingerprint": strings.Repeat("b", 64),
		"input_classes": []string{"original_file"}, "retained_artifact_classes": []string{"text"},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
	assert.Contains(t, body, "does not match the authenticated photo owner")
}

func TestPhotoOwnerMaintenanceGateRoute(t *testing.T) {
	gate := api.NewOperationGate()
	ts, _ := newTestServer(t, func(d *api.Deps) { d.Gate = gate })
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- gate.MaintainContext(t.Context(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	response, body := do(t, ts, http.MethodPost, "/api/v1/photos/owners", nil, map[string]string{"name": "blocked"})
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	assert.Contains(t, body, "maintenance_busy")
	response, body = do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
	assert.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	assert.Contains(t, body, "maintenance_busy")
	close(release)
	require.NoError(t, <-done)
}

func TestPhotoVisibilityRouteCoverage(t *testing.T) {
	ts, fixture := newTestServer(t, func(d *api.Deps) {
		d.ShutdownToken = "route-probe-token"
		configureProcessingTestService(t)(d)
	})
	expected := map[string]string{
		"cancelWebDownload":                  "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"prepareWebDownload":                 "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"revokeWebSession":                   "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"createWebSession":                   "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"enableAudit":                        "TestPhotoVisibilityHistoryRoutes",
		"auditNodeHistory":                   "TestPhotoVisibilityHistoryRoutes",
		"previewAuditEnrollment":             "TestPhotoVisibilityHistoryRoutes",
		"auditScopeHistory":                  "TestPhotoVisibilityHistoryRoutes",
		"auditStatus":                        "TestPhotoVisibilityHistoryRoutes",
		"verifyAudit":                        "TestPhotoVisibilityHistoryRoutes",
		"initBackupRepository":               "master-only whole-vault capability; no photo-specific source is selected",
		"restoreBackupSnapshot":              "master-only whole-vault capability; no photo-specific source is selected",
		"streamBackupSnapshotRestore":        "master-only whole-vault capability; no photo-specific source is selected",
		"listBackupSnapshots":                "master-only whole-vault capability; no photo-specific source is selected",
		"createBackupSnapshot":               "master-only whole-vault capability; no photo-specific source is selected",
		"streamBackupSnapshotCreation":       "master-only whole-vault capability; no photo-specific source is selected",
		"verifyBackupRepository":             "master-only whole-vault capability; no photo-specific source is selected",
		"streamBackupRepositoryVerification": "master-only whole-vault capability; no photo-specific source is selected",
		"batchMove":                          "TestPhotoVisibilityMutationAtomicity",
		"changeBatchTags":                    "TestPhotoVisibilityMutationAtomicity",
		"previewBatchTags":                   "TestPhotoVisibilityMutationAtomicity",
		"reserveBatesRange":                  "TestPhotoVisibilityBatesArtifactIDs",
		"readBatesAllocation":                "TestPhotoVisibilityBatesArtifactIDs",
		"listBatesExports":                   "TestPhotoVisibilityBatesArtifactIDs",
		"publishBatesExport":                 "TestPhotoVisibilityBatesArtifactIDs",
		"findBatesExports":                   "TestPhotoVisibilityBatesArtifactIDs",
		"readBatesExport":                    "TestPhotoVisibilityBatesArtifactIDs",
		"downloadBatesExportContent":         "TestPhotoVisibilityBatesArtifactIDs",
		"downloadBatesExport":                "TestPhotoVisibilityBatesArtifactIDs",
		"listBatesNamespaces":                "TestPhotoVisibilityBatesArtifactIDs",
		"createBatesNamespace":               "TestPhotoVisibilityBatesArtifactIDs",
		"planBatesStamp":                     "TestPhotoVisibilityBatesArtifactIDs",
		"listCollections":                    "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getCollection":                      "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getCollectionLabel":                 "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"setCollectionLabel":                 "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"listCollectionMembers":              "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getCollectionQuality":               "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"lookupContentReferences":            "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getDocumentProcessingCoverage":      "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"runDerivativePurge":                 "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"planDerivativePurge":                "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"listDocuments":                      "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"resolveDocumentSummaries":           "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"listDuplicateContent":               "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getDuplicateContentByHash":          "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"requestEmailDocumentProcessing":     "TestPhotoVisibilityDerivedRoutes",
		"publishEmailDocuments":              "TestPhotoVisibilityDerivedRoutes",
		"removeEmailDocumentPublication":     "TestPhotoVisibilityDerivedRoutes",
		"getEmailDocumentPublication":        "TestPhotoVisibilityDerivedRoutes",
		"listEmailDocumentRelations":         "TestPhotoVisibilityDerivedRoutes",
		"getEmailPDFJob":                     "TestPhotoVisibilityDerivedRoutes",
		"renderEmailPDF":                     "TestPhotoVisibilityDerivedRoutes",
		"listEmailPDFs":                      "TestPhotoVisibilityDerivedRoutes",
		"getEmailPDF":                        "TestPhotoVisibilityDerivedRoutes",
		"downloadEmailPDF":                   "TestPhotoVisibilityDerivedRoutes",
		"createExportJob":                    "TestPhotoVisibilityExports",
		"getExportJob":                       "TestPhotoVisibilityExports",
		"cancelExportJob":                    "TestPhotoVisibilityExports",
		"downloadExportArchive":              "TestPhotoVisibilityExports",
		"getExportJobEvents":                 "TestPhotoVisibilityExports",
		"createExportPlan":                   "TestPhotoVisibilityExports",
		"getExportPlan":                      "TestPhotoVisibilityExports",
		"getExportPlanPreview":               "TestPhotoVisibilityExports",
		"getExportOutputProblems":            "TestPhotoVisibilityExports",
		"createExportSource":                 "TestPhotoVisibilityExports",
		"getExportAttachmentPublications":    "TestPhotoVisibilityExports",
		"putExportChunk":                     "TestPhotoVisibilityExports",
		"getExportEmailPDFRecipes":           "TestPhotoVisibilityExports",
		"sealExportSource":                   "TestPhotoVisibilityExports",
		"readFormatCapabilities":             "master-only whole-vault capability; no photo-specific source is selected",
		"gc":                                 "master-only whole-vault capability; no photo-specific source is selected",
		"vaultInfo":                          "master-only whole-vault capability; no photo-specific source is selected",
		"ingest":                             "ingest and mailbox handlers do not return an existing photo source",
		"preflightIngest":                    "ingest and mailbox handlers do not return an existing photo source",
		"streamIngest":                       "ingest and mailbox handlers do not return an existing photo source",
		"listJobs":                           "master-only whole-vault capability; no photo-specific source is selected",
		"getStorageOperation":                "master-only whole-vault capability; no photo-specific source is selected",
		"cancelStorageOperation":             "master-only whole-vault capability; no photo-specific source is selected",
		"registerMailboxArchive":             "ingest and mailbox handlers do not return an existing photo source",
		"beginMailboxContainer":              "ingest and mailbox handlers do not return an existing photo source",
		"abortMailboxContainer":              "ingest and mailbox handlers do not return an existing photo source",
		"getMailboxContainer":                "ingest and mailbox handlers do not return an existing photo source",
		"uploadMailboxChunk":                 "ingest and mailbox handlers do not return an existing photo source",
		"previewMailboxContainer":            "ingest and mailbox handlers do not return an existing photo source",
		"sealMailboxContainer":               "ingest and mailbox handlers do not return an existing photo source",
		"listMailboxJobs":                    "ingest and mailbox handlers do not return an existing photo source",
		"beginMailboxJob":                    "ingest and mailbox handlers do not return an existing photo source",
		"getMailboxJob":                      "ingest and mailbox handlers do not return an existing photo source",
		"cancelMailboxJob":                   "ingest and mailbox handlers do not return an existing photo source",
		"mailboxEvents":                      "ingest and mailbox handlers do not return an existing photo source",
		"mailboxOccurrences":                 "ingest and mailbox handlers do not return an existing photo source",
		"resumeMailboxJob":                   "ingest and mailbox handlers do not return an existing photo source",
		"transferMailboxEML":                 "ingest and mailbox handlers do not return an existing photo source",
		"planMediaAcquisition":               "TestProcessingPhotoOwnerConsent",
		"grantMediaAcquisitionConsent":       "TestProcessingPhotoOwnerConsent",
		"revokeMediaAcquisitionConsent":      "TestProcessingPhotoOwnerConsent",
		"listMediaOccurrences":               "TestProcessingPhotoOwnerConsent",
		"declareMediaOccurrence":             "TestProcessingPhotoOwnerConsent",
		"revokeMediaOccurrence":              "TestProcessingPhotoOwnerConsent",
		"listMediaOrigins":                   "TestProcessingPhotoOwnerConsent",
		"listMediaSources":                   "TestProcessingPhotoOwnerConsent",
		"submitMediaSource":                  "TestProcessingPhotoOwnerConsent",
		"getMediaSource":                     "TestProcessingPhotoOwnerConsent",
		"importMediaArtifact":                "TestProcessingPhotoOwnerConsent",
		"retryMediaSource":                   "TestProcessingPhotoOwnerConsent",
		"createNode":                         "TestPhotoVisibilityHistoryRoutes",
		"getNode":                            "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"moveNode":                           "TestPhotoVisibilityMutationAtomicity",
		"listChildren":                       "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getNodeContent":                     "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"replaceNodeContent":                 "TestPhotoVisibilityMutationAtomicity",
		"listNodeProvenance":                 "TestPhotoVisibilityHistoryRoutes",
		"appendNodeProvenance":               "TestPhotoVisibilityHistoryRoutes",
		"restoreNode":                        "TestPhotoVisibilityMutationAtomicity",
		"revertNodeContent":                  "TestPhotoVisibilityMutationAtomicity",
		"listNodeTags":                       "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"unassignTag":                        "TestPhotoVisibilityMutationAtomicity",
		"assignTag":                          "TestPhotoVisibilityMutationAtomicity",
		"trashNode":                          "TestPhotoVisibilityMutationAtomicity",
		"verifyNodeContent":                  "TestPhotoVisibilityHistoryRoutes",
		"listContentVersions":                "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"pruneNodeContentVersions":           "exercisePhotoVisibilityPruneVersions",
		"listPackages":                       "package member visibility fixtures",
		"getPackage":                         "package member visibility fixtures",
		"assignPackageCustodian":             "package member visibility fixtures",
		"listPackageCustodians":              "package member visibility fixtures",
		"listPackageMembers":                 "package member visibility fixtures",
		"getPackageRecord":                   "package member visibility fixtures",
		"listPackageTimelineInputs":          "package member visibility fixtures",
		"beginPackageContainer":              "package member visibility fixtures",
		"abortPackageContainer":              "package member visibility fixtures",
		"getPackageContainer":                "package member visibility fixtures",
		"uploadPackageChunk":                 "package member visibility fixtures",
		"preflightPackageContainer":          "package member visibility fixtures",
		"sealPackageContainer":               "package member visibility fixtures",
		"resolvePackageCustodian":            "package member visibility fixtures",
		"listPackageFieldCatalog":            "package member visibility fixtures",
		"createPackageImport":                "package member visibility fixtures",
		"readPackageImport":                  "package member visibility fixtures",
		"cancelPackageImport":                "package member visibility fixtures",
		"listPackageLabelCandidates":         "package member visibility fixtures",
		"createPackagePreflight":             "package member visibility fixtures",
		"readPackagePreflight":               "package member visibility fixtures",
		"readPackagePreflightDiagnostics":    "package member visibility fixtures",
		"readPageImage":                      "TestPhotoVisibilityDerivedRoutes",
		"pageInventory":                      "TestPhotoVisibilityDerivedRoutes",
		"createPageRenderJob":                "TestPhotoVisibilityDerivedRoutes",
		"getPageRenderJob":                   "TestPhotoVisibilityDerivedRoutes",
		"cancelPageRenderJob":                "TestPhotoVisibilityDerivedRoutes",
		"resolvePath":                        "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"mkdirPath":                          "TestPhotoVisibilityMutationAtomicity",
		"movePath":                           "TestPhotoVisibilityMutationAtomicity",
		"unassignTagPath":                    "TestPhotoVisibilityMutationAtomicity",
		"assignTagPath":                      "TestPhotoVisibilityMutationAtomicity",
		"trashPath":                          "TestPhotoVisibilityMutationAtomicity",
		"listPeople":                         "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getPeopleCoverage":                  "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"rebuildDocumentPeople":              "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getPeopleRebuild":                   "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"createPhotoAsset":                   "TestPhotoOwnerRoutes",
		"getPhotoAsset":                      "TestPhotoOwnerRoutes",
		"setPhotoDisplay":                    "TestPhotoOwnerRoutes",
		"excludePhotoAsset":                  "TestPhotoOwnerRoutes",
		"attachPhotoFile":                    "TestPhotoOwnerRoutes",
		"detachPhotoFile":                    "TestPhotoOwnerRoutes",
		"getPhotoAssetByNode":                "TestPhotoOwnerRoutes",
		"promotePhotoNode":                   "TestPhotoOwnerRoutes",
		"listPhotoOwners":                    "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"createPhotoOwner":                   "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"removePhotoOwner":                   "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"renamePhotoOwner":                   "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"getPhotoSettings":                   "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"setPhotoSettings":                   "TestWebSessionPhotoOwner and TestPhotoOwnerRoutes",
		"grantDocumentProcessingConsent":     "TestProcessingPhotoOwnerConsent",
		"revokeDocumentProcessingConsent":    "TestProcessingPhotoOwnerConsent",
		"grantProcessingConsent":             "TestProcessingPhotoOwnerConsent",
		"revokeProcessingConsent":            "TestProcessingPhotoOwnerConsent",
		"startDocumentProcessing":            "TestProcessingPhotoOwnerConsent",
		"getDocumentProcessingJob":           "TestProcessingPhotoOwnerConsent",
		"planDocumentProcessing":             "TestProcessingPhotoOwnerConsent",
		"listDocumentProcessingProfiles":     "TestProcessingPhotoOwnerConsent",
		"resolveDocumentSourceFence":         "TestProcessingPhotoOwnerConsent",
		"previewQueryHighlights":             "TestPhotoVisibilityDerivedRoutes",
		"parseQuery":                         "TestPhotoVisibilityDerivedRoutes",
		"readDocumentRenditionBySelector":    "TestPhotoVisibilityDerivedRoutes",
		"resolveRenditionText":               "TestPhotoVisibilityDerivedRoutes",
		"readRenditionText":                  "TestPhotoVisibilityDerivedRoutes",
		"readDocumentRenditionWindow":        "TestPhotoVisibilityDerivedRoutes",
		"getDocumentRendition":               "TestPhotoVisibilityDerivedRoutes",
		"listSavedQueries":                   "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"createSavedQuery":                   "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"deleteSavedQuery":                   "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"getSavedQuery":                      "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"updateSavedQuery":                   "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"runSavedQuery":                      "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"search":                             "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"searchDocuments":                    "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"listTermReportHistory":              "exerciseTermReportRoutes",
		"createTermReport":                   "exerciseTermReportRoutes",
		"getTermReport":                      "exerciseTermReportRoutes",
		"downloadTermReportbundle":           "exerciseTermReportRoutes",
		"downloadTermReportcsv":              "exerciseTermReportRoutes",
		"getTermReportDates":                 "exerciseTermReportRoutes",
		"issueTermReportDownload":            "exerciseTermReportBrowserTicket",
		"reviseTermReport":                   "exerciseTermReportRoutes",
		"findSimilarDocuments":               "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"validateDocumentSearch":             "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"storageStatus":                      "master-only whole-vault capability; no photo-specific source is selected",
		"startStorageEvacuation":             "master-only whole-vault capability; no photo-specific source is selected",
		"previewStorageEvacuation":           "master-only whole-vault capability; no photo-specific source is selected",
		"storagePack":                        "master-only whole-vault capability; no photo-specific source is selected",
		"startStoragePlacement":              "master-only whole-vault capability; no photo-specific source is selected",
		"previewStoragePlacement":            "master-only whole-vault capability; no photo-specific source is selected",
		"storageRepack":                      "master-only whole-vault capability; no photo-specific source is selected",
		"startStorageRepair":                 "master-only whole-vault capability; no photo-specific source is selected",
		"previewStorageRepair":               "master-only whole-vault capability; no photo-specific source is selected",
		"startStorageSalvage":                "master-only whole-vault capability; no photo-specific source is selected",
		"previewStorageSalvage":              "master-only whole-vault capability; no photo-specific source is selected",
		"listBlobStores":                     "master-only whole-vault capability; no photo-specific source is selected",
		"registerBlobStore":                  "master-only whole-vault capability; no photo-specific source is selected",
		"previewBlobStoreRegistration":       "master-only whole-vault capability; no photo-specific source is selected",
		"unregisterBlobStore":                "master-only whole-vault capability; no photo-specific source is selected",
		"detachBlobStore":                    "master-only whole-vault capability; no photo-specific source is selected",
		"listTags":                           "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"createTag":                          "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"resolveTagByName":                   "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"deleteTag":                          "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getTag":                             "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"renameTag":                          "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"listTagNodes":                       "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"readTimelineCoverage":               "exerciseTimelineRoutes",
		"createTimelineRebuild":              "exerciseTimelineRoutes",
		"readTimelineRebuild":                "exerciseTimelineRoutes",
		"listTrash":                          "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"emptyTrash":                         "TestPhotoVisibilityMutationAtomicity",
		"uploadFile":                         "ingest and mailbox handlers do not return an existing photo source",
		"verify":                             "TestPhotoVisibilityMutationAtomicity",
		"getContentVersion":                  "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getContentVersionBytes":             "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getEmailMetadata":                   "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"ensureEmailMetadata":                "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getEmailMetadataGeneration":         "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"getEmailPart":                       "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"listWatchedInboxes":                 "TestPhotoVisibilityReadRoutes and TestPhotoVisibilityPopulations",
		"createWorkspaceQuery":               "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
		"readWorkspaceQueryPage":             "TestPhotoVisibilityCachedResources and TestPhotoVisibilityAggregateRoutes",
	}
	for _, fixtureCase := range []struct {
		name       string
		helper     string
		operations []string
		run        func(*testing.T)
	}{
		{name: "prune", helper: "exercisePhotoVisibilityPruneVersions",
			operations: []string{"pruneNodeContentVersions"}, run: exercisePhotoVisibilityPruneVersions},
		{name: "term-report", helper: "exerciseTermReportRoutes",
			operations: []string{"listTermReportHistory", "createTermReport", "getTermReport",
				"downloadTermReportbundle", "downloadTermReportcsv", "getTermReportDates", "reviseTermReport"},
			run: exerciseTermReportRoutes},
		{name: "term-report-download", helper: "exerciseTermReportBrowserTicket",
			operations: []string{"issueTermReportDownload"}, run: exerciseTermReportBrowserTicket},
		{name: "timeline", helper: "exerciseTimelineRoutes",
			operations: []string{"readTimelineCoverage", "createTimelineRebuild", "readTimelineRebuild"},
			run:        exerciseTimelineRoutes},
	} {
		t.Run("fixture/"+fixtureCase.name, func(t *testing.T) {
			for _, operationID := range fixtureCase.operations {
				require.Equal(t, fixtureCase.helper, expected[operationID], operationID)
			}
			fixtureCase.run(t)
		})
	}
	for operationID, disposition := range expected {
		require.NotEmpty(t, disposition, operationID+" needs a photo visibility disposition")
	}
	seen := make(map[string]string, len(expected))
	inspect := func(method, path, operationID string) {
		require.NotEmpty(t, operationID, method+" "+path+" has no operation ID")
		if previous, duplicate := seen[operationID]; duplicate {
			t.Fatalf("operation ID %q is registered twice: %s and %s", operationID, previous, method+" "+path)
		}
		require.Contains(t, expected, operationID, "new registered operation must be added to this inventory")
		seen[operationID] = method + " " + path
	}
	for path, item := range fixture.Server.API().OpenAPI().Paths {
		require.NotNil(t, item, path)
		if item.Get != nil {
			inspect(http.MethodGet, path, item.Get.OperationID)
		}
		if item.Put != nil {
			inspect(http.MethodPut, path, item.Put.OperationID)
		}
		if item.Post != nil {
			inspect(http.MethodPost, path, item.Post.OperationID)
		}
		if item.Delete != nil {
			inspect(http.MethodDelete, path, item.Delete.OperationID)
		}
		if item.Options != nil {
			inspect(http.MethodOptions, path, item.Options.OperationID)
		}
		if item.Head != nil {
			inspect(http.MethodHead, path, item.Head.OperationID)
		}
		if item.Patch != nil {
			inspect(http.MethodPatch, path, item.Patch.OperationID)
		}
		if item.Trace != nil {
			inspect(http.MethodTrace, path, item.Trace.OperationID)
		}
	}
	require.Len(t, seen, len(expected))
	for operationID := range expected {
		assert.Contains(t, seen, operationID)
	}

	// These Huma paths are the document, aggregate, derivative and export
	// families whose handlers can expose a photo-backed node.
	for _, path := range []string{
		"/api/v1/photos/assets/{asset_id}", "/api/v1/photos/assets/{asset_id}/exclude",
		"/api/v1/nodes/{id}", "/api/v1/nodes/{id}/content", "/api/v1/nodes/{id}/versions",
		"/api/v1/documents", "/api/v1/search", "/api/v1/duplicates", "/api/v1/duplicates/by-hash",
		"/api/v1/workspace/queries", "/api/v1/workspace/queries/{id}/pages",
		"/api/v1/processing/source-fences/resolve",
		"/api/v1/processing/consent/grants", "/api/v1/processing/consent/revocations",
		"/api/v1/exports/sources", "/api/v1/exports/sources/{id}/email-pdf-recipes",
		"/api/v1/exports/sources/{id}/attachment-publications", "/api/v1/exports/plans/{id}",
		"/api/v1/exports/plans/{id}/preview", "/api/v1/exports/plans/{id}/problems",
		"/api/v1/exports/jobs/{id}", "/api/v1/exports/jobs/{id}/download",
		"/api/v1/audit/history", "/api/v1/audit/scopes/{scope_id}/history",
		"/api/v1/renditions/text/content", "/api/v1/pages/image", "/api/v1/email-pdfs/{version_id}",
		"/api/v1/uploads",
	} {
		assert.Contains(t, fixture.Server.API().OpenAPI().Paths, path)
	}

	// Raw mux handlers aren't represented in OpenAPI. Probe each registered
	// method and path so a removed handler returns the mux's plain 404 page.
	for _, route := range []struct {
		method, path string
	}{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/api/daemon/challenge"},
		{http.MethodPost, "/api/daemon/shutdown"},
		{http.MethodGet, "/api/v1/exports/jobs/00000000-0000-4000-8000-000000000001/events"},
		{http.MethodGet, "/api/v1/email-pdfs/00000000-0000-4000-8000-000000000001"},
		{http.MethodGet, "/api/v1/email-pdfs/00000000-0000-4000-8000-000000000001/private/content"},
		{http.MethodGet, "/api/v1/versions/00000000-0000-4000-8000-000000000001/email"},
		{http.MethodGet, "/api/v1/versions/00000000-0000-4000-8000-000000000001/email/generations/00000000-0000-4000-8000-000000000002/parts/body/plain"},
		{http.MethodPut, "/api/v1/nodes/1/content"},
		{http.MethodGet, "/api/v1/bates/exports/00000000-0000-4000-8000-000000000001/content"},
		{http.MethodGet, "/api/daemon/web-upload"},
		{http.MethodPost, "/api/daemon/web-session"},
		{http.MethodDelete, "/api/daemon/web-session"},
		{http.MethodPost, "/api/daemon/web-download"},
		{http.MethodDelete, "/api/daemon/web-download"},
		{http.MethodGet, "/api/daemon/web-download/file"},
		{http.MethodPost, "/api/v1/uploads"},
		{http.MethodPost, "/api/v1/media/sources"},
		{http.MethodPost, "/api/v1/media/sources/probe/artifacts"},
		{http.MethodPost, "/api/v1/packages/containers"},
		{http.MethodGet, "/api/v1/packages/containers/probe"},
		{http.MethodPost, "/api/v1/packages/containers/probe/seal"},
		{http.MethodDelete, "/api/v1/packages/containers/probe"},
		{http.MethodPost, "/api/v1/packages/containers/probe/preflight"},
		{http.MethodPost, "/api/v1/packages/preflights"},
		{http.MethodPost, "/api/v1/packages/imports"},
		{http.MethodGet, "/api/v1/packages/imports/probe"},
		{http.MethodPost, "/api/v1/mailbox/containers"},
		{http.MethodGet, "/api/v1/mailbox/containers/probe"},
		{http.MethodPost, "/api/v1/mailbox/containers/probe/seal"},
		{http.MethodDelete, "/api/v1/mailbox/containers/probe"},
		{http.MethodPost, "/api/v1/mailbox/archives"},
		{http.MethodPost, "/api/v1/mailbox/transfers"},
	} {
		_, body := do(t, ts, route.method, route.path, nil, nil)
		assert.NotEqual(t, "404 page not found\n", body, route.method+" "+route.path)
	}

	// Probe real data-bearing handlers with a foreign photo. Route registration
	// alone cannot prove that a handler asks the store visibility authority.
	first, err := fixture.CreatePhotoOwner(t.Context(), "route first")
	require.NoError(t, err)
	second, err := fixture.CreatePhotoOwner(t.Context(), "route second")
	require.NoError(t, err)
	hash, size, err := fixture.Blobs.Write(strings.NewReader("route photo"))
	require.NoError(t, err)
	photo, err := fixture.CreateFile(store.WithPhotoOwner(t.Context(), first.ID), fixture.RootID(), "route-first.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := fixture.PhotoAssetForNode(store.WithPhotoOwner(t.Context(), first.ID), photo.ID)
	require.NoError(t, err)
	foreign := map[string]string{"X-Docbank-Owner": second.ID}
	for _, path := range []string{
		"/api/v1/photos/assets/" + asset.ID,
		"/api/v1/nodes/" + strconv.FormatInt(photo.ID, 10),
		"/api/v1/nodes/" + strconv.FormatInt(photo.ID, 10) + "/content",
		"/api/v1/nodes/" + strconv.FormatInt(photo.ID, 10) + "/versions",
		"/api/v1/versions/" + photo.CurrentVersionID,
		"/api/v1/versions/" + photo.CurrentVersionID + "/content",
		"/api/v1/path?path=/route-first.jpg",
	} {
		response, body := get(t, ts, path, foreign)
		assert.Equal(t, http.StatusNotFound, response.StatusCode, path+": "+body)
	}
	response, body := get(t, ts, "/api/v1/documents", foreign)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, "route-first.jpg")
	response, body = get(t, ts, "/api/v1/search?q=route-first", foreign)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, "route-first.jpg")
	response, body = get(t, ts, "/api/v1/content-references?sha256="+hash, foreign)
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.NotContains(t, body, photo.CurrentVersionID)
	response, body = do(t, ts, http.MethodPost, "/api/v1/processing/source-fences/resolve", foreign,
		map[string]any{"content_version_ids": []string{photo.CurrentVersionID}})
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
}
