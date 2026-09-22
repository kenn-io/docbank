package api_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/processing"
	storepkg "go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
	"golang.org/x/text/encoding/unicode"
)

func TestPackageExportIssuesOneUseVerifiedArchive(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	node := createFileWithChecksum(t, catalog, "synthetic.txt", "synthetic package export")
	occurrence := strings.Repeat("d", 32)
	snapshot, err := catalog.SealCollectionSnapshot(t.Context(), storepkg.SnapshotSealRequest{
		SnapshotID: uuid.NewString(), Members: []storepkg.CollectionSnapshotMember{{
			Ordinal: 1, OccurrenceID: occurrence, NodeID: node.ID, ContentVersionID: node.CurrentVersionID,
			BlobSHA256: node.BlobHash, Size: node.Size, FamilyID: occurrence, FamilyOrder: 1,
			DisplayName: node.Name, FrozenFieldsJSON: "{}", DocumentKind: "other",
		}},
	})
	require.NoError(t, err)
	created := srv.call(t, http.MethodPost, "/api/v1/packages/exports", mustPackageJSON(t, api.PackageExportRequest{
		SnapshotID: snapshot.SnapshotID, ProfileID: "export-csv-natives-v1",
	}), nil)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	var ticket api.PackageExportTicket
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &ticket))
	require.Equal(t, 1, ticket.Records)
	require.Equal(t, snapshot.SnapshotID, ticket.SnapshotID)

	response, body := do(t, srv.ts, http.MethodGet, ticket.URL, nil, nil)
	require.Equal(t, http.StatusOK, response.StatusCode)
	verified, err := processing.VerifyLoadFileExport(t.Context(), bytes.NewReader([]byte(body)), int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, ticket.ArchiveSHA256, verified.Receipt.ArchiveSHA256)
	second, _ := do(t, srv.ts, http.MethodGet, ticket.URL, nil, nil)
	require.Equal(t, http.StatusNotFound, second.StatusCode)
}

func TestPackageImportAdmitsFrozenPreflightAndReplaysOperation(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	previewResponse := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{
		Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
	}))
	require.Equal(t, http.StatusOK, previewResponse.Code, previewResponse.Body.String())
	var preview api.PackagePreflight
	require.NoError(t, json.Unmarshal(previewResponse.Body.Bytes(), &preview))
	require.False(t, preview.Blocking)
	request := api.PackageImportRequest{
		PreflightID: preview.PreflightID, Into: "/", Name: "synthetic-package",
		OperationID: uuid.NewString(), IndexSuppliedText: true,
	}
	first := srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t, request), nil)
	require.Equal(t, http.StatusAccepted, first.Code, first.Body.String())
	var job api.PackageImportJob
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &job))
	require.Equal(t, request.OperationID, job.OperationID)
	require.Equal(t, preview.PreflightID, job.PreflightID)
	require.Equal(t, preview.Records, job.Total)
	require.Equal(t, "queued", job.State)
	require.NotEmpty(t, job.PackageID)
	stored, err := catalog.PackageImportJob(t.Context(), "vault:"+catalog.VaultID(), request.OperationID)
	require.NoError(t, err)
	require.Equal(t, job.JobID, stored.ID)
	read := srv.get(t, "/api/v1/packages/imports/"+request.OperationID)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	browserRead := srv.call(t, http.MethodGet, "/api/v1/packages/imports/"+request.OperationID, "",
		map[string]string{"X-Api-Key": "", api.WebSessionHeader: packageBrowserToken(t, srv)})
	require.Equal(t, http.StatusNotFound, browserRead.Code, "a browser session cannot read the master owner's import")
	replay := srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t, request), nil)
	require.Equal(t, http.StatusAccepted, replay.Code, replay.Body.String())
	require.JSONEq(t, first.Body.String(), replay.Body.String())
	request.Name = "changed-package"
	conflict := srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t, request), nil)
	require.Equal(t, http.StatusConflict, conflict.Code, conflict.Body.String())
	cancelled := srv.call(t, http.MethodPost, "/api/v1/packages/imports/"+job.OperationID+"/cancel", "", nil)
	require.Equal(t, http.StatusOK, cancelled.Code, cancelled.Body.String())
	var stopped api.PackageImportJob
	require.NoError(t, json.Unmarshal(cancelled.Body.Bytes(), &stopped))
	require.Equal(t, "cancelled", stopped.State)
	require.Equal(t, job.JobID, stopped.JobID)
}

