package api_test

import (
	"encoding/json/v2"
	"fmt"
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
	response := srv.post(t, "/api/v1/packages/preflights", string(rawBody))
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
	unknown := srv.post(t, "/api/v1/packages/preflights", `{"profile":"dat-concordance-v1","encoding":"utf-8","source_kind":"root","source_ref":"/tmp","surprise":true}`)
	assert.Equal(t, 422, unknown.Code)
	oversize := srv.post(t, "/api/v1/packages/preflights", `{"profile":"`+strings.Repeat("x", (1<<20)+1)+`"}`)
	assert.Equal(t, 413, oversize.Code)
	assert.Equal(t, 0, tableCounts(t, store).packagePreflights)
}

func TestPreflightReportsBlockingDiagnosticsWithoutRefusingTheRequest(t *testing.T) {
	srv, _ := newPackageTestServer(t)
	response := srv.post(t, "/api/v1/packages/containers/pkg-bad/preflight", blockingPreflightBody(t))
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
		response := srv.post(t, "/api/v1/packages/preflights", string(rawBody))
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
	response := srv.post(t, "/api/v1/packages/preflights", string(rawBody))
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
	response := srv.post(t, "/api/v1/packages/preflights", string(rawBody))
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
	response := srv.post(t, "/api/v1/packages/preflights", string(rawBody))
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
	response := srv.post(t, "/api/v1/packages/preflights", string(rawBody))
	require.Equal(t, 200, response.Code, response.Body.String())
	var out api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	assert.False(t, out.Blocking)
	require.Equal(t, []api.PackageVolume{{Ordinal: 1, VolumeName: "DISC001", DeclaredRoot: "DISC001"}}, out.Volumes)
}
