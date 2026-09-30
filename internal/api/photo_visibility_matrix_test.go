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
	ts                                 *httptest.Server
	s                                  *testStore
	alice, bob                         store.PhotoOwner
	aliceNode, bobNode, receipt, beach store.Node
	aliceAsset                         store.PhotoAsset
	collectionID, tagID                string
}

// newPhotoRouteFixture gives Alice and Bob a photo in their own folders and
// stores /taxes/receipt.jpg and /photos/shared/beach.jpg outside both, all
// with the same bytes, in one collection and under one tag.
func newPhotoRouteFixture(t *testing.T, mutate func(*api.Deps)) photoRouteFixture {
	t.Helper()
	ts, s := newTestServer(t, mutate)
	ctx := t.Context()
	f := photoRouteFixture{ts: ts, s: s, alice: enrollPhotoOwner(t, s, "Alice"), bob: enrollPhotoOwner(t, s, "Bob")}
	hash, size, err := s.Blobs.Write(strings.NewReader("shared photo bytes"))
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "api", "photo matrix")
	require.NoError(t, err)
	f.collectionID = run.ID()
	ingest := func(dir, name string) store.Node {
		parent, err := s.MkdirAll(ctx, dir)
		require.NoError(t, err)
		node, _, err := s.IngestFile(ctx, run, parent.ID, name, hash, size, "image/jpeg", "/source/"+name, "")
		require.NoError(t, err)
		return node
	}
	f.aliceNode = ingest("/photos/"+f.alice.ID, "alice.jpg")
	f.bobNode = ingest("/photos/"+f.bob.ID, "bob.jpg")
	f.receipt = ingest("/taxes", "receipt.jpg")
	f.beach = ingest("/photos/shared", "beach.jpg")
	f.aliceAsset, err = s.PhotoAssetForNode(ctx, f.aliceNode.ID)
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "matrix")
	require.NoError(t, err)
	f.tagID = tag.ID
	for _, node := range []*store.Node{&f.aliceNode, &f.bobNode, &f.receipt, &f.beach} {
		_, err = s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
		require.NoError(t, err)
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

// createFileIn stores a text file in dir, creating dir as needed.
func createFileIn(t *testing.T, s *testStore, dir, name, content string) store.Node {
	t.Helper()
	parent, err := s.MkdirAll(t.Context(), dir)
	require.NoError(t, err)
	hash, size, err := s.Blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), parent.ID, name, hash, size, "text/plain")
	require.NoError(t, err)
	return node
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

type photoAnswer struct {
	status       int
	code, detail string
}

// photoAnswerOf keeps what a caller can compare, with the caller-supplied key normalized out of the detail.
func photoAnswerOf(t *testing.T, response *http.Response, body, key string) photoAnswer {
	t.Helper()
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &problem), body)
	detail := regexp.MustCompile(`\b`+regexp.QuoteMeta(key)+`\b`).ReplaceAllString(problem.Detail, "@")
	return photoAnswer{response.StatusCode, problem.Code, detail}
}

const missingPhotoUUID = "00000000-0000-4000-8000-000000000000"

