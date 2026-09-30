package api_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/pdfstamp"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestPackageExportProfilesThroughPublicPreflightAndImport(t *testing.T) {
	t.Parallel()
	source, catalog := newPackageTestServer(t)
	root := syntheticPackageRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "ab-package.dat"), []byte(
		"DOCID\x14PARENTID\x14NATIVE\x14PDF\x14BEGBATES\x14ENDBATES\n"+
			"REC001\x14\x14NATIVES/DOC-A.pdf\x14NATIVES/DOC-A.pdf\x14REC001\x14REC002\n"+
			"REC003\x14REC001\x14NATIVES/DOC-B.pdf\x14NATIVES/DOC-B.pdf\x14REC003\x14REC003\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "DATA", "ab-package.opt"), []byte(
		"REC001,VOL001,IMAGES/001/DOC-A-1.tif,Y,,,2\n"+
			"REC002,VOL001,IMAGES/001/DOC-A-2.tif,,,,\n"+
			"REC003,VOL001,IMAGES/001/DOC-B-1.tif,Y,,,1\n"), 0o600))
	sourceMapping := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","source_ordinal":0,"canonical":"loadfile.document.id"},{"source":"PARENTID","source_ordinal":1,"canonical":"loadfile.family.parent"},{"source":"NATIVE","source_ordinal":2,"canonical":"loadfile.file.native"},{"source":"PDF","source_ordinal":3,"canonical":"loadfile.file.produced_pdf"},{"source":"BEGBATES","source_ordinal":4,"canonical":"loadfile.label.begin"},{"source":"ENDBATES","source_ordinal":5,"canonical":"loadfile.label.end"}]}`)
	pkg, originals := importExportFixture(t, source, catalog, root, "dat-concordance-v1", sourceMapping)
	for _, profile := range []string{"export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1", "export-csv-natives-v1"} {
		t.Run(profile, func(t *testing.T) {
			if profile == "export-dat-pdf-v1" {
				require.Equal(t, 2, originals[0].SourcePageCount, "preflight must retain its verified PDF page count")
			}
			var archive bytes.Buffer
			_, err := processing.WriteLoadFileExport(t.Context(), catalog.Store, catalog.Blobs,
				processing.LoadFileExportRequest{SnapshotID: pkg.SnapshotID, SourcePackageID: pkg.PackageID, ProfileID: profile}, &archive)
			require.NoError(t, err)
			verified, err := processing.VerifyLoadFileExport(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
			require.NoError(t, err)
			extracted, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(extracted)) })
			mapping, err := canonical.Marshal(verified.Receipt.Mapping)
			require.NoError(t, err)
			fresh, freshCatalog := newPackageTestServer(t)
			freshPkg, members := importExportFixture(t, fresh, freshCatalog, extracted, verified.Receipt.Profile.ID, mapping)
			require.Len(t, members, 2)
			require.Equal(t, originals[0].BlobSHA256, members[0].BlobSHA256)
			require.Equal(t, originals[1].BlobSHA256, members[1].BlobSHA256)
			require.Equal(t, members[0].OccurrenceID, members[1].ParentOccurrenceID)
			require.Equal(t, members[0].FamilyID, members[1].FamilyID)
			if verified.Receipt.PageMap != "" {
				require.Equal(t, "REC001", members[0].DisplayName)
				require.Equal(t, "REC003", members[1].DisplayName)
				require.Equal(t, "REC001", verified.Crosswalk.Members[1].ParentDocumentID)
			}
			for index, member := range members {
				labels, err := freshCatalog.PackageLabels(t.Context(), freshPkg.PackageID, member.OccurrenceID)
				require.NoError(t, err)
				var begin, end string
				var pages []string
				for _, label := range labels {
					switch label.Endpoint {
					case "begin":
						begin = label.Label
					case "end":
						end = label.Label
					case "page":
						pages = append(pages, label.Label)
					}
				}
				require.Equal(t, []string{"REC001", "REC003"}[index], begin)
				require.Equal(t, []string{"REC002", "REC003"}[index], end)
				if verified.Receipt.PageMap != "" {
					require.ElementsMatch(t, [][]string{{"REC001", "REC002"}, {"REC003"}}[index], pages)
				}
			}
		})
	}
}

