package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
)

func seedBatesSnapshot(t *testing.T, s *testStore) (store.CollectionSnapshot, []api.BatesPageInput) {
	t.Helper()
	db, err := store.DefaultSQLiteDriver().Open(s.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var members []store.CollectionSnapshotMember
	var pages []api.BatesPageInput
	for i, count := range []int{2, 1} {
		name := []string{"A.pdf", "B.pdf"}[i]
		hash := strings.Repeat([]string{"a", "b"}[i], 64)
		node, err := s.CreateFile(t.Context(), s.RootID(), name, hash, 123, "application/pdf")
		require.NoError(t, err)
		source := document.PageSource{VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: 123}
		frames := make([]document.PageFrameV1, count)
		for p := range frames {
			frames[p], err = document.NewPDFPageFrame(source, p+1, [4]float64{0, 0, 72, 72}, [4]float64{0, 0, 72, 72}, 0)
			require.NoError(t, err)
		}
		doc := document.PageDocumentV1{Contract: document.PageFrameContractV1, Source: source, PageCount: count, Frames: frames}
		encoded, checksum, err := document.MarshalPageDocumentV1(doc)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `INSERT INTO page_documents(version_id,canonical_json,checksum) VALUES(?,?,?)`, source.VersionID, encoded, checksum)
		require.NoError(t, err)
		for _, frame := range frames {
			frameJSON, frameChecksum, err := document.MarshalPageFrameV1(frame)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO page_frames(version_id,page,canonical_json,checksum) VALUES(?,?,?,?)`,
				source.VersionID, frame.Page, frameJSON, frameChecksum)
			require.NoError(t, err)
		}
		member := store.CollectionSnapshotMember{Ordinal: i + 1, OccurrenceID: strings.Repeat([]string{"c", "d"}[i], 32),
			NodeID: node.ID, ContentVersionID: node.CurrentVersionID, BlobSHA256: node.BlobHash, Size: 123,
			FamilyID: strings.Repeat("c", 32), FamilyOrder: i + 1, DisplayName: name,
			FrozenFieldsJSON: "{}", DocumentKind: "other", SourcePageCount: count, SelectedPDFSHA256: node.BlobHash}
		if i == 1 {
			member.ParentOccurrenceID = members[0].OccurrenceID
		}
		members = append(members, member)
		for p := 1; p <= count; p++ {
			pages = append(pages, api.BatesPageInput{OccurrenceID: member.OccurrenceID, UnstampedSHA256: node.BlobHash, SourcePage: p, VerifiedPageCount: count})
		}
	}
	snapshot, err := s.SealCollectionSnapshot(t.Context(), store.SnapshotSealRequest{SnapshotID: uuid.NewString(), Members: members})
	require.NoError(t, err)
	return snapshot, pages
}

func TestBatesPlanPreviewsWithoutStampingAnything(t *testing.T) {
	srv, s := newPackageTestServer(t)
	snapshot, pages := seedBatesSnapshot(t, s)
	nsBody, err := json.Marshal(api.BatesNamespaceRequest{Prefix: "OUR", Padding: 6})
	require.NoError(t, err)
	created := srv.call(t, http.MethodPost, "/api/v1/bates/namespaces", string(nsBody), nil)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var ns api.BatesNamespace
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &ns))
	request := api.BatesPlanRequest{NamespaceID: ns.NamespaceID, SnapshotID: snapshot.SnapshotID,
		Prefix: "OUR", Padding: 6, StartAt: 41, Pages: pages}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	before := tableCounts(t, s)
	first := srv.call(t, http.MethodPost, "/api/v1/bates/preview", string(body), nil)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var plan api.BatesPlan
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &plan))
	require.True(t, plan.StampedNothing)
	require.Empty(t, plan.AllocationID)
	require.NotContains(t, first.Body.String(), "allocation_id")
	require.Equal(t, "OUR000041", plan.Labels[0].Label)
	second := srv.call(t, http.MethodPost, "/api/v1/bates/preview", string(body), nil)
	require.Equal(t, first.Body.String(), second.Body.String())
	implicit := request
	implicit.Pages = nil
	implicit.Prefix = ""
	implicit.Padding = 0
	implicitBody, err := json.Marshal(implicit)
	require.NoError(t, err)
	derived := srv.call(t, http.MethodPost, "/api/v1/bates/preview", string(implicitBody), nil)
	require.Equal(t, first.Body.String(), derived.Body.String())
	require.Equal(t, before.blobs, tableCounts(t, s).blobs)

	recipe := batesTestRecipe(ns, 41)
	reserve := api.BatesReserveRequest{OperationID: uuid.NewString(), SnapshotID: snapshot.SnapshotID, Recipe: recipe, Pages: pages}
	reserveBody, err := json.Marshal(reserve)
	require.NoError(t, err)
	reserved := srv.call(t, http.MethodPost, "/api/v1/bates/allocations", string(reserveBody), nil)
	require.Equal(t, http.StatusCreated, reserved.Code, reserved.Body.String())
	var allocation api.BatesAllocation
	require.NoError(t, json.Unmarshal(reserved.Body.Bytes(), &allocation))
	require.Equal(t, "OUR000041", allocation.Labels[0].Label)
	digest, err := recipe.SHA256()
	require.NoError(t, err)
	require.Equal(t, digest, allocation.RecipeSHA256, "the daemon derives the digest from the recipe")
	replay := srv.call(t, http.MethodPost, "/api/v1/bates/allocations", string(reserveBody), nil)
	require.Equal(t, allocation.AllocationID, decodeBatesAllocation(t, replay.Body.Bytes()).AllocationID)
	read := srv.get(t, "/api/v1/bates/allocations/"+allocation.AllocationID)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	require.Equal(t, allocation.AllocationID, decodeBatesAllocation(t, read.Body.Bytes()).AllocationID)
	for name, change := range map[string]func(*api.BatesReserveRequest){
		"incomplete recipe":   func(r *api.BatesReserveRequest) { r.Recipe.Contract = "" },
		"mismatched padding":  func(r *api.BatesReserveRequest) { r.Recipe.Padding = 7 },
		"mismatched prefix":   func(r *api.BatesReserveRequest) { r.Recipe.Prefix = "OTHER" },
		"restamping a source": func(r *api.BatesReserveRequest) { r.Recipe.Restamp = true },
	} {
		invalid := reserve
		invalid.OperationID = uuid.NewString()
		change(&invalid)
		invalidBody, err := json.Marshal(invalid)
		require.NoError(t, err)
		rejected := srv.call(t, http.MethodPost, "/api/v1/bates/allocations", string(invalidBody), nil)
		require.Equal(t, http.StatusUnprocessableEntity, rejected.Code, "%s: %s", name, rejected.Body.String())
		require.Contains(t, rejected.Body.String(), "invalid_bates_request", name)
	}
	reserve.Recipe = batesTestRecipe(ns, 42)
	changed, err := json.Marshal(reserve)
	require.NoError(t, err)
	conflict := srv.call(t, http.MethodPost, "/api/v1/bates/allocations", string(changed), nil)
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	require.Contains(t, conflict.Body.String(), "bates_reservation_conflict")

	request.StartAt = 0
	request.Pages = append([]api.BatesPageInput(nil), pages...)
	request.Pages[0].SourcePage = 2
	wrongPages, err := json.Marshal(request)
	require.NoError(t, err)
	mismatch := srv.call(t, http.MethodPost, "/api/v1/bates/preview", string(wrongPages), nil)
	require.Equal(t, http.StatusConflict, mismatch.Code, mismatch.Body.String())
	require.Contains(t, mismatch.Body.String(), "bates_page_count_mismatch")
	shortNamespace, err := json.Marshal(api.BatesNamespaceRequest{Prefix: "OVR", Padding: 1})
	require.NoError(t, err)
	shortCreated := srv.call(t, http.MethodPost, "/api/v1/bates/namespaces", string(shortNamespace), nil)
	require.Equal(t, http.StatusCreated, shortCreated.Code, shortCreated.Body.String())
	var short api.BatesNamespace
	require.NoError(t, json.Unmarshal(shortCreated.Body.Bytes(), &short))
	overflowBody, err := json.Marshal(api.BatesReserveRequest{OperationID: uuid.NewString(),
		SnapshotID: snapshot.SnapshotID, Recipe: batesTestRecipe(short, 9), Pages: pages})
	require.NoError(t, err)
	overflow := srv.call(t, http.MethodPost, "/api/v1/bates/allocations", string(overflowBody), nil)
	require.Equal(t, http.StatusUnprocessableEntity, overflow.Code, overflow.Body.String())
	require.Contains(t, overflow.Body.String(), "bates_overflow")
	list := srv.get(t, "/api/v1/bates/namespaces?limit=1")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var page api.BatesNamespacePage
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &page))
	require.Equal(t, 2, page.Total)
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.NextCursor)
	next := srv.get(t, "/api/v1/bates/namespaces?limit=1&cursor="+page.NextCursor)
	require.Equal(t, http.StatusOK, next.Code, next.Body.String())
	page = api.BatesNamespacePage{}
	require.NoError(t, json.Unmarshal(next.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	require.Empty(t, page.NextCursor)
}

func batesTestRecipe(namespace api.BatesNamespace, start int) pdfstamp.Recipe {
	return pdfstamp.Recipe{Contract: pdfstamp.RecipeContractV1, NamespaceID: namespace.NamespaceID,
		Prefix: namespace.Prefix, Suffix: namespace.Suffix, Padding: namespace.Padding, StartAt: start,
		Position: "bottom-right", MarginPoints: 24, FontName: "Helvetica", FontSizePoints: 9, Color: "#000000",
		Opacity: 1, Units: "point", RotationPolicy: "follow_page", EngineIdentity: pdfstamp.EngineIdentity{
			Name: "pdfcpu", Version: "v0.15.0", API: "AddWatermarksMap", Options: []string{"onTop=true", "update=restamp"}}}
}

func decodeBatesAllocation(t *testing.T, body []byte) api.BatesAllocation {
	t.Helper()
	var allocation api.BatesAllocation
	require.NoError(t, json.Unmarshal(body, &allocation))
	return allocation
}

func TestBatesExportRouteRejectsAnUnboundRun(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	response := srv.call(t, http.MethodPost, "/api/v1/bates/exports", `{}`, nil)
	require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "validation")
}

func TestBatesDownloadAcceptsBrowserEmptyObjectBody(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	response := srv.call(t, http.MethodPost,
		"/api/v1/bates/exports/11111111-1111-4111-8111-111111111111/download", `{}`, nil)
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
}

func TestBatesExportHistoryRejectsInvalidAndUnknownCursors(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	invalid := srv.get(t, "/api/v1/bates/exports?after=not-a-cursor&limit=1")
	require.Equal(t, http.StatusUnprocessableEntity, invalid.Code, invalid.Body.String())
	require.Contains(t, invalid.Body.String(), `"invalid_bates_cursor"`)
	unknown := srv.get(t, "/api/v1/bates/exports?after=11111111-1111-4111-8111-111111111111&limit=1")
	require.Equal(t, http.StatusUnprocessableEntity, unknown.Code, unknown.Body.String())
	require.Contains(t, unknown.Body.String(), `"invalid_bates_cursor"`)
}

func TestBatesInputErrorsAreValidationFailures(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	cursor := srv.get(t, "/api/v1/bates/namespaces?cursor=not-a-uuid")
	require.Equal(t, http.StatusUnprocessableEntity, cursor.Code, cursor.Body.String())
	require.Contains(t, cursor.Body.String(), `"invalid_bates_cursor"`)
	prefix := srv.call(t, http.MethodPost, "/api/v1/bates/namespaces", `{"prefix":"BAD%","padding":6}`, nil)
	require.Equal(t, http.StatusUnprocessableEntity, prefix.Code, prefix.Body.String())
	require.Contains(t, prefix.Body.String(), `"invalid_bates_request"`)
}

func TestBatesCandidateRouteRequiresOneSelectorAndReturnsBoundedCandidates(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	missing := srv.get(t, "/api/v1/bates/exports/candidates?limit=10")
	require.Equal(t, http.StatusUnprocessableEntity, missing.Code, missing.Body.String())
	multiple := srv.get(t, "/api/v1/bates/exports/candidates?bates_label=A&custodian_label=B&limit=10")
	require.Equal(t, http.StatusUnprocessableEntity, multiple.Code, multiple.Body.String())
	found := srv.get(t, "/api/v1/bates/exports/candidates?bates_label=NONE000001&limit=10")
	require.Equal(t, http.StatusOK, found.Code, found.Body.String())
	var page api.BatesCandidatePage
	require.NoError(t, json.Unmarshal(found.Body.Bytes(), &page))
	require.Empty(t, page.Items)
}
