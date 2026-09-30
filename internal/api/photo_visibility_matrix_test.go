package api_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type photoRouteFixture struct {
	ts                              *httptest.Server
	s                               *testStore
	first, second                   store.PhotoOwner
	firstNode, secondNode, ordinary store.Node
	firstAsset                      store.PhotoAsset
	collectionID, tagID             string
}

// newPhotoRouteFixture stores one photo per owner and an ordinary file, all
// with the same bytes, in one collection and under one tag.
func newPhotoRouteFixture(t *testing.T) photoRouteFixture {
	t.Helper()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	f := photoRouteFixture{ts: ts, s: s, first: enrollPhotoOwner(t, s, "First"), second: enrollPhotoOwner(t, s, "Second")}
	hash, size, err := s.Blobs.Write(strings.NewReader("shared photo bytes"))
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "api", "photo matrix")
	require.NoError(t, err)
	f.collectionID = run.ID()
	ingest := func(ctx2 store.PhotoOwner, name, mime string) store.Node {
		ownerCtx := ctx
		if ctx2.ID != "" {
			ownerCtx = store.WithPhotoOwner(ctx, ctx2.ID)
		}
		node, _, err := s.IngestFile(ownerCtx, run, s.RootID(), name, hash, size, mime, "/source/"+name, "")
		require.NoError(t, err)
		return node
	}
	f.firstNode = ingest(f.first, "first.jpg", "image/jpeg")
	f.secondNode = ingest(f.second, "second.jpg", "image/jpeg")
	f.ordinary = ingest(store.PhotoOwner{}, "ordinary.txt", "text/plain")
	f.firstAsset, err = s.PhotoAssetForNode(ctx, f.firstNode.ID)
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "matrix")
	require.NoError(t, err)
	f.tagID = tag.ID
	for _, node := range []store.Node{f.firstNode, f.secondNode, f.ordinary} {
		_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
	}
	for _, node := range []*store.Node{&f.firstNode, &f.secondNode, &f.ordinary} {
		*node, err = s.NodeByID(ctx, node.ID)
		require.NoError(t, err)
	}
	return f
}

// enrollPhotoOwner enrolls a new operator person as a photo owner.
func enrollPhotoOwner(t *testing.T, s *testStore, name string) store.PhotoOwner {
	t.Helper()
	person, err := s.CreatePerson(t.Context(), name, "operator")
	require.NoError(t, err)
	owner, err := s.EnrollPhotoOwner(t.Context(), person.PersonID, person.Revision)
	require.NoError(t, err)
	return owner
}

func ownerHeader(owner store.PhotoOwner) map[string]string {
	return map[string]string{api.PhotoOwnerHeader: owner.ID}
}

func issuePhotoOwnerSession(t *testing.T, ts *httptest.Server, headers map[string]string) map[string]string {
	t.Helper()
	response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", headers, nil)
	require.Equal(t, http.StatusCreated, response.StatusCode, body)
	var session struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &session))
	return map[string]string{"X-Api-Key": "", api.WebSessionHeader: session.Token}
}