func TestPackageExportSelectedPDFThroughPublicImport(t *testing.T) { //nolint:paralleltest // pdfcpu default configuration initializes process-wide state
	source, catalog := newPackageTestServer(t)
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "VOL001"), 0o700))
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetFont("Helvetica", "", 16)
	for page := 1; page <= 8; page++ {
		pdf.AddPage()
		pdf.Cell(250, 25, fmt.Sprintf("SOURCE PAGE %02d", page))
	}
	var original bytes.Buffer
	require.NoError(t, pdf.Output(&original))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "source.pdf"), original.Bytes(), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "data.dat"), []byte("DOCID\x14PDF\nSOURCE\x14source.pdf\n"), 0o600))
	mapping := []byte(`{"contract":"loadfile-mapping/v1","columns":[{"source":"DOCID","source_ordinal":0,"canonical":"loadfile.document.id"},{"source":"PDF","source_ordinal":1,"canonical":"loadfile.file.produced_pdf"}]}`)
	_, members := importExportFixture(t, source, catalog, root, "dat-concordance-v1", mapping)
	require.Len(t, members, 1)
	for _, test := range []struct {
		name    string
		pages   []int
		stamped bool
	}{
		{"whole", []int{1, 2, 3, 4, 5, 6, 7, 8}, false},
		{"selected", []int{3, 6, 8}, false},
		{"Bates selected", []int{3, 6, 8}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			member := members[0]
			member.SelectedSourcePages = test.pages
			snapshot, err := catalog.SealCollectionSnapshot(t.Context(), store.SnapshotSealRequest{SnapshotID: uuid.NewString(), SourceCollectionIDs: member.SourceCollectionIDs, Members: []store.CollectionSnapshotMember{member}})
			require.NoError(t, err)
			request := processing.LoadFileExportRequest{SnapshotID: snapshot.SnapshotID, ProfileID: "export-dat-pdf-v1"}
			if len(test.pages) < 8 {
				rejected := source.call(t, http.MethodPost, "/api/v1/packages/exports", mustPackageJSON(t, api.PackageExportRequest{
					SnapshotID: snapshot.SnapshotID, ProfileID: "export-csv-natives-v1",
				}), nil)
				require.Equal(t, http.StatusUnprocessableEntity, rejected.Code, rejected.Body.String())
			}
			var originalArtifact []byte
			var before store.BatesAllocation
			if test.stamped {
				namespace, err := catalog.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
				require.NoError(t, err)
				recipe := batesTestRecipe(api.BatesNamespace{NamespaceID: namespace.NamespaceID, Prefix: "SYN", Padding: 6}, 1)
				digest, err := recipe.SHA256()
				require.NoError(t, err)
				pages, err := catalog.SnapshotBatesPages(t.Context(), snapshot.SnapshotID)
				require.NoError(t, err)
				allocation, err := catalog.ReserveBatesRange(t.Context(), store.BatesPlanRequest{OperationID: uuid.NewString(), NamespaceID: namespace.NamespaceID, SnapshotID: snapshot.SnapshotID, RecipeSHA256: digest, StartAt: 1, Pages: pages})
				require.NoError(t, err)
				_, err = processing.PublishBatesExport(t.Context(), catalog.Store, catalog.Blobs, allocation.AllocationID, recipe)
				require.NoError(t, err)
				request.BatesAllocationID = allocation.AllocationID
				originalArtifact, _, err = processing.ReadBatesExport(t.Context(), catalog.Store, catalog.Blobs, allocation.AllocationID)
				require.NoError(t, err)
				before, err = catalog.BatesAllocation(t.Context(), allocation.AllocationID)
				require.NoError(t, err)
			}
			var archive bytes.Buffer
			_, err = processing.WriteLoadFileExport(t.Context(), catalog.Store, catalog.Blobs, request, &archive)
			require.NoError(t, err)
			verified, err := processing.VerifyLoadFileExport(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
			require.NoError(t, err)
			var sourcePages []int
			for index, page := range verified.Manifest.Images {
				require.Equal(t, index+1, page.PageOrdinal)
				sourcePages = append(sourcePages, page.SourcePage)
			}
			require.Equal(t, test.pages, sourcePages)
			extracted, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(extracted)) })
			output, err := os.ReadFile(filepath.Join(extracted, "VOL001", "PDF", "DOC000001.pdf"))
			require.NoError(t, err)
			var contents []string
			require.NoError(t, pdfapi.ExtractContent(bytes.NewReader(output), nil, func(r io.Reader, _ int) error {
				data, err := io.ReadAll(r)
				contents = append(contents, string(data))
				return err
			}, nil))
			require.Len(t, contents, len(test.pages))
			if !test.stamped {
				for index, page := range test.pages {
					require.Contains(t, contents[index], fmt.Sprintf("SOURCE PAGE %02d", page))
				}
			}
			if test.stamped {
				combined, err := os.ReadFile(filepath.Join(extracted, filepath.FromSlash(verified.Crosswalk.CombinedArtifact.RelPath)))
				require.NoError(t, err)
				require.Equal(t, originalArtifact, combined)
				_, err = pdfstamp.VerifyStamped(t.Context(), output, []pdfstamp.PageLabel{{SourcePage: 1, Label: "SYN000001"}, {SourcePage: 2, Label: "SYN000002"}, {SourcePage: 3, Label: "SYN000003"}})
				require.NoError(t, err)
				after, err := catalog.BatesAllocation(t.Context(), request.BatesAllocationID)
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			mapping, err := canonical.Marshal(verified.Receipt.Mapping)
			require.NoError(t, err)
			fresh, freshCatalog := newPackageTestServer(t)
			freshPkg, imported := importExportFixture(t, fresh, freshCatalog, extracted, verified.Receipt.Profile.ID, mapping)
			require.Len(t, imported, 1)
			require.Equal(t, len(test.pages), imported[0].SourcePageCount)
			stream, err := freshCatalog.Blobs.Open(imported[0].BlobSHA256)
			require.NoError(t, err)
			defer func() { require.NoError(t, stream.Close()) }()
			importedBytes, err := io.ReadAll(stream)
			require.NoError(t, err)
			require.Equal(t, output, importedBytes)
			if test.stamped {
				labels, err := freshCatalog.PackageLabels(t.Context(), freshPkg.PackageID, imported[0].OccurrenceID)
				require.NoError(t, err)
				var assigned []string
				for _, label := range labels {
					if label.Endpoint == "page" && label.Provenance == "assigned" {
						assigned = append(assigned, label.Label)
					}
				}
				require.ElementsMatch(t, []string{"SYN000001", "SYN000002", "SYN000003"}, assigned)
			}
		})
	}
}

