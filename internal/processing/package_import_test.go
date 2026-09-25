package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

type packageImportTestEnv struct {
	Catalog     *store.Store
	Blobs       *blob.Store
	Root        string
	PackageID   string
	Owner       string
	OperationID string
	Contents    [][]byte
}

func newPackageImportTestEnv(t *testing.T, count int, acceptPartial bool, withPages ...bool) *packageImportTestEnv {
	t.Helper()
	pages := len(withPages) > 0 && withPages[0]
	return newPackageImportTestEnvOptions(t, count, acceptPartial, pages, false)
}

func newPackageImportTestEnvOptions(t *testing.T, count int, acceptPartial, withPages, indexText bool, families ...loadfile.Family) *packageImportTestEnv {
	t.Helper()
	return newPackageImportTestEnvWithNativeFile(t, count, acceptPartial, withPages, indexText, "", nil, families...)
}

func newPackageImportTestEnvWithNativeFile(t *testing.T, count int, acceptPartial, withPages, indexText bool,
	nativeName string, nativeContent []byte, families ...loadfile.Family) *packageImportTestEnv {
	t.Helper()
	ctx := t.Context()
	vault := t.TempDir()
	catalog, err := store.Open(filepath.Join(vault, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(vault, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
	var records []loadfile.Record
	var files []loadfile.FileRef
	var images []loadfile.ImageRef
	contents := make([][]byte, count)
	for index := range count {
		letter := string(rune('A' + index))
		name := letter + ".txt"
		content := []byte("Synthetic native document " + letter + "\n")
		if index == 0 && nativeName != "" {
			name, content = nativeName, nativeContent
		}
		contents[index] = content
		require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", name), content, 0o600))
		ref := loadfile.FileRef{Role: "native", Volume: "VOL001", RelPath: name, Declared: name,
			SHA256: packageImportTestHash(content), Size: int64(len(content)), Status: "available"}
		files = append(files, ref)
		recordFiles := []loadfile.FileRef{ref}
		if indexText {
			textName := letter + "-supplied.txt"
			textBytes := []byte("Synthetic supplied text " + letter + "\n")
			require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", textName), textBytes, 0o600))
			textRef := loadfile.FileRef{Role: "supplied_text", Volume: "VOL001", RelPath: textName,
				Declared: textName, SHA256: packageImportTestHash(textBytes), Size: int64(len(textBytes)), Status: "available"}
			files = append(files, textRef)
			recordFiles = append(recordFiles, textRef)
		}
		labelNumber := index + 1
		if withPages && index > 0 {
			labelNumber++
		}
		label := fmt.Sprintf("EXT00000%d", labelNumber)
		records = append(records, loadfile.Record{RowID: "row-" + letter, DocID: "DOC-" + letter,
			LoadFile: "VOL001/DATA.DAT", RowOrdinal: index + 1,
			Fields: []loadfile.Field{{Column: "BEGBATES", Ordinal: 0, Canonical: "loadfile.label.begin",
				Raw: label, Value: loadfile.Value{Kind: "text", Text: label}}},
			Files: recordFiles})
		if index < len(families) {
			records[index].Family = families[index]
		}
		if withPages {
			pageCount := 1
			if index == 0 {
				pageCount = 2
			} else {
				records[index].Family.ParentDocID = "DOC-A"
			}
			for page := 1; page <= pageCount; page++ {
				pageName := fmt.Sprintf("%s-%d.tif", letter, page)
				pageBytes := []byte(fmt.Sprintf("synthetic page %s-%d", letter, page))
				require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", pageName), pageBytes, 0o600))
				files = append(files, loadfile.FileRef{Role: "page_image", Volume: "VOL001", RelPath: pageName,
					Declared: pageName, SHA256: packageImportTestHash(pageBytes), Size: int64(len(pageBytes)), Status: "available"})
				labelNumber := index + page
				if index > 0 {
					labelNumber++
				}
				imageKey := fmt.Sprintf("EXT00000%d", labelNumber)
				images = append(images, loadfile.ImageRef{ImageKey: imageKey,
					Volume: "VOL001", RelPath: pageName, DocumentBreak: page == 1,
					PageOrdinal: page})
			}
		}
	}
	resolver, err := loadfile.NewResolver(ctx, root, nil)
	require.NoError(t, err)
	rootDigest, err := resolver.RootDigest()
	require.NoError(t, err)
	require.NoError(t, resolver.Close())
	rootBinding, err := store.PackageVolumeRootBinding(rootDigest, "VOL001")
	require.NoError(t, err)

	profile, err := loadfile.ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	profileJSON, err := canonical.Marshal(profile)
	require.NoError(t, err)
	profileSHA := packageImportTestHash(profileJSON)
	mapping := loadfile.Mapping{Contract: loadfile.MappingContractV1, Columns: []loadfile.MappingColumn{},
		DefaultCustodian: "Doe, Jane"}
	mappingJSON, err := canonical.Marshal(mapping)
	require.NoError(t, err)
	mappingSHA := packageImportTestHash(mappingJSON)
	manifest := loadfile.Manifest{ProfileSHA256: profileSHA, MappingSHA256: mappingSHA, Mapping: mapping,
		Volumes: []loadfile.Volume{{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}},
		Records: records, Images: images, Files: files}
	var encoded bytes.Buffer
	require.NoError(t, manifest.WriteJSONL(&encoded))
	manifestSHA := packageImportTestHash(encoded.Bytes())
	write, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	require.Equal(t, manifestSHA, write.Hash)
	require.NoError(t, catalog.RecordBlob(ctx, write.Hash, write.Size, packageImportTestPhysical(t, write)))
	owner, preflightID := "synthetic-operator", uuid.NewString()
	_, err = catalog.PutPackagePreflight(ctx, store.PackagePreflightRecord{
		PreflightID: preflightID, Owner: owner, SourceKind: "root", SourceRef: rootDigest,
		SourceLocator: root, ProfileJSON: string(profileJSON), MappingJSON: string(mappingJSON),
		ProfileSHA256: profileSHA, MappingSHA256: mappingSHA, ManifestSHA256: manifestSHA,
		ManifestBlobSHA256: manifestSHA, CanonicalJSON: []byte(`{}`), DiagnosticsJSON: []byte(`[]`),
	})
	require.NoError(t, err)
	run, err := catalog.BeginIngest(ctx, "package:loadfile", "synthetic-package")
	require.NoError(t, err)
	packageID := uuid.NewString()
	request := store.PackageRequest{PackageID: packageID, Direction: "received", PackageName: "synthetic-package",
		ProfileSHA256: profileSHA, ProfileJSON: string(profileJSON), MappingSHA256: mappingSHA,
		MappingJSON: string(mappingJSON), ManifestSHA256: manifestSHA, ManifestBlobSHA256: manifestSHA,
		IngestID: run.ID(), State: "importing", Volumes: []store.PackageVolume{{Ordinal: 1, VolumeName: "VOL001",
			DeclaredRoot: "VOL001", MappedRoot: "VOL001", ResolvedRootSHA256: rootBinding}}}
	operationID := uuid.NewString()
	jobJSON, err := canonical.Marshal(packageImportWork{AcceptPartial: acceptPartial,
		IndexSuppliedText: indexText, Into: "/", SourceKind: "root", SourceLocator: root, Total: count})
	require.NoError(t, err)
	job := store.PackageImportJobRequest{ID: uuid.NewString(), Owner: owner, OperationID: operationID,
		RequestSHA256: packageImportTestHash([]byte("synthetic request")), PreflightID: preflightID,
		PackageID: packageID, JobJSON: jobJSON}
	_, err = catalog.AdmitPackageImport(ctx, run, request, job)
	require.NoError(t, err)
	return &packageImportTestEnv{Catalog: catalog, Blobs: blobs, Root: root,
		PackageID: packageID, Owner: owner, OperationID: operationID, Contents: contents}
}

func (e *packageImportTestEnv) config() PackageImportConfig {
	return PackageImportConfig{Catalog: e.Catalog, Blobs: e.Blobs,
		Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Mutate: func(_ context.Context, fn func() error) error { return fn() }}
}

func TestPackageImportWorkerCommitsOneVerifiedRootRecord(t *testing.T) {
	env := newPackageImportTestEnv(t, 1, false)
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	more, err := worker.ProcessOnce(t.Context())
	require.NoError(t, err)
	require.True(t, more)
	got, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	require.Equal(t, "complete", got.State)
	require.Equal(t, 1, got.MemberCount)
	members, err := env.Catalog.SnapshotMembers(t.Context(), got.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, packageImportTestHash(env.Contents[0]), members[0].BlobSHA256)
	labels, err := env.Catalog.LookupPackageLabel(t.Context(), "EXT000001", env.PackageID, "", "received")
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, members[0].ContentVersionID, labels[0].ContentVersionID)
	recordKey, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
	require.NoError(t, err)
	custodians, _, err := env.Catalog.Custodians(t.Context(), store.CustodianScope{Kind: "package",
		PackageID: env.PackageID, PackageRecordID: recordKey, HasPackageRecordID: true}, false, 10, 0)
	require.NoError(t, err)
	require.Len(t, custodians, 1)
	require.Equal(t, "Doe, Jane", custodians[0].RawLabel)
}

func TestPackageImportDetectsMIMEFromVerifiedBytes(t *testing.T) {
	const extension = ".docbank561"
	require.NoError(t, mime.AddExtensionType(extension, "application/jpg"))
	require.Equal(t, "application/jpg", mime.TypeByExtension(extension))
	jpeg := []byte{0xff, 0xd8, 0xff}
	env := newPackageImportTestEnvWithNativeFile(t, 1, false, false, false, "A"+extension, jpeg)
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.NoError(t, err)

	key, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
	require.NoError(t, err)
	path := "/" + env.PackageID + "/" + store.PackageOccurrenceID(env.PackageID, key) + "-00-native" + extension
	node, err := env.Catalog.NodeByPath(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", node.MimeType)
	require.Equal(t, packageImportTestHash(jpeg), node.BlobHash)
	require.Equal(t, int64(len(jpeg)), node.Size)
}

func TestPackageRecordLabelsKeepReceivedAndAssignedProvenanceSeparate(t *testing.T) {
	record := loadfile.Record{Fields: []loadfile.Field{
		{Canonical: "loadfile.label.set", Raw: "EXT"},
		{Canonical: "loadfile.label.begin", Raw: "EXT000001"},
		{Canonical: "loadfile.label.assigned.set", Raw: "CASE"},
		{Canonical: "loadfile.label.assigned.begin", Raw: "CASE000001"},
		{Canonical: "loadfile.label.assigned.end", Raw: "CASE000002"},
	}}
	received, assigned := packageRecordLabels("11111111-1111-4111-8111-111111111111",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "22222222-2222-4222-8222-222222222222", record)
	require.Len(t, received, 1)
	require.Equal(t, "received", received[0].Provenance)
	require.Equal(t, "EXT", received[0].LabelSet)
	require.Equal(t, "EXT000001", received[0].Label)
	require.Len(t, assigned, 2)
	require.Equal(t, "assigned", assigned[0].Provenance)
	require.Equal(t, "CASE", assigned[0].LabelSet)
	require.Equal(t, "CASE000001", assigned[0].Label)
	require.Equal(t, "CASE000002", assigned[1].Label)
}

func TestPackageImportWorkerResumesAfterCommittedRecord(t *testing.T) {
	env := newPackageImportTestEnv(t, 2, false)
	injected := errors.New("synthetic interruption before second record")
	firstKey, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
	require.NoError(t, err)
	var interrupted atomic.Bool
	config := env.config()
	config.Mutate = func(ctx context.Context, fn func() error) error {
		if _, headErr := env.Catalog.PackageImportHead(ctx, env.PackageID, firstKey); headErr == nil && interrupted.CompareAndSwap(false, true) {
			return injected
		}
		return fn()
	}
	worker, err := NewPackageImportWorker(config)
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.ErrorIs(t, err, injected)
	firstReceipt, err := env.Catalog.PackageImportHead(t.Context(), env.PackageID, firstKey)
	require.NoError(t, err)
	require.Equal(t, "committed", firstReceipt.State)
	secondKey, err := store.PackageRecordKey("VOL001/DATA.DAT", 2, "DOC-B")
	require.NoError(t, err)
	_, err = env.Catalog.PackageImportHead(t.Context(), env.PackageID, secondKey)
	require.ErrorIs(t, err, store.ErrNotFound)

	resumed, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = resumed.ProcessOnce(t.Context())
	require.NoError(t, err)
	unchanged, err := env.Catalog.PackageImportHead(t.Context(), env.PackageID, firstKey)
	require.NoError(t, err)
	require.Equal(t, firstReceipt.ReceiptID, unchanged.ReceiptID)
	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	require.Equal(t, "complete", pkg.State)
	require.Equal(t, 2, pkg.MemberCount)
}

func TestPackageImportWorkerPersistsSuppliedTextGenerationInReceipt(t *testing.T) {
	env := newPackageImportTestEnvOptions(t, 1, false, false, true)
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.NoError(t, err)
	key, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
	require.NoError(t, err)
	receipt, err := env.Catalog.PackageImportHead(t.Context(), env.PackageID, key)
	require.NoError(t, err)
	var data packageImportReceiptData
	require.NoError(t, json.Unmarshal(receipt.ReceiptJSON, &data))
	require.NotNil(t, data.Member)
	var supplied *store.CollectionSnapshotRepresentation
	for index := range data.Member.Representations {
		if data.Member.Representations[index].Role == "supplied_text" {
			supplied = &data.Member.Representations[index]
			break
		}
	}
	require.NotNil(t, supplied)
	require.Equal(t, "supplied", supplied.TextAuthority)
	require.NotEmpty(t, supplied.LexicalGenerationID)
	members, err := env.Catalog.SnapshotMembers(t.Context(), (mustPackage(t, env)).SnapshotID, 0, 10)
	require.NoError(t, err)
	var frozen *store.CollectionSnapshotRepresentation
	for index := range members[0].Representations {
		if members[0].Representations[index].Role == "supplied_text" {
			frozen = &members[0].Representations[index]
			break
		}
	}
	require.NotNil(t, frozen)
	require.Equal(t, supplied.LexicalGenerationID, frozen.LexicalGenerationID)
}

func mustPackage(t *testing.T, env *packageImportTestEnv) store.Package {
	t.Helper()
	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	return pkg
}

func TestPackageImportWorkerRecoversStagedNodeWithoutRecordReceipt(t *testing.T) {
	env := newPackageImportTestEnv(t, 1, false)
	injected := errors.New("synthetic interruption after staging")
	key, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
	require.NoError(t, err)
	stagedPath := "/" + env.PackageID + "/" + store.PackageOccurrenceID(env.PackageID, key) + "-00-native.txt"
	var interrupted atomic.Bool
	config := env.config()
	config.Mutate = func(ctx context.Context, fn func() error) error {
		if err := fn(); err != nil {
			return err
		}
		_, err := env.Catalog.NodeByPath(ctx, stagedPath)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if interrupted.CompareAndSwap(false, true) {
			return injected
		}
		return nil
	}
	worker, err := NewPackageImportWorker(config)
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.ErrorIs(t, err, injected)
	staged, err := env.Catalog.NodeByPath(t.Context(), stagedPath)
	require.NoError(t, err)
	_, err = env.Catalog.PackageImportHead(t.Context(), env.PackageID, key)
	require.ErrorIs(t, err, store.ErrNotFound)

	resumed, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = resumed.ProcessOnce(t.Context())
	require.NoError(t, err)
	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	require.Equal(t, "complete", pkg.State)
	require.Equal(t, 1, pkg.MemberCount)
	recovered, err := env.Catalog.NodeByPath(t.Context(), stagedPath)
	require.NoError(t, err)
	require.Equal(t, staged.CurrentVersionID, recovered.CurrentVersionID)
}

func TestPackageImportWorkerAcceptsMissingPostPreflightFileAsPartial(t *testing.T) {
	env := newPackageImportTestEnv(t, 2, true)
	require.NoError(t, os.Remove(filepath.Join(env.Root, "VOL001", "B.txt")))
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.NoError(t, err)
	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	require.Equal(t, "partial", pkg.State)
	require.Equal(t, 1, pkg.MemberCount)
	missingKey, err := store.PackageRecordKey("VOL001/DATA.DAT", 2, "DOC-B")
	require.NoError(t, err)
	receipt, err := env.Catalog.PackageImportHead(t.Context(), env.PackageID, missingKey)
	require.NoError(t, err)
	require.Equal(t, "rejected", receipt.State)
	require.Empty(t, receipt.ContentVersionID)
	labels, err := env.Catalog.LookupPackageLabel(t.Context(), "EXT000002", env.PackageID, "", "received")
	require.NoError(t, err)
	require.Empty(t, labels)
}

func TestPackageImportWorkerStrictModeRejectsDriftedRootBeforePublishing(t *testing.T) {
	env := newPackageImportTestEnv(t, 2, false)
	require.NoError(t, os.Remove(filepath.Join(env.Root, "VOL001", "B.txt")))
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.ErrorIs(t, err, store.ErrPackageConflict)
	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	require.Equal(t, "failed", pkg.State)
	firstKey, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
	require.NoError(t, err)
	_, err = env.Catalog.PackageImportHead(t.Context(), env.PackageID, firstKey)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = env.Catalog.NodeByPath(t.Context(), "/"+env.PackageID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestPackageImportWorkerFailsChangedAdmittedBytes(t *testing.T) {
	for _, name := range []string{"B.txt", "B-supplied.txt", "B-1.tif"} {
		t.Run(name, func(t *testing.T) {
			env := newPackageImportTestEnvOptions(t, 2, false, true, true)
			file := filepath.Join(env.Root, "VOL001", name)
			replacement, err := os.ReadFile(file)
			require.NoError(t, err)
			replacement[0] ^= 1
			require.NoError(t, os.WriteFile(file, replacement, 0o600))
			worker, err := NewPackageImportWorker(env.config())
			require.NoError(t, err)
			_, err = worker.ProcessOnce(t.Context())
			require.ErrorIs(t, err, store.ErrPackageConflict)
			pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
			require.NoError(t, err)
			require.Equal(t, "failed", pkg.State)
			require.Empty(t, pkg.SnapshotID)
			_, err = env.Catalog.NodeByPath(t.Context(), "/"+env.PackageID)
			require.ErrorIs(t, err, store.ErrNotFound)
			firstKey, err := store.PackageRecordKey("VOL001/DATA.DAT", 1, "DOC-A")
			require.NoError(t, err)
			_, err = env.Catalog.PackageImportHead(t.Context(), env.PackageID, firstKey)
			require.ErrorIs(t, err, store.ErrNotFound)
			job, err := env.Catalog.PackageImportJob(t.Context(), env.Owner, env.OperationID)
			require.NoError(t, err)
			require.Equal(t, "failed", job.State)
			more, err := worker.ProcessOnce(t.Context())
			require.NoError(t, err)
			require.False(t, more)
		})
	}
}

func TestPackageImportWorkerTreatsInvalidDestinationsAsTerminal(t *testing.T) {
	require.True(t, terminalPackageImportError(store.ErrNotFound))
	require.True(t, terminalPackageImportError(store.ErrNotDir))
}

func TestPackageImportWorkerRetainsFamilyAndMissingDeclaredPage(t *testing.T) {
	env := newPackageImportTestEnv(t, 2, true, true)
	require.NoError(t, os.Remove(filepath.Join(env.Root, "VOL001", "B-1.tif")))
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.NoError(t, err)
	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	require.Equal(t, "partial", pkg.State)
	require.Equal(t, 2, pkg.MemberCount)
	require.Equal(t, 2, pkg.PageCount)
	members, err := env.Catalog.SnapshotMembers(t.Context(), pkg.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, members, 2)
	require.Equal(t, members[0].OccurrenceID, members[1].ParentOccurrenceID)
	require.Equal(t, members[0].FamilyID, members[1].FamilyID)
	require.Equal(t, 1, members[0].FamilyOrder)
	require.Equal(t, 2, members[1].FamilyOrder)
	var missingPage *store.CollectionSnapshotRepresentation
	for index := range members[1].Representations {
		rep := &members[1].Representations[index]
		if rep.Role == "page_image" {
			missingPage = rep
			break
		}
	}
	require.NotNil(t, missingPage)
	require.Equal(t, "missing", missingPage.Status)
	require.Empty(t, missingPage.BlobSHA256)
}

func TestPackageImportWorkerRerootsChildWhenParentIsRejected(t *testing.T) {
	env := newPackageImportTestEnv(t, 2, true, true)
	for _, name := range []string{"A.txt", "A-1.tif", "A-2.tif"} {
		require.NoError(t, os.Remove(filepath.Join(env.Root, "VOL001", name)))
	}
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(t.Context())
	require.NoError(t, err)
	pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
	require.NoError(t, err)
	require.Equal(t, "partial", pkg.State)
	require.Equal(t, 1, pkg.MemberCount)
	members, err := env.Catalog.SnapshotMembers(t.Context(), pkg.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Empty(t, members[0].ParentOccurrenceID)
	require.Equal(t, members[0].OccurrenceID, members[0].FamilyID)
}

func TestPackageImportWorkerPreservesExplicitFamilyGroups(t *testing.T) {
	for _, rejectFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject_first=%t", rejectFirst), func(t *testing.T) {
			env := newPackageImportTestEnvOptions(t, 5, rejectFirst, false, false,
				loadfile.Family{GroupID: "family-one"}, loadfile.Family{GroupID: "family-one"},
				loadfile.Family{ParentDocID: "DOC-B"}, loadfile.Family{GroupID: "family-one"},
				loadfile.Family{GroupID: "family-two"})
			if rejectFirst {
				require.NoError(t, os.Remove(filepath.Join(env.Root, "VOL001", "A.txt")))
			}
			worker, err := NewPackageImportWorker(env.config())
			require.NoError(t, err)
			_, err = worker.ProcessOnce(t.Context())
			require.NoError(t, err)
			pkg, err := env.Catalog.Package(t.Context(), env.PackageID)
			require.NoError(t, err)
			members, err := env.Catalog.SnapshotMembers(t.Context(), pkg.SnapshotID, 0, 10)
			require.NoError(t, err)
			if rejectFirst {
				require.Len(t, members, 4)
			} else {
				require.Len(t, members, 5)
			}
			root := members[0].OccurrenceID
			for index, member := range members[:len(members)-1] {
				require.Equal(t, root, member.FamilyID)
				require.Equal(t, index+1, member.FamilyOrder)
				if member.DisplayName != "DOC-C" {
					require.Empty(t, member.ParentOccurrenceID)
				} else {
					require.Equal(t, members[index-1].OccurrenceID, member.ParentOccurrenceID)
				}
			}
			separate := members[len(members)-1]
			require.Equal(t, "DOC-E", separate.DisplayName)
			require.Equal(t, separate.OccurrenceID, separate.FamilyID)
			require.NoError(t, env.Catalog.ValidateMetadata(t.Context()))
		})
	}
}

func TestPackageImportWorkerOpensOwnerSealedZIPSource(t *testing.T) {
	env := newPackageImportTestEnv(t, 1, false)
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	member, err := zipWriter.Create("VOL001/A.txt")
	require.NoError(t, err)
	_, err = member.Write(env.Contents[0])
	require.NoError(t, err)
	require.NoError(t, zipWriter.Close())
	owner, containerID := "synthetic-operator", "synthetic-package-container"
	config := env.config()
	config.OpenContainer = func(_ context.Context, gotOwner, gotID string) (io.ReaderAt, int64, error) {
		if gotOwner != owner || gotID != containerID {
			return nil, 0, store.ErrNotFound
		}
		return bytes.NewReader(archive.Bytes()), int64(archive.Len()), nil
	}
	worker, err := NewPackageImportWorker(config)
	require.NoError(t, err)
	_, _, err = worker.sourceRoot(t.Context(), "another-owner", packageImportWork{
		SourceKind: "container", SourceLocator: containerID})
	require.ErrorIs(t, err, store.ErrNotFound)
	root, cleanup, err := worker.sourceRoot(t.Context(), owner, packageImportWork{
		SourceKind: "container", SourceLocator: containerID})
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(root, "VOL001", "A.txt"))
	require.NoError(t, err)
	require.Equal(t, env.Contents[0], data)
	require.NoError(t, cleanup())
	_, err = os.Stat(root)
	require.True(t, os.IsNotExist(err))
}

func packageImportTestHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func packageImportTestPhysical(t *testing.T, receipt blob.WriteReceipt) store.BlobPhysical {
	t.Helper()
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	return store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize,
		PackEligible: receipt.PackEligible, MD5: receipt.MD5, Created: receipt.Created}
}

func TestPackageImageFilesFollowDocumentBreaksAndVerifiedFileStatus(t *testing.T) {
	manifest := loadfile.Manifest{
		Records: []loadfile.Record{
			{DocID: "DOC-A", Fields: []loadfile.Field{{Canonical: "loadfile.label.begin", Raw: "EXT000001"}}},
			{DocID: "DOC-B", Fields: []loadfile.Field{{Canonical: "loadfile.label.begin", Raw: "EXT000003"}}},
		},
		Images: []loadfile.ImageRef{
			{ImageKey: "EXT000001", DocumentBreak: true, Volume: "VOL001", RelPath: "A-1.tif", PageOrdinal: 1},
			{ImageKey: "EXT000002", Volume: "VOL001", RelPath: "A-2.tif", PageOrdinal: 2},
			{ImageKey: "EXT000003", DocumentBreak: true, Volume: "VOL001", RelPath: "B-1.tif", PageOrdinal: 1},
		},
		Files: []loadfile.FileRef{
			{Role: "page_image", Volume: "VOL001", RelPath: "A-1.tif", Status: "available"},
			{Role: "page_image", Volume: "VOL001", RelPath: "A-2.tif", Status: "available"},
			{Role: "page_image", Volume: "VOL001", RelPath: "B-1.tif", Status: "missing"},
		},
	}
	pages, err := packageImageFiles(manifest)
	require.NoError(t, err)
	require.Len(t, pages["DOC-A"], 2)
	require.Equal(t, "EXT000002", pages["DOC-A"][1].Image.ImageKey)
	require.Equal(t, "missing", pages["DOC-B"][0].File.Status)
	manifest.Files = append(manifest.Files, loadfile.FileRef{Role: "page_image", Volume: "VOL001",
		RelPath: "A-2.tif", Status: "missing"})
	_, err = packageImageFiles(manifest)
	require.ErrorIs(t, err, store.ErrPackageConflict)
}

func TestFrozenPackageFieldsPreservePerFieldDisclosure(t *testing.T) {
	zero, one := 0, 1
	record := loadfile.Record{Fields: []loadfile.Field{
		{Column: "BEGBATES", Ordinal: 0, Canonical: "loadfile.label.begin", Raw: "EXT000001"},
		{Column: "Subject", Ordinal: 1, Canonical: "loadfile.document.description", Raw: "Synthetic private context"},
	}}
	mapping := loadfile.Mapping{Columns: []loadfile.MappingColumn{
		{Source: "BEGBATES", SourceOrdinal: &zero, Sensitive: false},
		{Source: "Subject", SourceOrdinal: &one, Sensitive: true},
	}}
	frozen := frozenPackageFields(record, mapping)
	require.Len(t, frozen, 2)
	require.False(t, frozen[0].Sensitive)
	require.True(t, frozen[1].Sensitive, "the confirmed mapping overrides a normally public field")
}

func TestPackageInputVerificationRejectsSameSizeMetadataReplacement(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
	file := filepath.Join(root, "VOL001", "DATA.DAT")
	require.NoError(t, os.WriteFile(file, []byte("original"), 0o600))
	resolver, err := loadfile.NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resolver.Close()) })
	require.NoError(t, os.WriteFile(file, []byte("tampered"), 0o600))
	ref := loadfile.FileRef{Role: "raw_load_file", Volume: "VOL001", RelPath: "DATA.DAT",
		SHA256: packageImportTestHash([]byte("original")), Size: 8, Status: "available"}
	err = verifyPackageInputFile(t.Context(), resolver, loadfile.Volume{Name: "VOL001", DeclaredRoot: "VOL001"}, ref)
	require.ErrorIs(t, err, store.ErrPackageConflict)
}