func TestPackagePreflightRejectsObjectAboveIngestBound(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	path := filepath.Join(root, "VOL001", "oversized.pdf")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(1<<32+1))
	require.NoError(t, file.Close())
	response := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{
		Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
	}))
	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())
	require.Zero(t, tableCounts(t, catalog).packagePreflights)
}

func TestPackagePreflightRejectsRecordAboveReceiptBound(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	var header, row []string
	header = append(header, "DOCID", "NATIVE")
	row = append(row, "DOC-A", "NATIVES/DOC-A.pdf")
	for index := range 14 {
		header = append(header, fmt.Sprintf("EXTRA%02d", index))
		row = append(row, strings.Repeat("\"", 64<<10))
	}
	line := func(fields []string) string { return "þ" + strings.Join(fields, "þ\x14þ") + "þ\r\n" }
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "ab-package.dat"),
		[]byte(line(header)+line(row)), 0o600))
	response := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{
		Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
	}))
	require.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())
	require.Zero(t, tableCounts(t, catalog).packagePreflights)
}

func TestSealedZIPPackagePreflightRetainsContainerBinding(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	require.NoError(t, filepath.Walk(root, func(name string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		entry, err := writer.Create(filepath.ToSlash(rel))
		if err != nil {
			return fmt.Errorf("create ZIP fixture entry: %w", err)
		}
		content, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		_, err = entry.Write(content)
		return err
	}))
	require.NoError(t, writer.Close())
	digest := sha256.Sum256(archive.Bytes())
	containerID := "synthetic-load-file-zip"
	created := srv.call(t, http.MethodPost, "/api/v1/packages/containers", mustPackageJSON(t, map[string]any{
		"container_id": containerID, "sha256": hex.EncodeToString(digest[:]), "size": archive.Len(),
	}), nil)
	require.Equal(t, 201, created.Code, created.Body.String())
	upload, err := http.NewRequest(http.MethodPut, srv.ts.URL+"/api/v1/packages/containers/"+containerID+"/chunks/0", bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	upload.Header.Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
	upload.Header.Set(api.BlobSizeHeader, strconv.Itoa(archive.Len()))
	response, err := srv.ts.Client().Do(upload)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 200, response.StatusCode)
	sealed := srv.call(t, http.MethodPost, "/api/v1/packages/containers/"+containerID+"/seal", "", nil)
	require.Equal(t, 200, sealed.Code, sealed.Body.String())
	request := api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "container", SourceRef: containerID}
	preview := srv.call(t, http.MethodPost, "/api/v1/packages/containers/"+containerID+"/preflight", mustPackageJSON(t, request), nil)
	require.Equal(t, 200, preview.Code, preview.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(preview.Body.Bytes(), &out))
	require.Equal(t, "container", out.SourceKind)
	require.Equal(t, 2, out.Records)
	require.Equal(t, 3, out.Pages)
	require.NotContains(t, out.SourceRef, root)
	record, err := catalog.PackagePreflight(t.Context(), "vault:"+catalog.VaultID(), out.PreflightID)
	require.NoError(t, err)
	require.Equal(t, containerID, record.SourceLocator)
	require.NotEmpty(t, record.ProfileJSON)
	require.NotEmpty(t, record.MappingJSON)
	read := srv.get(t, "/api/v1/packages/preflights/"+out.PreflightID)
	require.Equal(t, 200, read.Code, read.Body.String())
}