// TestPhotoOwnersSeeOnlyTheirPhotos runs each route kind for both owners. A
// listing shows the owner's photo and the ordinary file and never the other
// owner's photo; an addressed read or mutation of the other photo is a 404.
func TestPhotoOwnersSeeOnlyTheirPhotos(t *testing.T) {
	f := newPhotoRouteFixture(t)
	id := func(n store.Node) string { return strconv.FormatInt(n.ID, 10) }
	type answer struct {
		status       int
		code, detail string
	}
	// answerOf keeps what a caller can compare, with the caller-supplied key normalized out of the detail.
	answerOf := func(response *http.Response, body, key string) answer {
		t.Helper()
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &problem), body)
		detail := regexp.MustCompile(`\b`+regexp.QuoteMeta(key)+`\b`).ReplaceAllString(problem.Detail, "@")
		return answer{response.StatusCode, problem.Code, detail}
	}
	missingUUID := "00000000-0000-4000-8000-000000000000"
	listings := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nodes/" + strconv.FormatInt(f.s.RootID(), 10) + "/children"},
		{http.MethodGet, "/api/v1/documents"},
		{http.MethodGet, "/api/v1/search?q=jpg"},
		{http.MethodGet, "/api/v1/tags/" + f.tagID + "/nodes"},
		{http.MethodGet, "/api/v1/collections/" + f.collectionID + "/members"},
		{http.MethodGet, "/api/v1/duplicates"},
		{http.MethodGet, "/api/v1/duplicates/by-hash?sha256=" + f.firstNode.BlobHash + "&size=" + strconv.FormatInt(f.firstNode.Size, 10)},
		{http.MethodGet, "/api/v1/content-references?sha256=" + f.firstNode.BlobHash},
		{http.MethodPost, "/api/v1/workspace/queries"},
	}
	for _, test := range []struct {
		viewer             store.PhotoOwner
		own, other         store.Node
		ownName, otherName string
	}{
		{f.first, f.firstNode, f.secondNode, "first.jpg", "second.jpg"},
		{f.second, f.secondNode, f.firstNode, "second.jpg", "first.jpg"},
	} {
		headers := ownerHeader(test.viewer)
		for _, listing := range listings {
			var body any
			if listing.method == http.MethodPost {
				body = map[string]any{"query": map[string]any{}, "page_size": 50}
			}
			response, text := do(t, f.ts, listing.method, listing.path, headers, body)
			require.Equal(t, http.StatusOK, response.StatusCode, listing.path+": "+text)
			assert.NotContains(t, text, test.otherName, listing.path)
			assert.NotContains(t, text, test.other.CurrentVersionID, listing.path)
		}
		response, text := get(t, f.ts, "/api/v1/collections/"+f.collectionID+"/members", headers)
		require.Equal(t, http.StatusOK, response.StatusCode, text)
		assert.Contains(t, text, `"total":2`, "the member total counts only visible members")
		missingNode := strconv.FormatInt(test.other.ID+100000, 10)
		for _, probe := range []struct{ path, key, missing string }{
			{"/api/v1/nodes/@", id(test.other), missingNode},
			{"/api/v1/nodes/@/content", id(test.other), missingNode},
			{"/api/v1/nodes/@/versions", id(test.other), missingNode},
			{"/api/v1/nodes/@/provenance", id(test.other), missingNode},
			{"/api/v1/nodes/@/tags", id(test.other), missingNode},
			{"/api/v1/versions/@", test.other.CurrentVersionID, missingUUID},
			{"/api/v1/versions/@/content", test.other.CurrentVersionID, missingUUID},
			{"/api/v1/path?path=/@", test.otherName, "missing.jpg"},
			{"/api/v1/photos/nodes/@/asset", id(test.other), missingNode},
		} {
			path := strings.ReplaceAll(probe.path, "@", probe.key)
			response, text := get(t, f.ts, path, headers)
			hidden := answerOf(response, text, probe.key)
			assert.Equal(t, http.StatusNotFound, hidden.status, path+": "+text)
			response, text = get(t, f.ts, strings.ReplaceAll(probe.path, "@", probe.missing), headers)
			assert.Equal(t, answerOf(response, text, probe.missing), hidden, path)
		}
		for _, path := range []string{"/api/v1/nodes/" + id(test.own), "/api/v1/nodes/" + id(f.ordinary)} {
			response, text := get(t, f.ts, path, headers)
			assert.Equal(t, http.StatusOK, response.StatusCode, path+": "+text)
		}
	}

	second := ownerHeader(f.second)
	response, body := get(t, f.ts, "/api/v1/photos/assets/"+f.firstAsset.ID, second)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	ifMatch := func(revision int64) map[string]string {
		return map[string]string{api.PhotoOwnerHeader: f.second.ID, "If-Match": strconv.Quote(strconv.FormatInt(revision, 10))}
	}
	firstID := strconv.FormatInt(f.firstNode.ID, 10)
	pageHash, pageSize, err := f.s.Blobs.Write(strings.NewReader("shared photo bytes"))
	require.NoError(t, err)
	missingID := strconv.FormatInt(f.firstNode.ID+100000, 10)
	nodeID := func(key string) int64 {
		n, err := strconv.ParseInt(key, 10, 64)
		require.NoError(t, err)
		return n
	}
	for _, mutation := range []struct {
		method, path string
		headers      map[string]string
		body         func(key string) any
		key, missing string
	}{
		{http.MethodPost, "/api/v1/photos/assets/@/exclude", ifMatch(f.firstAsset.Revision),
			func(string) any { return map[string]bool{"excluded": true} }, f.firstAsset.ID, missingUUID},
		{http.MethodPost, "/api/v1/photos/assets", second,
			func(key string) any { return map[string]any{"node_id": nodeID(key)} }, firstID, missingID},
		{http.MethodPut, "/api/v1/nodes/@/tags/" + f.tagID, ifMatch(f.firstNode.Revision), nil, firstID, missingID},
		{http.MethodPatch, "/api/v1/nodes/@", ifMatch(f.firstNode.Revision),
			func(string) any { return map[string]string{"dest_path": "/moved.jpg"} }, firstID, missingID},
		{http.MethodPatch, "/api/v1/nodes/@", ifMatch(f.firstNode.Revision),
			func(string) any { return map[string]string{"new_name": "moved.jpg"} }, firstID, missingID},
		{http.MethodPost, "/api/v1/batch/move", second, func(key string) any {
			return map[string]any{"moves": []map[string]any{
				{"node_id": nodeID(key), "revision": f.firstNode.Revision, "destination_path": "/moved.jpg"}}}
		}, firstID, missingID},
		{http.MethodPost, "/api/v1/nodes/@/versions/prune", ifMatch(f.firstNode.Revision),
			func(string) any { return map[string]any{"all_prior": true} }, firstID, missingID},
		{http.MethodPost, "/api/v1/pages/inventory", second, func(key string) any {
			return map[string]any{"selection": store.PageBinding{
				NodeID: nodeID(key), Revision: f.firstNode.Revision,
				Source: document.PageSource{VersionID: f.firstNode.CurrentVersionID, SHA256: pageHash, Size: pageSize}}}
		}, firstID, missingID},
		{http.MethodPost, "/api/v1/nodes/@/trash", ifMatch(f.firstNode.Revision), nil, firstID, missingID}, // last: a leak would change the revision the other probes use
	} {
		send := func(key string) answer {
			var body any
			if mutation.body != nil {
				body = mutation.body(key)
			}
			response, text := do(t, f.ts, mutation.method, strings.ReplaceAll(mutation.path, "@", key), mutation.headers, body)
			return answerOf(response, text, key)
		}
		hidden := send(mutation.key)
		assert.Equal(t, http.StatusNotFound, hidden.status, mutation.path)
		assert.Equal(t, send(mutation.missing), hidden, mutation.path)
	}

	// Promote with If-Match answers a missing node with a stale revision, so
	// another owner's photo must get the same answer.
	promote := func(key string) answer {
		response, body := do(t, f.ts, http.MethodPost, "/api/v1/photos/nodes/"+key+"/promote",
			ifMatch(f.firstAsset.Revision), map[string]any{})
		return answerOf(response, body, key)
	}
	assert.Equal(t, promote(missingID), promote(firstID))

	// Trash lists only the viewer's photo roots.
	for _, node := range []store.Node{f.firstNode, f.secondNode} {
		_, _, err := f.s.Trash(t.Context(), node.ID, node.Revision)
		require.NoError(t, err)
	}
	response, body = get(t, f.ts, "/api/v1/trash", second)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "second.jpg")
	assert.NotContains(t, body, "first.jpg")
	trashed, err := f.s.NodeByID(t.Context(), f.firstNode.ID)
	require.NoError(t, err)
	response, body = do(t, f.ts, http.MethodPost, "/api/v1/nodes/"+firstID+"/restore", ifMatch(trashed.Revision), nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)

	// A purged photo leaves its asset memberless, and the asset stays its owner's.
	_, err = f.s.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	response, body = get(t, f.ts, "/api/v1/photos/assets/"+f.firstAsset.ID, second)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	response, body = get(t, f.ts, "/api/v1/photos/assets/"+f.firstAsset.ID, ownerHeader(f.first))
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
}