// TestPhotoOwnersSeeOnlyTheirPhotos runs each route kind as Bob against
// Alice's folder: listings leave it out, and addressing it answers like a
// missing node.
func TestPhotoOwnersSeeOnlyTheirPhotos(t *testing.T) {
	f := newPhotoRouteFixture(t, nil)
	id := func(n store.Node) string { return strconv.FormatInt(n.ID, 10) }
	bob := ownerHeader(f.bob)
	missingNode := strconv.FormatInt(f.aliceNode.ID+100000, 10)
	photos, err := f.s.NodeByPath(t.Context(), "/photos")
	require.NoError(t, err)

	for _, probe := range []struct{ name, path, key, missing string }{
		{"get_node", "/api/v1/nodes/@", id(f.aliceNode), missingNode},
		{"get_node_content", "/api/v1/nodes/@/content", id(f.aliceNode), missingNode},
		{"list_versions", "/api/v1/nodes/@/versions", id(f.aliceNode), missingNode},
		{"list_provenance", "/api/v1/nodes/@/provenance", id(f.aliceNode), missingNode},
		{"node_tags", "/api/v1/nodes/@/tags", id(f.aliceNode), missingNode},
		{"get_version", "/api/v1/versions/@", f.aliceNode.CurrentVersionID, missingPhotoUUID},
		{"get_version_bytes", "/api/v1/versions/@/content", f.aliceNode.CurrentVersionID, missingPhotoUUID},
		{"resolve_path", "/api/v1/path?path=/photos/@", f.alice.ID, missingPhotoUUID},
		{"photo_asset", "/api/v1/photos/assets/@", f.aliceAsset.ID, missingPhotoUUID},
		{"photo_asset_by_node", "/api/v1/photos/nodes/@/asset", id(f.aliceNode), missingNode},
	} {
		t.Run(probe.name, func(t *testing.T) {
			response, text := get(t, f.ts, strings.ReplaceAll(probe.path, "@", probe.key), bob)
			hidden := photoAnswerOf(t, response, text, probe.key)
			require.Equal(t, http.StatusNotFound, hidden.status, text)
			response, text = get(t, f.ts, strings.ReplaceAll(probe.path, "@", probe.missing), bob)
			require.Equal(t, photoAnswerOf(t, response, text, probe.missing), hidden)
			t.Logf("hidden answer: %+v", hidden)
		})
	}

	t.Run("children", func(t *testing.T) {
		for _, test := range []struct {
			headers map[string]string
			names   []string
		}{
			{bob, []string{f.bob.ID, "shared"}},
			{nil, []string{f.alice.ID, f.bob.ID, "shared"}},
		} {
			response, text := get(t, f.ts, "/api/v1/nodes/"+id(photos)+"/children", test.headers)
			require.Equal(t, http.StatusOK, response.StatusCode, text)
			var page api.NodePage
			require.NoError(t, json.Unmarshal([]byte(text), &page))
			var names []string
			for _, item := range page.Items {
				names = append(names, item.Name)
			}
			assert.ElementsMatch(t, test.names, names)
			assert.Equal(t, len(test.names), page.Total)
			t.Logf("total=%d", page.Total)
		}
	})

	t.Run("listings", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/documents", "/api/v1/tags/" + f.tagID + "/nodes",
			"/api/v1/collections/" + f.collectionID + "/members", "/api/v1/duplicates",
			"/api/v1/duplicates/by-hash?sha256=" + f.aliceNode.BlobHash + "&size=" + strconv.FormatInt(f.aliceNode.Size, 10),
			"/api/v1/content-references?sha256=" + f.aliceNode.BlobHash,
		} {
			response, text := get(t, f.ts, path, bob)
			require.Equal(t, http.StatusOK, response.StatusCode, path+": "+text)
			assert.NotContains(t, text, "alice.jpg", path)
			assert.NotContains(t, text, f.aliceNode.CurrentVersionID, path)
			assert.Contains(t, text, "receipt.jpg", path)
		}
		response, text := get(t, f.ts, "/api/v1/collections/"+f.collectionID+"/members", bob)
		require.Equal(t, http.StatusOK, response.StatusCode, text)
		assert.Contains(t, text, `"total":3`, "the member total counts only visible members")
	})

	t.Run("search", func(t *testing.T) {
		for _, headers := range []map[string]string{bob, ownerHeader(f.alice)} {
			response, text := get(t, f.ts, "/api/v1/search?q=jpg", headers)
			require.Equal(t, http.StatusOK, response.StatusCode, text)
			assert.Contains(t, text, "receipt.jpg")
			assert.Contains(t, text, "beach.jpg")
			if headers[api.PhotoOwnerHeader] == f.bob.ID {
				assert.NotContains(t, text, "alice.jpg")
			} else {
				assert.NotContains(t, text, "bob.jpg")
			}
		}
	})

	t.Run("workspace_queries", func(t *testing.T) {
		query := map[string]any{"query": map[string]any{}, "page_size": 50, "facets": []string{"extension"}}
		for _, test := range []struct {
			headers map[string]string
			total   int64
		}{{bob, 3}, {nil, 4}} {
			response, text := do(t, f.ts, http.MethodPost, "/api/v1/workspace/queries", test.headers, query)
			require.Equal(t, http.StatusOK, response.StatusCode, text)
			var result api.WorkspaceQueryResponse
			require.NoError(t, json.Unmarshal([]byte(text), &result))
			assert.Equal(t, test.total, result.Total)
			require.Len(t, result.Facets, 1)
			var jpg int64
			for _, value := range result.Facets[0].Values {
				if value.Key == "jpg" {
					jpg = value.Count
				}
			}
			assert.Equal(t, test.total, jpg, "the extension facet counts only visible files")
			t.Logf("total=%d jpg_facet=%d", result.Total, jpg)
		}
	})

	t.Run("tag_counts", func(t *testing.T) {
		for _, test := range []struct {
			headers map[string]string
			count   int
		}{{bob, 3}, {nil, 4}} {
			response, text := get(t, f.ts, "/api/v1/tags", test.headers)
			require.Equal(t, http.StatusOK, response.StatusCode, text)
			var page api.TagPage
			require.NoError(t, json.Unmarshal([]byte(text), &page))
			require.Len(t, page.Items, 1)
			assert.Equal(t, test.count, page.Items[0].AssignmentCount)
			t.Logf("assignment_count=%d", page.Items[0].AssignmentCount)
		}
	})

	t.Run("outside_owner_folders", func(t *testing.T) {
		for _, headers := range []map[string]string{bob, ownerHeader(f.alice)} {
			for _, node := range []store.Node{f.receipt, f.beach} {
				response, text := get(t, f.ts, "/api/v1/nodes/"+id(node), headers)
				assert.Equal(t, http.StatusOK, response.StatusCode, text)
			}
		}
	})

	t.Run("master_without_owner", func(t *testing.T) {
		response, master := get(t, f.ts, "/api/v1/nodes/"+id(f.aliceNode)+"/versions", nil)
		require.Equal(t, http.StatusOK, response.StatusCode, master)
		response, owner := get(t, f.ts, "/api/v1/nodes/"+id(f.aliceNode)+"/versions", ownerHeader(f.alice))
		require.Equal(t, http.StatusOK, response.StatusCode, owner)
		assert.Equal(t, owner, master)
	})

	t.Run("mutations", func(t *testing.T) {
		ifMatch := func(revision int64) map[string]string {
			return map[string]string{api.PhotoOwnerHeader: f.bob.ID, "If-Match": strconv.Quote(strconv.FormatInt(revision, 10))}
		}
		aliceID := id(f.aliceNode)
		pageHash, pageSize, err := f.s.Blobs.Write(strings.NewReader("shared photo bytes"))
		require.NoError(t, err)
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
			{http.MethodPost, "/api/v1/photos/assets/@/exclude", ifMatch(f.aliceAsset.Revision),
				func(string) any { return map[string]bool{"excluded": true} }, f.aliceAsset.ID, missingPhotoUUID},
			{http.MethodPost, "/api/v1/photos/assets", bob,
				func(key string) any { return map[string]any{"node_id": nodeID(key)} }, aliceID, missingNode},
			{http.MethodPut, "/api/v1/nodes/@/tags/" + f.tagID, ifMatch(f.aliceNode.Revision), nil, aliceID, missingNode},
			{http.MethodPatch, "/api/v1/nodes/@", ifMatch(f.aliceNode.Revision),
				func(string) any { return map[string]string{"dest_path": "/moved.jpg"} }, aliceID, missingNode},
			{http.MethodPatch, "/api/v1/nodes/@", ifMatch(f.aliceNode.Revision),
				func(string) any { return map[string]string{"new_name": "moved.jpg"} }, aliceID, missingNode},
			{http.MethodPost, "/api/v1/nodes/@/versions/prune", ifMatch(f.aliceNode.Revision),
				func(string) any { return map[string]any{"all_prior": true} }, aliceID, missingNode},
			{http.MethodPost, "/api/v1/pages/inventory", bob, func(key string) any {
				return map[string]any{"selection": store.PageBinding{
					NodeID: nodeID(key), Revision: f.aliceNode.Revision,
					Source: document.PageSource{VersionID: f.aliceNode.CurrentVersionID, SHA256: pageHash, Size: pageSize}}}
			}, aliceID, missingNode},
			{http.MethodPost, "/api/v1/nodes/@/trash", ifMatch(f.aliceNode.Revision), nil, aliceID, missingNode}, // last: a leak would change the revision the other probes use
		} {
			send := func(key string) photoAnswer {
				var body any
				if mutation.body != nil {
					body = mutation.body(key)
				}
				response, text := do(t, f.ts, mutation.method, strings.ReplaceAll(mutation.path, "@", key), mutation.headers, body)
				return photoAnswerOf(t, response, text, key)
			}
			hidden := send(mutation.key)
			assert.Equal(t, http.StatusNotFound, hidden.status, mutation.path)
			assert.Equal(t, send(mutation.missing), hidden, mutation.path)
		}

		// Promote with If-Match answers a missing node with a stale revision, so
		// another owner's photo must get the same answer.
		promote := func(key string) photoAnswer {
			response, body := do(t, f.ts, http.MethodPost, "/api/v1/photos/nodes/"+key+"/promote",
				ifMatch(f.aliceAsset.Revision), map[string]any{})
			return photoAnswerOf(t, response, body, key)
		}
		assert.Equal(t, promote(missingNode), promote(aliceID))
	})
}