func TestPreflightPersistsOneExpiringRowAndMutatesNothingElse(t *testing.T) {
	srv, store := newPackageTestServer(t)
	before := tableCounts(t, store)
	root := syntheticPackageRoot(t)
	rawBody, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
	require.NoError(t, err)
	response := srv.post(t, string(rawBody))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.True(t, canonical.IsSHA256Hex(out.ManifestSHA256))
	assert.NotEmpty(t, out.SourceRef)
	assert.NotContains(t, out.SourceRef, string(os.PathSeparator))
	assert.Equal(t, 2, out.Records)
	assert.Equal(t, 3, out.Pages)
	after := tableCounts(t, store)
	assert.Equal(t, before.nodes, after.nodes)
	assert.Equal(t, before.contentVersions, after.contentVersions)
	assert.Equal(t, before.blobs+2, after.blobs, "only staged manifest and diagnostics blobs are added")
	assert.Equal(t, 1, after.packagePreflights)
	assert.NotEmpty(t, out.ExpiresAt)
	read := srv.get(t, "/api/v1/packages/preflights/"+out.PreflightID)
	require.Equal(t, 200, read.Code, read.Body.String())
	driver := storepkg.DefaultSQLiteDriver()
	db, err := driver.Open(store.DBPath, sqlite.OpenOptions{Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var locator, profileJSON, mappingJSON string
	require.NoError(t, db.QueryRow(`SELECT source_locator,profile_json,mapping_json FROM package_preflights WHERE preflight_id=?`, out.PreflightID).Scan(&locator, &profileJSON, &mappingJSON))
	assert.Equal(t, root, locator)
	assert.NotEmpty(t, profileJSON)
	assert.NotEmpty(t, mappingJSON)
}

func TestPreflightRejectsUnknownMembersAndOversizeBodies(t *testing.T) {
	srv, store := newPackageTestServer(t)
	unknown := srv.post(t, `{"profile":"dat-concordance-v1","encoding":"utf-8","source_kind":"root","source_ref":"/tmp","surprise":true}`)
	assert.Equal(t, 422, unknown.Code)
	oversize := srv.post(t, `{"profile":"`+strings.Repeat("x", (1<<20)+1)+`"}`)
	assert.Equal(t, 413, oversize.Code)
	assert.Equal(t, 0, tableCounts(t, store).packagePreflights)
}

func TestPreflightRejectsEmptySource(t *testing.T) {
	root := syntheticPackageRoot(t)
	t.Chdir(root)
	srv, store := newPackageTestServer(t)
	for _, source := range []string{"", `,"source_ref":""`} {
		response := srv.post(t, `{"profile":"dat-concordance-v1","encoding":"utf-8","source_kind":"root"`+source+`}`)
		require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "source_ref")
	}
	assert.Zero(t, tableCounts(t, store).packagePreflights)
}

func TestPreflightSelectsOPTPageCountProfile(t *testing.T) {
	root := syntheticPackageRoot(t)
	pageMap := "DOC-A,VOL001,IMAGES\\001\\DOC-A-1.tif,Y,2,,\r\nDOC-A,VOL001,IMAGES\\001\\DOC-A-2.tif,,,,\r\nDOC-B,VOL001,IMAGES\\001\\DOC-B-1.tif,Y,1,,\r\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "ab-package.opt"), []byte(pageMap), 0o600))
	srv, _ := newPackageTestServer(t)
	body, err := json.Marshal(map[string]string{
		"profile": "dat-concordance-v1", "encoding": "utf-8", "source_kind": "root", "source_ref": root,
		"page_map_profile": "opt-pagecount5-v1",
	})
	require.NoError(t, err)
	response := srv.post(t, string(body))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.False(t, out.Blocking, "%+v", out.Diagnostics)
	assert.Equal(t, 3, out.Pages)
	for _, profile := range []string{"unknown", "dat-concordance-v1", "lfp-ipro-v1"} {
		response = srv.post(t, strings.ReplaceAll(string(body), "opt-pagecount5-v1", profile))
		require.Equal(t, http.StatusUnprocessableEntity, response.Code, response.Body.String())
	}
}

func TestPreflightReportsBlockingDiagnosticsWithoutRefusingTheRequest(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	response := srv.post(t, blockingPreflightBody(t))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.True(t, out.Blocking)
	assert.NotEmpty(t, out.Diagnostics)
	assert.Equal(t, len(out.Diagnostics), out.DiagnosticCount)
}

func TestPreflightManifestBindsSameSizeSourceByteChanges(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	request := func() api.PackagePreflight {
		rawBody, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
		require.NoError(t, err)
		response := srv.post(t, string(rawBody))
		require.Equal(t, 200, response.Code, response.Body.String())
		var out api.PackagePreflight
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
		return out
	}
	before := request()
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "IMAGES", "001", "DOC-A-1.tif"), []byte("synthetic-z1"), 0o600))
	after := request()
	assert.Equal(t, before.SourceRef, after.SourceRef, "root inventory digest intentionally binds metadata")
	assert.NotEqual(t, before.ManifestSHA256, after.ManifestSHA256, "manifest must bind retained source bytes")
}

