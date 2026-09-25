package api_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPackageImportIndexesSuppliedTextWithOrWithoutNative(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		native bool
		index  bool
	}{
		{name: "text-only-unindexed"},
		{name: "text-only-indexed", index: true},
		{name: "native-with-supplied-text", native: true, index: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			srv, catalog := newPackageTestServer(t)
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
			dat := "þDOCIDþ\x14þTEXTþ\r\nþDOC-Aþ\x14þA.txtþ\r\n"
			if testCase.native {
				dat = "þDOCIDþ\x14þTEXTþ\x14þNATIVEþ\r\nþDOC-Aþ\x14þA.txtþ\x14þA.native.txtþ\r\n"
				require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "A.native.txt"), []byte("synthetic native bytes"), 0o600))
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "records.dat"), []byte(dat), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "A.txt"), []byte("quenchwood sender text\n"), 0o600))
			response := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{
				Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
			}))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var preview api.PackagePreflight
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &preview))
			require.False(t, preview.Blocking, "%+v", preview.Diagnostics)
			admitted := srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t, api.PackageImportRequest{
				PreflightID: preview.PreflightID, Into: "/", Name: "synthetic-package",
				OperationID: uuid.NewString(), IndexSuppliedText: testCase.index,
			}), nil)
			require.Equal(t, http.StatusAccepted, admitted.Code, admitted.Body.String())
			var job api.PackageImportJob
			require.NoError(t, json.Unmarshal(admitted.Body.Bytes(), &job))
			worker, err := processing.NewPackageImportWorker(processing.PackageImportConfig{
				Catalog: catalog.Store, Blobs: catalog.Blobs, Owner: "synthetic-worker",
			})
			require.NoError(t, err)
			_, err = worker.ProcessOnce(t.Context())
			require.NoError(t, err)
			pkg, err := catalog.Package(t.Context(), job.PackageID)
			require.NoError(t, err)
			require.Equal(t, "complete", pkg.State)
			members, err := catalog.SnapshotMembers(t.Context(), pkg.SnapshotID, 0, 10)
			require.NoError(t, err)
			require.Len(t, members, 1)
			var supplied *store.CollectionSnapshotRepresentation
			for index := range members[0].Representations {
				if members[0].Representations[index].Role == "supplied_text" {
					supplied = &members[0].Representations[index]
					break
				}
			}
			require.NotNil(t, supplied)
			if testCase.native {
				require.NotEqual(t, members[0].ContentVersionID, supplied.ContentVersionID)
			} else {
				require.Equal(t, members[0].ContentVersionID, supplied.ContentVersionID)
			}
			search := srv.get(t, "/api/v1/search?q=quenchwood&limit=10")
			require.Equal(t, http.StatusOK, search.Code, search.Body.String())
			var report api.SearchReport
			require.NoError(t, json.Unmarshal(search.Body.Bytes(), &report))
			if testCase.index {
				require.Equal(t, "supplied", supplied.TextAuthority)
				require.NotEmpty(t, supplied.LexicalGenerationID)
				require.Len(t, report.Hits, 1)
				require.Equal(t, members[0].ContentVersionID, report.Hits[0].Node.CurrentVersionID)
				require.Equal(t, store.SearchMatchContent, report.Hits[0].Match)
			} else {
				require.Equal(t, "none", supplied.TextAuthority)
				require.Empty(t, supplied.LexicalGenerationID)
				require.Empty(t, report.Hits)
			}
			require.NoError(t, catalog.ValidateMetadata(t.Context()))
		})
	}
}