func TestPhotoOwnerSessions(t *testing.T) {
	f := newPhotoRouteFixture(t)
	query := map[string]any{"query": map[string]any{}, "page_size": 50}
	workspace := func(headers map[string]string) string {
		t.Helper()
		response, body := do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", headers, query)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		return body
	}

	// A session binds at issuance and cannot switch owners itself.
	firstSession := issuePhotoOwnerSession(t, f.ts, ownerHeader(f.first))
	body := workspace(firstSession)
	assert.Contains(t, body, "first.jpg")
	assert.NotContains(t, body, "second.jpg")
	firstSession[api.PhotoOwnerHeader] = f.second.ID
	response, body := do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", firstSession, query)
	assert.Equal(t, http.StatusForbidden, response.StatusCode, body)
	response, body = do(t, f.ts, http.MethodPost, "/api/daemon/web-session",
		map[string]string{api.PhotoOwnerHeader: "00000000-0000-4000-8000-000000000000"}, nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode, body)

	// Without a header, sessions and master requests use the default owner,
	// the earliest enrollment.
	for _, headers := range []map[string]string{nil, issuePhotoOwnerSession(t, f.ts, nil)} {
		body = workspace(headers)
		assert.Contains(t, body, "first.jpg")
		assert.Contains(t, body, "ordinary.txt")
		assert.NotContains(t, body, "second.jpg")
	}
}