func TestPreflightDiagnosticsAreCountedAndPaged(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	var opt strings.Builder
	for index := range 300 {
		breakFlag := ""
		if index == 0 {
			breakFlag = "Y"
		}
		_, _ = fmt.Fprintf(&opt, "DOC-A,VOL001,MISSING\\PAGE-%03d.tif,%s,,,\r\n", index, breakFlag)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "ab-package.opt"), []byte(opt.String()), 0o600))
	rawBody, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
	require.NoError(t, err)
	response := srv.post(t, string(rawBody))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.Equal(t, 300, out.DiagnosticCount)
	assert.Len(t, out.Diagnostics, 250)

	first := srv.get(t, "/api/v1/packages/preflights/"+out.PreflightID+"/diagnostics?limit=100")
	require.Equal(t, 200, first.Code, first.Body.String())
	var page api.PackageDiagnosticPage
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &page))
	assert.Equal(t, 300, page.Total)
	assert.Len(t, page.Diagnostics, 100)
	require.NotEmpty(t, page.NextCursor)
	for _, cursor := range []string{"!invalid", "b3RoZXI6MA"} {
		invalid := srv.get(t, "/api/v1/packages/preflights/"+out.PreflightID+"/diagnostics?cursor="+cursor)
		assert.Equal(t, 422, invalid.Code, invalid.Body.String())
	}
	second := srv.get(t, "/api/v1/packages/preflights/"+out.PreflightID+"/diagnostics?limit=100&cursor="+page.NextCursor)
	require.Equal(t, 200, second.Code, second.Body.String())
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &page))
	assert.Len(t, page.Diagnostics, 100)
}

func TestPreflightAppliesConfirmedMappingAndNestedVolumeRoot(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(root, "DELIVERY"), 0o700))
	require.NoError(t, os.Rename(filepath.Join(root, "VOL001"), filepath.Join(root, "DELIVERY", "VOL001")))
	documentID, native := "loadfile.document.id", "loadfile.file.native"
	zero, one := 0, 1
	mapping, err := json.Marshal(loadfile.Mapping{
		Contract: loadfile.MappingContractV1,
		Columns: []loadfile.MappingColumn{
			{Source: "DOCID", SourceOrdinal: &zero, Canonical: &documentID},
			{Source: "NATIVE", SourceOrdinal: &one, Canonical: &native},
		},
		VolumeRoots: map[string]string{"VOL001": "DELIVERY/VOL001"},
	})
	require.NoError(t, err)
	rawBody, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root, Mapping: mapping})
	require.NoError(t, err)
	response := srv.post(t, string(rawBody))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.False(t, out.Blocking)
	require.Equal(t, []api.PackageVolume{{Ordinal: 1, VolumeName: "VOL001", DeclaredRoot: "VOL001"}}, out.Volumes)
}