func TestPackageImportTerminalFailureRemovesOnlyUnreceiptedDocuments(t *testing.T) {
	t.Parallel()
	srv, catalog := newPackageTestServer(t)
	ctx := t.Context()
	root := t.TempDir()
	volume := filepath.Join(root, "VOL001")
	require.NoError(t, os.Mkdir(volume, 0o700))
	const nativeB = "Synthetic native document B\n"
	const suppliedB = "Synthetic supplied text B\n"
	files := map[string]string{
		"A.txt":          "Synthetic native document A\n",
		"A-supplied.txt": "Synthetic supplied text A\n",
		"B.bin":          nativeB,
		"B-supplied.txt": suppliedB,
		"records.dat": "þDOCIDþ\x14þNATIVEþ\x14þTEXTþ\r\n" +
			"þDOC-Aþ\x14þA.txtþ\x14þA-supplied.txtþ\r\n" +
			"þDOC-Bþ\x14þB.binþ\x14þB-supplied.txtþ\r\n",
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(volume, name), []byte(content), 0o600))
	}
	shared := createFileWithContent(t, srv.ts, catalog, "/shared.txt", nativeB)
	response := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{
		Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: root,
	}))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var preview api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &preview))
	require.False(t, preview.Blocking, "%+v", preview.Diagnostics)
	request := api.PackageImportRequest{
		PreflightID: preview.PreflightID, Into: "/", Name: "synthetic-package",
		OperationID: uuid.NewString(), IndexSuppliedText: true,
	}
	admitted := srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t, request), nil)
	require.Equal(t, http.StatusAccepted, admitted.Code, admitted.Body.String())
	var job api.PackageImportJob
	require.NoError(t, json.Unmarshal(admitted.Body.Bytes(), &job))
	worker, err := processing.NewPackageImportWorker(processing.PackageImportConfig{
		Catalog: catalog.Store, Blobs: catalog.Blobs, Owner: "synthetic-worker",
	})
	require.NoError(t, err)
	_, err = worker.ProcessOnce(ctx)
	// Both B representations are staged before supplied-text indexing rejects
	// the unsupported native type. No worker error is injected by this test.
	require.ErrorContains(t, err, "unsupported native media type")
	statusResponse := srv.get(t, "/api/v1/packages/imports/"+request.OperationID)
	require.Equal(t, http.StatusOK, statusResponse.Code, statusResponse.Body.String())
	var status api.PackageImportJob
	require.NoError(t, json.Unmarshal(statusResponse.Body.Bytes(), &status))
	require.Equal(t, "failed", status.State)
	require.Equal(t, 1, status.Committed)
	pkg, err := catalog.Package(ctx, job.PackageID)
	require.NoError(t, err)
	require.Equal(t, "failed", pkg.State)
	require.Empty(t, pkg.SnapshotID)

	secondKey, err := store.PackageRecordKey("records.dat", 3, "DOC-B")
	require.NoError(t, err)
	_, err = catalog.PackageImportHead(ctx, job.PackageID, secondKey)
	require.ErrorIs(t, err, store.ErrNotFound)
	occurrence := store.PackageOccurrenceID(job.PackageID, secondKey)
	for _, suffix := range []string{"-00-native.bin", "-01-supplied_text.txt"} {
		_, err = catalog.NodeByPath(ctx, "/"+job.PackageID+"/"+occurrence+suffix)
		require.ErrorIs(t, err, store.ErrNotFound, "unreceipted documents must be removed")
	}
	unreachable, err := catalog.UnreachableBlobs(ctx)
	require.NoError(t, err)
	require.Contains(t, unreachable, store.BlobInfo{Hash: testHash(suppliedB), Size: int64(len(suppliedB))},
		"unreceipted versions must no longer keep unique bytes live")

	firstKey, err := store.PackageRecordKey("records.dat", 2, "DOC-A")
	require.NoError(t, err)
	first, err := catalog.PackageImportHead(ctx, job.PackageID, firstKey)
	require.NoError(t, err)
	require.Equal(t, "committed", first.State)
	var receipt struct {
		Member *store.CollectionSnapshotMember `json:"member"`
	}
	require.NoError(t, json.Unmarshal(first.ReceiptJSON, &receipt))
	require.NotNil(t, receipt.Member)
	for _, rep := range receipt.Member.Representations {
		if rep.Status == "available" {
			version, err := catalog.ContentVersionByID(ctx, rep.ContentVersionID)
			require.NoError(t, err, "committed representations must survive")
			_, err = catalog.NodeByID(ctx, version.NodeID)
			require.NoError(t, err)
		}
	}
	gotShared, err := catalog.NodeByPath(ctx, "/shared.txt")
	require.NoError(t, err)
	require.Equal(t, shared.ID, gotShared.ID)
	require.Equal(t, shared.CurrentVersionID, gotShared.CurrentVersionID)
	require.Equal(t, shared.Revision, gotShared.Revision)
	stream, _, err := catalog.Blobs.OpenStreamContext(ctx, shared.BlobHash)
	require.NoError(t, err)
	content, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Equal(t, nativeB, string(content))
	require.NoError(t, catalog.ValidateMetadata(ctx))
}