func TestPackageExportSelectsOnlyProfileTextRoles(t *testing.T) {
	t.Parallel()
	source, catalog := newPackageTestServer(t)
	_, members := importExportFixture(t, source, catalog, syntheticPackageRoot(t), "dat-concordance-v1", nil)
	textContents := map[string]string{
		"supplied_text":  "Supplied text from the sender.\n",
		"rendition_text": "Text extracted from the document.\n",
	}
	textRepresentations := make(map[string]store.CollectionSnapshotRepresentation)
	for role, content := range textContents {
		hash, size, err := catalog.Blobs.Write(bytes.NewBufferString(content))
		require.NoError(t, err)
		require.NoError(t, catalog.RecordBlob(t.Context(), hash, size, store.BlobPhysical{Encoding: "raw", StoredBytes: size}))
		textRepresentations[role] = store.CollectionSnapshotRepresentation{
			OccurrenceID: members[0].OccurrenceID, ContentVersionID: members[0].ContentVersionID,
			Role: role, Status: "available", TextAuthority: "none", BlobSHA256: hash, Size: size, MediaType: "text/plain",
		}
	}
	for _, profile := range []struct {
		id   string
		want [3]string // supplied only, rendition only, both
	}{
		{"export-dat-pdf-v1", [3]string{"supplied_text", "rendition_text", "supplied_text"}},
		{"export-dat-opt-images-v1", [3]string{"", "rendition_text", "rendition_text"}},
		{"export-csv-natives-v1", [3]string{"", "rendition_text", "rendition_text"}},
		{"export-dat-lfp-images-v1", [3]string{"supplied_text", "", "supplied_text"}},
	} {
		for index, roles := range [][]string{{"supplied_text"}, {"rendition_text"}, {"supplied_text", "rendition_text"}} {
			t.Run(fmt.Sprintf("%s/%v", profile.id, roles), func(t *testing.T) {
				member := members[0]
				member.Representations = slices.DeleteFunc(slices.Clone(member.Representations), func(rep store.CollectionSnapshotRepresentation) bool {
					return rep.Role == "supplied_text" || rep.Role == "rendition_text"
				})
				for _, role := range roles {
					member.Representations = append(member.Representations, textRepresentations[role])
				}
				snapshot, err := catalog.SealCollectionSnapshot(t.Context(), store.SnapshotSealRequest{
					SnapshotID: uuid.NewString(), SourceCollectionIDs: member.SourceCollectionIDs, Members: []store.CollectionSnapshotMember{member},
				})
				require.NoError(t, err)
				var archive bytes.Buffer
				_, err = processing.WriteLoadFileExport(t.Context(), catalog.Store, catalog.Blobs,
					processing.LoadFileExportRequest{SnapshotID: snapshot.SnapshotID, ProfileID: profile.id}, &archive)
				require.NoError(t, err)
				verified, err := processing.VerifyLoadFileExport(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
				require.NoError(t, err)
				extracted, err := loadfile.ExtractZIP(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(extracted)) })
				textFiles, err := filepath.Glob(filepath.Join(extracted, "VOL001", "TEXT", "*.txt"))
				require.NoError(t, err)
				wantRole := profile.want[index]
				var exportedRoles []string
				for _, role := range verified.Crosswalk.Members[0].Roles {
					if role.Role == "supplied_text" || role.Role == "rendition_text" {
						exportedRoles = append(exportedRoles, role.Role)
					}
				}
				if wantRole == "" {
					require.Empty(t, textFiles)
					require.Empty(t, exportedRoles)
				} else {
					require.Len(t, textFiles, 1)
					content, err := os.ReadFile(textFiles[0])
					require.NoError(t, err)
					require.Equal(t, textContents[wantRole], string(content))
					require.Equal(t, []string{wantRole}, exportedRoles)
				}
				mapping, err := canonical.Marshal(verified.Receipt.Mapping)
				require.NoError(t, err)
				fresh, freshCatalog := newPackageTestServer(t)
				_, imported := importExportFixture(t, fresh, freshCatalog, extracted, verified.Receipt.Profile.ID, mapping)
				require.Len(t, imported, 1)
				var importedText []string
				for _, rep := range imported[0].Representations {
					if rep.Role == "supplied_text" && rep.Status == "available" {
						reader, err := freshCatalog.Blobs.Open(rep.BlobSHA256)
						require.NoError(t, err)
						content, err := io.ReadAll(reader)
						require.NoError(t, err)
						require.NoError(t, reader.Close())
						importedText = append(importedText, string(content))
					}
				}
				if wantRole == "" {
					require.Empty(t, importedText)
				} else {
					require.Equal(t, []string{textContents[wantRole]}, importedText)
				}
			})
		}
	}
}