func TestPreflightCountsPDFPagesThroughTheConfinedHandle(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	optPath := filepath.Join(root, "VOL001", "DATA", "ab-package.opt")
	rawOPT, err := os.ReadFile(optPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(optPath, []byte(strings.Replace(string(rawOPT), ",Y,,,2", ",Y,,,3", 1)), 0o600))
	rawBody, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
	require.NoError(t, err)
	response := srv.post(t, string(rawBody))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	require.True(t, out.Blocking)
	var mismatch *api.PackageDiagnostic
	for index := range out.Diagnostics {
		if out.Diagnostics[index].Code == "page_count_mismatch" {
			mismatch = &out.Diagnostics[index]
			break
		}
	}
	require.NotNil(t, mismatch)
	assert.Equal(t, "declared 3 pages; source has 2", mismatch.Detail)
	assert.NotEmpty(t, mismatch.RowID)
}

func TestPreflightUsesSingleDeclaredVolumeWithoutNameHeuristics(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	require.NoError(t, os.Rename(filepath.Join(root, "VOL001"), filepath.Join(root, "DISC001")))
	optPath := filepath.Join(root, "DISC001", "DATA", "ab-package.opt")
	rawOPT, err := os.ReadFile(optPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(optPath, []byte(strings.ReplaceAll(string(rawOPT), "VOL001", "DISC001")), 0o600))
	rawBody, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
	require.NoError(t, err)
	response := srv.post(t, string(rawBody))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.False(t, out.Blocking)
	require.Equal(t, []api.PackageVolume{{Ordinal: 1, VolumeName: "DISC001", DeclaredRoot: "DISC001"}}, out.Volumes)
}

func TestPreflightReportsCorrectableInputErrors(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T) string
		status  int
		code    string
	}{
		{"missing root", func(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "missing") }, 422, "package_reference_unsafe"},
		{"malformed DAT", func(t *testing.T) string {
			t.Helper()
			root := syntheticPackageRoot(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "ab-package.dat"), []byte("þDOCIDþ\r\nþunfinished"), 0o600))
			return root
		}, 422, "invalid_package_data"},
		{"inventory limit", func(t *testing.T) string {
			t.Helper()
			root := syntheticPackageRoot(t)
			for i := range 65 {
				volume := filepath.Join(root, fmt.Sprintf("EXTRA%03d", i))
				require.NoError(t, os.Mkdir(volume, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(volume, "source.txt"), []byte("synthetic"), 0o600))
			}
			return root
		}, 413, "package_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.prepare(t)
			body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
			require.NoError(t, err)
			response := srv.post(t, string(body))
			require.Equal(t, tc.status, response.Code, response.Body.String())
			var problem api.Error
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
			assert.Equal(t, tc.code, problem.Code)
			assert.NotContains(t, problem.Detail, root)
		})
	}
}

func TestPreflightRejectsIncompleteVolumeMapping(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL002"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL002", "source.txt"), []byte("second volume"), 0o600))
	mapping, err := json.Marshal(loadfile.Mapping{Contract: loadfile.MappingContractV1, VolumeRoots: map[string]string{"VOL001": "VOL001"}})
	require.NoError(t, err)
	body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root, Mapping: mapping})
	require.NoError(t, err)
	response := srv.post(t, string(body))
	require.Equal(t, 422, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "VOL002")
}

