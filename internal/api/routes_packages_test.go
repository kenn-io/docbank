package api_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
)

func TestPreflightPersistsOneExpiringRowAndMutatesNothingElse(t *testing.T) {
	srv, store := newPackageTestServer(t)
	before := tableCounts(t, store)
	rawBody, err := json.Marshal(api.PackagePreflightRequest{Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: syntheticPackageRoot(t)})
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