func TestPhotoOwnerSessionInAuditedVault(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/ordinary.txt", "ordinary")
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	session := issuePhotoOwnerSession(t, ts, nil)
	response, body := do(t, ts, http.MethodPost, "/api/v1/workspace/queries", session,
		map[string]any{"query": map[string]any{}, "page_size": 50})
	assert.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "ordinary.txt")
	owners, err := s.PhotoOwners(t.Context())
	require.NoError(t, err)
	assert.Empty(t, owners)
	// Resolving without the mutation gate keeps session issuance open during maintenance.
	ownerID, resolved, err := s.ResolvePhotoOwner(t.Context())
	require.NoError(t, err)
	assert.True(t, resolved)
	assert.Empty(t, ownerID)
}

// TestPhotoVisibilityRouteCoverage fails when a registered operation has no
// photo owner disposition: a two-owner test that covers it, or the reason
// it needs no filter.
func TestPhotoVisibilityRouteCoverage(t *testing.T) {
	_, fixture := newTestServer(t, nil)
	dispositions := map[string][]string{
		"TestPhotoOwnersSeeOnlyTheirPhotos": {
			"listChildren", "listDocuments", "search", "listTagNodes", "listCollectionMembers",
			"listDuplicateContent", "getDuplicateContentByHash", "lookupContentReferences",
			"createWorkspaceQuery", "getNode", "getNodeContent", "listContentVersions",
			"listNodeProvenance", "listNodeTags", "getContentVersion", "getContentVersionBytes",
			"resolvePath", "getPhotoAsset", "getPhotoAssetByNode", "excludePhotoAsset",
			"promotePhotoNode", "createPhotoAsset", "assignTag", "listTrash", "trashNode", "moveNode",
			"batchMove", "pruneNodeContentVersions", "pageInventory", "restoreNode",
		},
		"TestPhotoOwnerSessions": {"createWebSession"},
		"TestPhotoOwnerRoutes": {
			"listPhotoOwners", "enrollPhotoOwner", "removePhotoOwner", "renamePerson", "retirePerson",
			"grantProcessingConsent", "revokeProcessingConsent",
		},
		"store TestPhotoOwnersEnrollment":                              {"mergePerson"},
		"processing TestPackageImportEnrollsPhotosUnderAdmittingOwner": {"createPackageImport", "listPackageMembers"},
		"processing TestConsentBelongsToTheRequestPhotoOwner": {
			"planDocumentProcessing", "grantDocumentProcessingConsent", "revokeDocumentProcessingConsent",
			"startDocumentProcessing", "searchDocuments",
		},
		"applies the visibility rule inline in its own store query": {
			"resolveDocumentSourceFence", "createTermReport", "listTermReportHistory",
		},
		"uses the node, version, path, asset, catalog or query loader of a covered route": {
			"resolveDocumentSummaries", "findSimilarDocuments", "runSavedQuery", "validateDocumentSearch",
			"previewQueryHighlights", "setPhotoDisplay", "attachPhotoFile", "detachPhotoFile",
			"auditNodeHistory", "appendNodeProvenance", "readPageImage", "resolveRenditionText",
			"unassignTag", "assignTagPath", "unassignTagPath", "changeBatchTags", "previewBatchTags",
			"replaceNodeContent", "revertNodeContent", "verifyNodeContent", "movePath", "trashPath",
			"prepareWebDownload", "getDocumentRendition", "readDocumentRenditionBySelector",
			"readDocumentRenditionWindow", "createPageRenderJob", "getPageRenderJob", "cancelPageRenderJob",
		},
		"vault-wide: totals, settings and maintenance return no photo identity": {
			"getPhotoSettings", "setPhotoSettings", "listTags", "getTag", "resolveTagByName", "createTag",
			"renameTag", "deleteTag", "listCollections", "getCollection", "getCollectionLabel",
			"setCollectionLabel", "getCollectionQuality", "getDocumentProcessingCoverage",
			"readTimelineCoverage", "createTimelineRebuild", "readTimelineRebuild", "getPeopleCoverage",
			"rebuildDocumentPeople", "getPeopleRebuild", "listPeople", "emptyTrash", "verify", "gc",
			"vaultInfo", "readFormatCapabilities", "listJobs", "storageStatus", "listBlobStores",
			"registerBlobStore", "previewBlobStoreRegistration", "unregisterBlobStore", "detachBlobStore",
			"startStorageEvacuation", "previewStorageEvacuation", "storagePack", "storageRepack",
			"startStoragePlacement", "previewStoragePlacement", "startStorageRepair",
			"previewStorageRepair", "startStorageSalvage", "previewStorageSalvage",
			"getStorageOperation", "cancelStorageOperation", "initBackupRepository",
			"listBackupSnapshots", "createBackupSnapshot", "streamBackupSnapshotCreation",
			"restoreBackupSnapshot", "streamBackupSnapshotRestore", "verifyBackupRepository",
			"streamBackupRepositoryVerification", "auditStatus", "verifyAudit", "enableAudit",
			"previewAuditEnrollment", "auditScopeHistory", "listDocumentProcessingProfiles",
			"planDerivativePurge", "runDerivativePurge", "parseQuery",
			"listSavedQueries", "getSavedQuery", "createSavedQuery", "updateSavedQuery",
			"deleteSavedQuery", "listPackages", "getPackage", "listPackageFieldCatalog",
			"listPackageLabelCandidates", "listPackageCustodians", "assignPackageCustodian",
			"resolvePackageCustodian", "mkdirPath", "createNode", "listWatchedInboxes",
			"createPerson", "getPerson", "splitPerson",
		},
		"delayed result: serves what it captured under main's resource-owner checks": {
			"readWorkspaceQueryPage", "createExportJob", "getExportJob", "cancelExportJob",
			"downloadExportArchive", "getExportJobEvents", "createExportPlan", "getExportPlan",
			"getExportPlanPreview", "getExportOutputProblems", "createExportSource",
			"getExportAttachmentPublications", "putExportChunk", "getExportEmailPDFRecipes",
			"sealExportSource", "createPackageExport", "getPackageRecord", "listPackageTimelineInputs",
			"readPackageImport", "cancelPackageImport", "getTermReport",
			"reviseTermReport", "getTermReportDates", "issueTermReportDownload",
			"downloadTermReportbundle", "downloadTermReportcsv", "reserveBatesRange",
			"readBatesAllocation", "listBatesExports", "publishBatesExport", "findBatesExports",
			"readBatesExport", "downloadBatesExportContent", "downloadBatesExport", "listBatesNamespaces",
			"createBatesNamespace", "planBatesStamp",
			"getDocumentProcessingJob", "readRenditionText",
			"requestEmailDocumentProcessing", "publishEmailDocuments", "removeEmailDocumentPublication",
			"getEmailDocumentPublication", "listEmailDocumentRelations", "getEmailPDFJob",
			"renderEmailPDF", "listEmailPDFs", "getEmailPDF", "downloadEmailPDF", "getEmailMetadata",
			"ensureEmailMetadata", "getEmailMetadataGeneration", "getEmailPart",
		},
		"ingest and media: new photos take the request owner; media keeps its own principal": {
			"ingest", "preflightIngest", "streamIngest", "uploadFile", "createPackagePreflight",
			"readPackagePreflight", "readPackagePreflightDiagnostics", "beginPackageContainer",
			"abortPackageContainer", "getPackageContainer", "uploadPackageChunk",
			"preflightPackageContainer", "sealPackageContainer", "registerMailboxArchive",
			"beginMailboxContainer", "abortMailboxContainer", "getMailboxContainer",
			"uploadMailboxChunk", "previewMailboxContainer", "sealMailboxContainer", "listMailboxJobs",
			"beginMailboxJob", "getMailboxJob", "cancelMailboxJob", "mailboxEvents",
			"mailboxOccurrences", "resumeMailboxJob", "transferMailboxEML", "planMediaAcquisition",
			"grantMediaAcquisitionConsent", "revokeMediaAcquisitionConsent", "listMediaOccurrences",
			"declareMediaOccurrence", "revokeMediaOccurrence", "listMediaOrigins", "listMediaSources",
			"submitMediaSource", "getMediaSource", "importMediaArtifact", "retryMediaSource",
		},
		"session-owned: browser session and download lifecycle": {"revokeWebSession", "cancelWebDownload"},
	}
	expected := map[string]string{}
	for disposition, operations := range dispositions {
		for _, operationID := range operations {
			_, duplicate := expected[operationID]
			require.False(t, duplicate, operationID+" has two dispositions")
			expected[operationID] = disposition
		}
	}
	var unlisted []string
	for _, item := range fixture.Server.API().OpenAPI().Paths {
		for _, operation := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if operation == nil {
				continue
			}
			if _, ok := expected[operation.OperationID]; !ok {
				unlisted = append(unlisted, operation.OperationID)
			}
			delete(expected, operation.OperationID)
		}
	}
	assert.Empty(t, unlisted, "registered operations need a photo owner disposition")
	assert.Empty(t, expected, "dispositions name unregistered operations")
}