func TestPhotoOwnerSessions(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileIn(t, s, "/photos", "early.txt", "early")
	query := map[string]any{"query": map[string]any{}, "page_size": 50}
	workspace := func(headers map[string]string) (*http.Response, string) {
		t.Helper()
		return do(t, ts, http.MethodPost, "/api/v1/workspace/queries", headers, query)
	}

	// Before any owner is enrolled the vault is single-user.
	early := issuePhotoOwnerSession(t, ts, nil)
	response, body := workspace(early)
	require.Equal(t, http.StatusOK, response.StatusCode, body)
	assert.Contains(t, body, "early.txt")

	enroll := func(name string) store.PhotoOwner {
		person, err := s.CreatePerson(t.Context(), name, "operator")
		require.NoError(t, err)
		response, body := do(t, ts, http.MethodPost, "/api/v1/photos/owners",
			map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(person.Revision, 10))},
			map[string]string{"person_id": person.PersonID})
		require.Equal(t, http.StatusCreated, response.StatusCode, body)
		var owner api.PhotoOwner
		require.NoError(t, json.Unmarshal([]byte(body), &owner))
		return store.PhotoOwner{ID: owner.ID, Name: owner.Name}
	}
	alice, bob := enroll("Alice"), enroll("Bob")
	createFileIn(t, s, "/photos/"+alice.ID, "alice.txt", "alice")
	createFileIn(t, s, "/photos/"+bob.ID, "bob.txt", "bob")
	t.Run("ownerless_session_revoked", func(t *testing.T) {
		response, body := workspace(early)
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode, body)
	})
	t.Run("ownerless_issuance_refused", func(t *testing.T) {
		response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", nil, nil)
		require.Equal(t, http.StatusUnprocessableEntity, response.StatusCode, body)
		assert.Contains(t, body, `"code":"validation"`)
		assert.Contains(t, body, "docbank web --owner")
	})
	t.Run("unknown_owner", func(t *testing.T) {
		response, body := do(t, ts, http.MethodPost, "/api/daemon/web-session", map[string]string{api.PhotoOwnerHeader: missingPhotoUUID}, nil)
		assert.Equal(t, http.StatusNotFound, response.StatusCode, body)
	})
	t.Run("session_binds_owner", func(t *testing.T) {
		session := issuePhotoOwnerSession(t, ts, ownerHeader(alice))
		response, body := workspace(session)
		require.Equal(t, http.StatusOK, response.StatusCode, body)
		assert.Contains(t, body, "alice.txt")
		assert.Contains(t, body, "early.txt")
		assert.NotContains(t, body, "bob.txt")
		session[api.PhotoOwnerHeader] = bob.ID
		response, body = workspace(session)
		assert.Equal(t, http.StatusForbidden, response.StatusCode, body)
	})
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
			"promotePhotoNode", "createPhotoAsset", "assignTag", "moveNode",
			"pruneNodeContentVersions", "pageInventory", "listTags",
		},
		"TestPhotoOwnerFolderTrash":         {"listTrash", "trashNode", "restoreNode"},
		"TestPhotoOwnerFolderWrites":        {"mkdirPath"},
		"TestPhotoOwnerTotals":              {"getCollectionQuality", "getDocumentProcessingCoverage"},
		"TestPhotoImportStaysInOwnerFolder": {"startPhotoImport", "listPhotoImports", "getPhotoImport", "cancelPhotoImport", "listJobs"},
		"TestPhotoOwnerSessions":            {"createWebSession"},
		"TestPhotoOwnerRoutes": {
			"listPhotoOwners", "enrollPhotoOwner", "removePhotoOwner", "renamePerson", "retirePerson",
			"grantProcessingConsent", "revokeProcessingConsent",
		},
		"store TestPhotoOwnersEnrollment": {"mergePerson"},
		"processing TestConsentBelongsToTheRequestPhotoOwner": {
			"planDocumentProcessing", "grantDocumentProcessingConsent", "revokeDocumentProcessingConsent",
			"startDocumentProcessing", "searchDocuments",
		},
		"applies the visibility rule inline in its own store query": {
			"resolveDocumentSourceFence", "createTermReport", "listTermReportHistory", "listPackageMembers",
		},
		"uses the node, version, path, asset, catalog or query loader of a covered route": {
			"resolveDocumentSummaries", "findSimilarDocuments", "runSavedQuery", "validateDocumentSearch",
			"previewQueryHighlights", "setPhotoDisplay", "attachPhotoFile", "detachPhotoFile",
			"auditNodeHistory", "appendNodeProvenance", "readPageImage", "resolveRenditionText",
			"unassignTag", "assignTagPath", "unassignTagPath", "changeBatchTags", "previewBatchTags",
			"replaceNodeContent", "revertNodeContent", "verifyNodeContent", "movePath", "trashPath",
			"prepareWebDownload", "getDocumentRendition", "readDocumentRenditionBySelector",
			"readDocumentRenditionWindow", "createPageRenderJob", "getPageRenderJob", "cancelPageRenderJob",
			"createPackageImport", "createNode",
		},
		"master only: sessions cannot move, and the master key moves between owners' folders": {"batchMove"},
		"master only: not session-reachable, so the master key reads vault figures": {
			"readTimelineCoverage", "getPeopleCoverage",
		},
		"no node dimension: deduplicated blob bytes, extractor capabilities, or the audit ledger of vaults that refuse enrollment": {
			"storageStatus", "readFormatCapabilities", "auditStatus",
		},
		"vault-wide: settings and maintenance return no photo identity": {
			"getPhotoSettings", "setPhotoSettings", "getTag", "resolveTagByName", "createTag",
			"renameTag", "deleteTag", "listCollections", "getCollection", "getCollectionLabel",
			"setCollectionLabel", "createTimelineRebuild", "readTimelineRebuild",
			"rebuildDocumentPeople", "getPeopleRebuild", "listPeople", "emptyTrash", "verify", "gc",
			"vaultInfo", "listBlobStores",
			"registerBlobStore", "previewBlobStoreRegistration", "unregisterBlobStore", "detachBlobStore",
			"startStorageEvacuation", "previewStorageEvacuation", "storagePack", "storageRepack",
			"startStoragePlacement", "previewStoragePlacement", "startStorageRepair",
			"previewStorageRepair", "startStorageSalvage", "previewStorageSalvage",
			"getStorageOperation", "cancelStorageOperation", "initBackupRepository",
			"listBackupSnapshots", "createBackupSnapshot", "streamBackupSnapshotCreation",
			"restoreBackupSnapshot", "streamBackupSnapshotRestore", "verifyBackupRepository",
			"streamBackupRepositoryVerification", "verifyAudit", "enableAudit",
			"previewAuditEnrollment", "auditScopeHistory", "listDocumentProcessingProfiles",
			"planDerivativePurge", "runDerivativePurge", "parseQuery",
			"listSavedQueries", "getSavedQuery", "createSavedQuery", "updateSavedQuery",
			"deleteSavedQuery", "listPackages", "getPackage", "listPackageFieldCatalog",
			"listPackageLabelCandidates", "listPackageCustodians", "assignPackageCustodian",
			"resolvePackageCustodian", "listWatchedInboxes",
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
		"ingest and media: writes land where the request's loader resolves; media keeps its own principal": {
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