func TestPreflightReadsLFPPageMap(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	require.NoError(t, os.Remove(filepath.Join(root, "VOL001", "DATA", "ab-package.opt")))
	lfp := "## synthetic page map\nIM,DOC-A,D,0,@VOL001;IMAGES\\001;DOC-A-1.tif;2,0\nIM,DOC-A,,0,@VOL001;IMAGES\\001;DOC-A-2.tif;2,0\nIM,DOC-B,D,0,@VOL001;IMAGES\\001;DOC-B-1.tif;2,90\n"
	path := filepath.Join(root, "VOL001", "DATA", "ab-package.lfp")
	require.NoError(t, os.WriteFile(path, []byte(lfp), 0o600))
	body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
	require.NoError(t, err)
	response := srv.post(t, string(body))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.Equal(t, 3, out.Pages)
	assert.False(t, out.Blocking)
	require.NoError(t, os.WriteFile(path, []byte("VN,VOL001\n"), 0o600))
	response = srv.post(t, string(body))
	assert.Equal(t, 422, response.Code, response.Body.String())
}

func TestPreflightRequiresFirstPageDocumentBoundary(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	for extension, pageMap := range map[string]string{
		"opt": "DOC-A,VOL001,IMAGES\\001\\DOC-A-1.tif,,,,\nDOC-B,VOL001,IMAGES\\001\\DOC-B-1.tif,Y,,,1\n",
		"lfp": "IM,DOC-A,,0,@VOL001;IMAGES\\001;DOC-A-1.tif;2,0\nIM,DOC-B,D,0,@VOL001;IMAGES\\001;DOC-B-1.tif;2,0\n",
	} {
		t.Run(extension, func(t *testing.T) {
			root := syntheticPackageRoot(t)
			require.NoError(t, os.Remove(filepath.Join(root, "VOL001", "DATA", "ab-package.opt")))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "ab-package."+extension), []byte(pageMap), 0o600))
			body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
			require.NoError(t, err)
			response := srv.post(t, string(body))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var out api.PackagePreflight
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
			assert.True(t, out.Blocking)
			require.Len(t, out.Diagnostics, 1)
			assert.Equal(t, "image_boundary_missing", out.Diagnostics[0].Code)
			assert.Equal(t, "blocking", out.Diagnostics[0].Severity)
			assert.Equal(t, "DOC-A", out.Diagnostics[0].RowID)
			assert.Equal(t, 1, out.Diagnostics[0].RowOrdinal)
		})
	}
}

func TestPreflightHashesExactEncodedLoadFileBytes(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	for extension, pageMap := range map[string]string{
		"opt": "DOC-A,VOL001,IMAGES\\001\\DOC-A-1.tif,Y,,,2\r\n",
		"lfp": "## synthetic page map\r\nIM,DOC-A,D,0,@VOL001;IMAGES\\001;DOC-A-1.tif;2,0\r\n",
	} {
		t.Run(extension, func(t *testing.T) {
			root := syntheticPackageRoot(t)
			require.NoError(t, os.Remove(filepath.Join(root, "VOL001", "DATA", "ab-package.opt")))
			rawFiles := map[string][]byte{
				"DATA/ab-package.dat":          []byte("DOCID\x14NOTE\r\nDOC-A\x14café\r\n"),
				"DATA/ab-package." + extension: []byte(pageMap),
			}
			for name, source := range rawFiles {
				encoded, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes(source)
				require.NoError(t, err)
				rawFiles[name] = encoded
				require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", filepath.FromSlash(name)), encoded, 0o600))
			}
			body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-16le", SourceKind: "root", SourceRef: root})
			require.NoError(t, err)
			response := srv.post(t, string(body))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var out api.PackagePreflight
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
			require.False(t, out.Blocking, "%+v", out.Diagnostics)
			manifest, err := catalog.Blobs.OpenContext(t.Context(), out.ManifestSHA256)
			require.NoError(t, err)
			defer func() { require.NoError(t, manifest.Close()) }()
			scanner := bufio.NewScanner(manifest)
			found := 0
			for scanner.Scan() {
				var item struct {
					Kind  string         `json:"kind"`
					Value jsontext.Value `json:"value"`
				}
				require.NoError(t, json.Unmarshal(scanner.Bytes(), &item))
				if item.Kind != "file" {
					continue
				}
				var ref loadfile.FileRef
				require.NoError(t, json.Unmarshal(item.Value, &ref))
				if ref.Role != "raw_load_file" {
					continue
				}
				found++
				source, exists := rawFiles[ref.RelPath]
				require.True(t, exists)
				assert.Equal(t, "VOL001", ref.Volume)
				assert.Equal(t, int64(len(source)), ref.Size)
				assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(source)), ref.SHA256)
			}
			require.NoError(t, scanner.Err())
			assert.Equal(t, 2, found)
		})
	}
}