// Use normal preflight and import admission; never install a prepared manifest
// or preflight row in the destination vault.
func importExportFixture(t *testing.T, srv *testServer, catalog *testStore, root, profile string, mapping []byte) (store.Package, []store.CollectionSnapshotMember) {
	t.Helper()
	response := srv.post(t, mustPackageJSON(t, api.PackagePreflightRequest{
		SourceKind: "root", SourceRef: root, Profile: profile, Encoding: "utf-8", Mapping: mapping,
	}))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var preview api.PackagePreflight
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &preview))
	require.False(t, preview.Blocking, "%+v", preview.Diagnostics)
	request := api.PackageImportRequest{PreflightID: preview.PreflightID, Into: "/", Name: "synthetic-export", OperationID: uuid.NewString()}
	response = srv.call(t, http.MethodPost, "/api/v1/packages/imports", mustPackageJSON(t, request), nil)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	worker, err := processing.NewPackageImportWorker(processing.PackageImportConfig{
		Catalog: catalog.Store, Blobs: catalog.Blobs, Owner: "synthetic-worker",
	})
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.NoError(t, err)
	response = srv.get(t, "/api/v1/packages/imports/"+request.OperationID)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var job api.PackageImportJob
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &job))
	require.Equal(t, "complete", job.State)
	pkg, err := catalog.Package(t.Context(), job.PackageID)
	require.NoError(t, err)
	members, err := catalog.SnapshotMembers(t.Context(), pkg.SnapshotID, 0, 10)
	require.NoError(t, err)
	return pkg, members
}