func TestPreflightCancellationDoesNotPersistReceipt(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: syntheticPackageRoot(t)})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/packages/preflights", strings.NewReader(string(body)))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Api-Key", testAPIKey)
	response := httptest.NewRecorder()
	srv.ts.Config.Handler.ServeHTTP(response, request)
	assert.Contains(t, response.Body.String(), "context canceled")
	assert.Zero(t, tableCounts(t, catalog).packagePreflights)
}

func TestPreflightBlocksEveryRecordWithoutDocumentID(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	for header, values := range map[string]string{"DOCID": "þþ\nþþ\n", "UNMAPPED": "SOURCE-A\nSOURCE-B\n"} {
		t.Run(header, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "package.dat"), []byte(header+"\n"+values), 0o600))
			body, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
			require.NoError(t, err)
			response := srv.post(t, string(body))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var out api.PackagePreflight
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
			assert.True(t, out.Blocking)
			require.Len(t, out.Diagnostics, 2)
			for i, diagnostic := range out.Diagnostics {
				assert.Equal(t, "missing_document_id", diagnostic.Code)
				assert.Equal(t, "blocking", diagnostic.Severity)
				assert.Equal(t, "package.dat", diagnostic.LoadFile)
				assert.Equal(t, i+2, diagnostic.RowOrdinal)
				assert.NotEmpty(t, diagnostic.RowID)
			}
			assert.NotEqual(t, out.Diagnostics[0].RowID, out.Diagnostics[1].RowID)
		})
	}
}

func TestPreflightUsesCSVNormalizationForQuotedMultilineFields(t *testing.T) {
	srv, catalog := newPackageTestServer(t)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
	source := "DOCID,NOTE\r\nDOC-A,\"first line\r\nsecond line, \"\"quoted\"\"\"\r\nDOC-B,plain\r\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "package.dat"), []byte(source), 0o600))
	body, err := json.Marshal(api.PackagePreflightRequest{Profile: "csv-rfc4180-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root})
	require.NoError(t, err)
	response := srv.post(t, string(body))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	require.False(t, out.Blocking, "%+v", out.Diagnostics)
	require.Equal(t, 2, out.Records)
	manifest, err := catalog.Blobs.OpenContext(t.Context(), out.ManifestSHA256)
	require.NoError(t, err)
	defer func() { require.NoError(t, manifest.Close()) }()
	var records []loadfile.Record
	scanner := bufio.NewScanner(manifest)
	for scanner.Scan() {
		var item struct {
			Kind  string          `json:"kind"`
			Value loadfile.Record `json:"value"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &item))
		if item.Kind == "record" {
			records = append(records, item.Value)
		}
	}
	require.NoError(t, scanner.Err())
	require.Len(t, records, 2)
	assert.Equal(t, "DOC-A", records[0].DocID)
	require.Len(t, records[0].Fields, 2)
	assert.Equal(t, "first line\nsecond line, \"quoted\"", records[0].Fields[1].Raw)
	assert.Equal(t, "DOC-B", records[1].DocID)
	require.Len(t, records[1].Fields, 2)
	assert.Equal(t, "plain", records[1].Fields[1].Raw)
}
