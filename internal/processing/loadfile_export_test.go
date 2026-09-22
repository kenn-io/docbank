package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

func TestLoadFileExportRoundTripsThroughIndependentFreshVault(t *testing.T) {
	ctx := t.Context()
	source := newPackageImportTestEnvOptions(t, 2, false, true, true)
	worker, err := NewPackageImportWorker(source.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(ctx)
	require.NoError(t, err)
	pkg := mustPackage(t, source)
	require.Equal(t, "complete", pkg.State)
	sourceMembers, err := source.Catalog.SnapshotMembers(ctx, pkg.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, sourceMembers, 2)
	assigned := store.PackageLabelRow{PackageID: pkg.PackageID, Provenance: "assigned", LabelSet: "CASE",
		Label: "CASE000001", LabelSortKey: "CASE000001", OccurrenceID: sourceMembers[0].OccurrenceID,
		ContentVersionID: sourceMembers[0].ContentVersionID, PageState: "verified", Endpoint: "begin"}
	require.NoError(t, source.Catalog.AssignPackageLabels(ctx, pkg.PackageID, sourceMembers[0].OccurrenceID,
		sourceMembers[0].ContentVersionID, []store.PackageLabelRow{assigned}))
	record, err := source.Catalog.PackageRecordByOccurrence(ctx, pkg.PackageID, sourceMembers[0].OccurrenceID)
	require.NoError(t, err)
	_, err = source.Catalog.SetCustodian(ctx, store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "package", PackageID: pkg.PackageID, PackageRecordID: record.RowID,
			HasPackageRecordID: true},
		RawLabel: "Synthetic Primary", Rank: "primary", Basis: "package_column", SourceRef: "CUSTODIAN",
		IfMatchRevision: 1,
	})
	require.NoError(t, err)

	var archive bytes.Buffer
	built, err := WriteLoadFileExport(ctx, source.Catalog, source.Blobs, LoadFileExportRequest{
		SnapshotID: pkg.SnapshotID, SourcePackageID: pkg.PackageID, ProfileID: "export-dat-lfp-images-v1",
	}, &archive)
	require.NoError(t, err)
	require.Equal(t, 2, built.Receipt.RecordCount)
	require.Equal(t, 3, built.Receipt.PageCount)
	require.NotEmpty(t, built.Receipt.ArchiveSHA256)
	require.NotEmpty(t, built.Receipt.CrosswalkSHA256)
	require.Equal(t, int64(archive.Len()), built.Receipt.Size)
	require.Equal(t, "CUSTODIAN", built.Receipt.Mapping.CustodianColumn)
	require.NotNil(t, built.Crosswalk.Members[0].PrimaryCustodian)
	require.Equal(t, "Synthetic Primary", built.Crosswalk.Members[0].PrimaryCustodian.RawLabel)

	verified, err := VerifyLoadFileExport(ctx, bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.NoError(t, err)
	require.Equal(t, built.Receipt, verified.Receipt)
	builtManifestSHA, err := built.Manifest.SHA256()
	require.NoError(t, err)
	verifiedManifestSHA, err := verified.Manifest.SHA256()
	require.NoError(t, err)
	require.Equal(t, builtManifestSHA, verifiedManifestSHA)
	builtCrosswalk, err := canonical.Marshal(built.Crosswalk)
	require.NoError(t, err)
	verifiedCrosswalk, err := canonical.Marshal(verified.Crosswalk)
	require.NoError(t, err)
	require.JSONEq(t, string(builtCrosswalk), string(verifiedCrosswalk))

	tampered := append([]byte(nil), archive.Bytes()...)
	tampered[len(tampered)/2] ^= 0xff
	_, err = VerifyLoadFileExport(ctx, bytes.NewReader(tampered), int64(len(tampered)))
	require.Error(t, err)
	withExtra := loadFileArchiveWithExtraEntry(t, archive.Bytes())
	_, err = VerifyLoadFileExport(ctx, bytes.NewReader(withExtra), int64(len(withExtra)))
	require.Error(t, err)

	root, err := loadfile.ExtractZIP(ctx, bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(root)) })
	freshRoot := t.TempDir()
	freshCatalog, err := store.Open(filepath.Join(freshRoot, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, freshCatalog.Close()) })
	freshBlobs, err := blob.New(store.NewPackCatalog(freshCatalog), filepath.Join(freshRoot, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, freshBlobs.Close()) })

	resolver, err := loadfile.NewResolver(ctx, root, nil)
	require.NoError(t, err)
	rootDigest, err := resolver.RootDigest()
	require.NoError(t, err)
	require.NoError(t, resolver.Close())
	var manifestJSON bytes.Buffer
	require.NoError(t, verified.Manifest.WriteJSONL(&manifestJSON))
	written, err := freshBlobs.WriteDetailedContext(ctx, bytes.NewReader(manifestJSON.Bytes()))
	require.NoError(t, err)
	require.Equal(t, verified.Receipt.ManifestSHA256, written.Hash)
	require.NoError(t, freshCatalog.RecordBlob(ctx, written.Hash, written.Size, processingBlobPhysical(t, written)))
	profileJSON, err := canonical.Marshal(verified.Receipt.Profile)
	require.NoError(t, err)
	mappingJSON, err := canonical.Marshal(verified.Receipt.Mapping)
	require.NoError(t, err)
	owner, preflightID := "fresh-vault", uuid.NewString()
	_, err = freshCatalog.PutPackagePreflight(ctx, store.PackagePreflightRecord{
		PreflightID: preflightID, Owner: owner, SourceKind: "root", SourceRef: rootDigest,
		SourceLocator: root, ProfileJSON: string(profileJSON), MappingJSON: string(mappingJSON),
		ProfileSHA256: verified.Receipt.ProfileSHA256, MappingSHA256: verified.Receipt.MappingSHA256,
		ManifestSHA256: written.Hash, ManifestBlobSHA256: written.Hash,
		CanonicalJSON: []byte(`{}`), DiagnosticsJSON: []byte(`[]`),
	})
	require.NoError(t, err)
	run, err := freshCatalog.BeginIngest(ctx, "package:loadfile", "fresh-round-trip")
	require.NoError(t, err)
	freshPackageID := uuid.NewString()
	volumes := make([]store.PackageVolume, len(verified.Manifest.Volumes))
	for index, volume := range verified.Manifest.Volumes {
		binding, err := store.PackageVolumeRootBinding(rootDigest, volume.DeclaredRoot)
		require.NoError(t, err)
		volumes[index] = store.PackageVolume{Ordinal: volume.Ordinal, VolumeName: volume.Name,
			DeclaredRoot: volume.DeclaredRoot, MappedRoot: volume.DeclaredRoot, ResolvedRootSHA256: binding}
	}
	_, err = freshCatalog.AdmitPackageImport(ctx, run, store.PackageRequest{
		PackageID: freshPackageID, Direction: "received", PackageName: "fresh-round-trip",
		ProfileSHA256: verified.Receipt.ProfileSHA256, ProfileJSON: string(profileJSON),
		MappingSHA256: verified.Receipt.MappingSHA256, MappingJSON: string(mappingJSON),
		ManifestSHA256: written.Hash, ManifestBlobSHA256: written.Hash,
		IngestID: run.ID(), State: "importing", Volumes: volumes,
	}, store.PackageImportJobRequest{
		ID: uuid.NewString(), Owner: owner, OperationID: uuid.NewString(), RequestSHA256: packageImportTestHash([]byte("fresh-round-trip")),
		PreflightID: preflightID, PackageID: freshPackageID,
		JobJSON: []byte(`{"accept_partial":false,"index_supplied_text":true,"into":"/","source_kind":"root","source_locator":"` + root + `","total":2}`),
	})
	require.NoError(t, err)
	freshWorker, err := NewPackageImportWorker(PackageImportConfig{Catalog: freshCatalog, Blobs: freshBlobs,
		Owner: "fresh-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
		Mutate: func(_ context.Context, fn func() error) error { return fn() }})
	require.NoError(t, err)
	_, err = freshWorker.ProcessOnce(ctx)
	require.NoError(t, err)
	freshPackage, err := freshCatalog.Package(ctx, freshPackageID)
	require.NoError(t, err)
	require.Equal(t, "complete", freshPackage.State)
	freshMembers, err := freshCatalog.SnapshotMembers(ctx, freshPackage.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, freshMembers, 2)
	require.Equal(t, sourceMembers[0].BlobSHA256, freshMembers[0].BlobSHA256)
	require.Equal(t, sourceMembers[1].BlobSHA256, freshMembers[1].BlobSHA256)
	require.Equal(t, freshMembers[0].OccurrenceID, freshMembers[1].ParentOccurrenceID)
	require.Equal(t, freshMembers[0].FamilyID, freshMembers[1].FamilyID)
	labels, err := freshCatalog.PackageLabels(ctx, freshPackageID, freshMembers[0].OccurrenceID)
	require.NoError(t, err)
	require.Contains(t, labels, store.PackageLabelRow{PackageID: freshPackageID, Provenance: "assigned",
		LabelSet: "CASE", Label: "CASE000001", LabelSortKey: "CASE000001",
		OccurrenceID: freshMembers[0].OccurrenceID, ContentVersionID: freshMembers[0].ContentVersionID,
		PageState: "unknown", Endpoint: "begin"})
	require.True(t, snapshotHasIndexedSuppliedText(freshMembers[0]))
	require.True(t, snapshotHasIndexedSuppliedText(freshMembers[1]))
	freshRecord, err := freshCatalog.PackageRecordByOccurrence(ctx, freshPackageID, freshMembers[0].OccurrenceID)
	require.NoError(t, err)
	freshCustodians, _, err := freshCatalog.Custodians(ctx, store.CustodianScope{Kind: "package", PackageID: freshPackageID,
		PackageRecordID: freshRecord.RowID, HasPackageRecordID: true}, false, 10, 0)
	require.NoError(t, err)
	require.Len(t, freshCustodians, 1)
	require.Equal(t, "Synthetic Primary", freshCustodians[0].RawLabel)
	require.Nil(t, freshCustodians[0].PersonID, "canonical person identities stay local to a vault")
}

func TestLoadFileExportReusesCommittedBatesArtifactWithoutAllocatingLabels(t *testing.T) {
	env := newBatesExportFixture(t)
	ctx := t.Context()
	artifact, err := PublishBatesExport(ctx, env.catalog, env.blobs, env.allocationID, env.recipe)
	require.NoError(t, err)
	combinedBytes, _, err := ReadBatesExport(ctx, env.catalog, env.blobs, env.allocationID)
	require.NoError(t, err)
	allocationBefore, err := env.catalog.BatesAllocation(ctx, env.allocationID)
	require.NoError(t, err)

	var archive bytes.Buffer
	written, err := WriteLoadFileExport(ctx, env.catalog, env.blobs, LoadFileExportRequest{
		SnapshotID: allocationBefore.SnapshotID, ProfileID: "export-dat-pdf-v1", BatesAllocationID: env.allocationID,
	}, &archive)
	require.NoError(t, err)
	require.Equal(t, env.allocationID, written.Receipt.BatesAllocationID)
	require.Equal(t, artifact.PageCount, written.Receipt.PageCount)
	require.NotNil(t, written.Crosswalk.CombinedArtifact)
	require.Equal(t, "VOL001/ARTIFACTS/"+env.allocationID+".pdf", written.Crosswalk.CombinedArtifact.RelPath)
	require.Equal(t, artifact.BlobSHA256, written.Crosswalk.CombinedArtifact.SHA256)
	require.Len(t, written.Crosswalk.Members, 1)
	require.Len(t, written.Crosswalk.Members[0].BatesPages, 3)
	require.Equal(t, 1, written.Crosswalk.Members[0].BatesPages[0].ArtifactOutputPage)
	require.Equal(t, 1, written.Crosswalk.Members[0].BatesPages[0].DerivativeOutputPage)
	require.Equal(t, []string{"CON000001", "CON000002", "CON000003"}, []string{
		written.Crosswalk.Members[0].BatesPages[0].Label, written.Crosswalk.Members[0].BatesPages[1].Label,
		written.Crosswalk.Members[0].BatesPages[2].Label,
	})
	roleIndex := slices.IndexFunc(written.Crosswalk.Members[0].Roles, func(role LoadFileExportRole) bool {
		return role.Role == "stamped_pdf"
	})
	require.NotEqual(t, -1, roleIndex)
	require.NotEqual(t, artifact.BlobSHA256, written.Crosswalk.Members[0].Roles[roleIndex].SHA256)
	combinedEntry := readLoadFileZIPEntry(t, archive.Bytes(), written.Crosswalk.CombinedArtifact.RelPath)
	require.Equal(t, combinedBytes, combinedEntry)

	verified, err := VerifyLoadFileExport(ctx, bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.NoError(t, err)
	fileIndex := slices.IndexFunc(verified.Manifest.Records[0].Files, func(file loadfile.FileRef) bool {
		return file.Role == "produced_pdf"
	})
	require.NotEqual(t, -1, fileIndex)
	require.Equal(t, written.Crosswalk.Members[0].Roles[roleIndex].SHA256,
		verified.Manifest.Records[0].Files[fileIndex].SHA256)
	allocationAfter, err := env.catalog.BatesAllocation(ctx, env.allocationID)
	require.NoError(t, err)
	require.Equal(t, allocationBefore, allocationAfter)
}

func TestLoadFileExportBatesProductionRoundTripsThroughFreshVault(t *testing.T) {
	ctx := t.Context()
	env := newBatesExportFixture(t)
	allocation, err := env.catalog.BatesAllocation(ctx, env.allocationID)
	require.NoError(t, err)
	sourceMembers, err := env.catalog.SnapshotMembers(ctx, allocation.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, sourceMembers, 1)
	person, err := env.catalog.CreatePerson(ctx, "Synthetic Bates Custodian", "operator")
	require.NoError(t, err)
	_, err = env.catalog.SetCustodian(ctx, store.CustodianRequest{Scope: store.CustodianScope{Kind: "document",
		NodeID: sourceMembers[0].NodeID, ContentVersionID: sourceMembers[0].ContentVersionID}, PersonID: person.PersonID,
		RawLabel: "Synthetic Bates Custodian", Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic",
		IfMatchRevision: 1})
	require.NoError(t, err)
	artifact, err := PublishBatesExport(ctx, env.catalog, env.blobs, env.allocationID, env.recipe)
	require.NoError(t, err)
	var archive bytes.Buffer
	built, err := WriteLoadFileExport(ctx, env.catalog, env.blobs, LoadFileExportRequest{SnapshotID: allocation.SnapshotID,
		ProfileID: "export-dat-pdf-v1", BatesAllocationID: env.allocationID}, &archive)
	require.NoError(t, err)
	verified, err := VerifyLoadFileExport(ctx, bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.NoError(t, err)
	require.Equal(t, artifact.BlobSHA256, verified.Crosswalk.CombinedArtifact.SHA256)
	crosswalkJSON, err := canonical.Marshal(verified.Crosswalk)
	require.NoError(t, err)
	require.NotContains(t, string(crosswalkJSON), person.PersonID, "source-vault person UUIDs are not portable")
	require.Equal(t, []int{3, 6, 8}, []int{verified.Crosswalk.Members[0].BatesPages[0].SourcePage,
		verified.Crosswalk.Members[0].BatesPages[1].SourcePage, verified.Crosswalk.Members[0].BatesPages[2].SourcePage})

	freshCatalog, freshBlobs, freshPackage, freshMembers := importVerifiedLoadFileArchive(t, archive.Bytes(), verified)
	require.Len(t, freshMembers, 1)
	require.Equal(t, []int{1, 2, 3}, freshMembers[0].SelectedSourcePages,
		"the imported derivative freezes every verified page")
	namespaces, _, _, err := freshCatalog.BatesNamespaces(ctx, "", 10)
	require.NoError(t, err)
	require.Empty(t, namespaces, "import does not mint a local Bates namespace")
	pages, err := freshCatalog.SnapshotBatesPages(ctx, freshPackage.SnapshotID, store.MaxSnapshotPages)
	require.NoError(t, err)
	require.Len(t, pages, 3, "the imported production can immediately enter the Bates workflow")
	rebatesNamespace, err := freshCatalog.EnsureBatesNamespace(ctx, "REB", "", 6)
	require.NoError(t, err)
	rebatesRecipe := batesExportRecipe(rebatesNamespace, 1)
	rebatesRecipe.Restamp = true
	rebatesRecipeSHA, err := rebatesRecipe.SHA256()
	require.NoError(t, err)
	rebatesAllocation, err := freshCatalog.ReserveBatesRange(ctx, store.BatesPlanRequest{
		OperationID: uuid.NewString(), NamespaceID: rebatesNamespace.NamespaceID,
		SnapshotID: freshPackage.SnapshotID, RecipeSHA256: rebatesRecipeSHA, Pages: pages,
	})
	require.NoError(t, err)
	_, err = PublishBatesExport(ctx, freshCatalog, freshBlobs, rebatesAllocation.AllocationID, rebatesRecipe)
	require.NoError(t, err)
	var rebatedArchive bytes.Buffer
	_, err = WriteLoadFileExport(ctx, freshCatalog, freshBlobs, LoadFileExportRequest{
		SnapshotID: freshPackage.SnapshotID, SourcePackageID: freshPackage.PackageID,
		ProfileID: "export-dat-pdf-v1", BatesAllocationID: rebatesAllocation.AllocationID,
	}, &rebatedArchive)
	require.NoError(t, err)
	rebated, err := VerifyLoadFileExport(ctx, bytes.NewReader(rebatedArchive.Bytes()), int64(rebatedArchive.Len()))
	require.NoError(t, err)
	require.Equal(t, "REB000001", exportRecordLabel(rebated.Manifest.Records[0], "loadfile.label.assigned.begin"))
	require.Equal(t, "REB000003", exportRecordLabel(rebated.Manifest.Records[0], "loadfile.label.assigned.end"))
	priorLabels := make([]string, 0, len(rebated.Crosswalk.Members[0].Labels))
	for _, label := range rebated.Crosswalk.Members[0].Labels {
		priorLabels = append(priorLabels, label.Label)
	}
	require.Contains(t, priorLabels, "CON000002", "prior label claims remain in the crosswalk")
	produced := availableRepresentations(freshMembers[0])["produced_pdf"]
	require.Len(t, produced, 1)
	roleIndex := slices.IndexFunc(verified.Crosswalk.Members[0].Roles, func(role LoadFileExportRole) bool {
		return role.Role == "stamped_pdf"
	})
	require.NotEqual(t, -1, roleIndex)
	require.Equal(t, verified.Crosswalk.Members[0].Roles[roleIndex].SHA256, produced[0].BlobSHA256)
	labels, err := freshCatalog.PackageLabels(ctx, freshPackage.PackageID, freshMembers[0].OccurrenceID)
	require.NoError(t, err)
	require.Contains(t, labels, store.PackageLabelRow{PackageID: freshPackage.PackageID, Provenance: "assigned",
		LabelSet: allocation.NamespaceID, Label: "CON000001", LabelSortKey: "CON000001",
		OccurrenceID: freshMembers[0].OccurrenceID, ContentVersionID: freshMembers[0].ContentVersionID,
		PageState: "unknown", Endpoint: "begin"})
	require.Contains(t, labels, store.PackageLabelRow{PackageID: freshPackage.PackageID, Provenance: "assigned",
		LabelSet: allocation.NamespaceID, Label: "CON000002", LabelSortKey: "CON000002",
		OccurrenceID: freshMembers[0].OccurrenceID, ContentVersionID: freshMembers[0].ContentVersionID,
		PageNumber: 2, PageState: "unknown", Endpoint: "page"})
	interior, err := freshCatalog.LookupPackageLabel(ctx, "CON000002", freshPackage.PackageID, "", "assigned")
	require.NoError(t, err)
	require.Len(t, interior, 1, "interior Bates labels remain exact lookup authority after import")
	record, err := freshCatalog.PackageRecordByOccurrence(ctx, freshPackage.PackageID, freshMembers[0].OccurrenceID)
	require.NoError(t, err)
	custodians, _, err := freshCatalog.Custodians(ctx, store.CustodianScope{Kind: "package", PackageID: freshPackage.PackageID,
		PackageRecordID: record.RowID, HasPackageRecordID: true}, false, 10, 0)
	require.NoError(t, err)
	require.Len(t, custodians, 1)
	require.Equal(t, "Synthetic Bates Custodian", custodians[0].RawLabel)
	require.Nil(t, custodians[0].PersonID)
	require.Equal(t, built.Receipt.ArchiveSHA256, verified.Receipt.ArchiveSHA256)
}

func importVerifiedLoadFileArchive(t *testing.T, archive []byte, verified LoadFileExportResult) (
	*store.Store, *blob.Store, store.Package, []store.CollectionSnapshotMember,
) {
	t.Helper()
	ctx := t.Context()
	root, err := loadfile.ExtractZIP(ctx, bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loadfile.RemoveExtractedZIP(root)) })
	freshRoot := t.TempDir()
	catalog, err := store.Open(filepath.Join(freshRoot, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(freshRoot, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	resolver, err := loadfile.NewResolver(ctx, root, nil)
	require.NoError(t, err)
	rootDigest, err := resolver.RootDigest()
	require.NoError(t, err)
	require.NoError(t, resolver.Close())
	var manifestJSON bytes.Buffer
	require.NoError(t, verified.Manifest.WriteJSONL(&manifestJSON))
	written, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(manifestJSON.Bytes()))
	require.NoError(t, err)
	require.NoError(t, catalog.RecordBlob(ctx, written.Hash, written.Size, processingBlobPhysical(t, written)))
	profileJSON, err := canonical.Marshal(verified.Receipt.Profile)
	require.NoError(t, err)
	mappingJSON, err := canonical.Marshal(verified.Receipt.Mapping)
	require.NoError(t, err)
	owner, preflightID := "fresh-bates-vault", uuid.NewString()
	_, err = catalog.PutPackagePreflight(ctx, store.PackagePreflightRecord{PreflightID: preflightID, Owner: owner,
		SourceKind: "root", SourceRef: rootDigest, SourceLocator: root, ProfileJSON: string(profileJSON),
		MappingJSON: string(mappingJSON), ProfileSHA256: verified.Receipt.ProfileSHA256,
		MappingSHA256: verified.Receipt.MappingSHA256, ManifestSHA256: written.Hash,
		ManifestBlobSHA256: written.Hash, CanonicalJSON: []byte(`{}`), DiagnosticsJSON: []byte(`[]`)})
	require.NoError(t, err)
	run, err := catalog.BeginIngest(ctx, "package:loadfile", "fresh-bates-round-trip")
	require.NoError(t, err)
	packageID := uuid.NewString()
	volumes := make([]store.PackageVolume, len(verified.Manifest.Volumes))
	for index, volume := range verified.Manifest.Volumes {
		binding, err := store.PackageVolumeRootBinding(rootDigest, volume.DeclaredRoot)
		require.NoError(t, err)
		volumes[index] = store.PackageVolume{Ordinal: volume.Ordinal, VolumeName: volume.Name,
			DeclaredRoot: volume.DeclaredRoot, MappedRoot: volume.DeclaredRoot, ResolvedRootSHA256: binding}
	}
	_, err = catalog.AdmitPackageImport(ctx, run, store.PackageRequest{PackageID: packageID, Direction: "received",
		PackageName: "fresh-bates-round-trip", ProfileSHA256: verified.Receipt.ProfileSHA256,
		ProfileJSON: string(profileJSON), MappingSHA256: verified.Receipt.MappingSHA256, MappingJSON: string(mappingJSON),
		ManifestSHA256: written.Hash, ManifestBlobSHA256: written.Hash, IngestID: run.ID(), State: "importing", Volumes: volumes},
		store.PackageImportJobRequest{ID: uuid.NewString(), Owner: owner, OperationID: uuid.NewString(),
			RequestSHA256: packageImportTestHash([]byte("fresh-bates-round-trip")), PreflightID: preflightID,
			PackageID: packageID, JobJSON: []byte(`{"accept_partial":false,"index_supplied_text":true,"into":"/","source_kind":"root","source_locator":"` + root + `","total":` + strconv.Itoa(verified.Receipt.RecordCount) + `}`)})
	require.NoError(t, err)
	worker, err := NewPackageImportWorker(PackageImportConfig{Catalog: catalog, Blobs: blobs, Owner: "fresh-worker",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Mutate: func(_ context.Context, fn func() error) error { return fn() }})
	require.NoError(t, err)
	_, err = worker.ProcessOnce(ctx)
	require.NoError(t, err)
	pkg, err := catalog.Package(ctx, packageID)
	require.NoError(t, err)
	require.Equal(t, "complete", pkg.State)
	members, err := catalog.SnapshotMembers(ctx, pkg.SnapshotID, 0, 250)
	require.NoError(t, err)
	return catalog, blobs, pkg, members
}

func TestLoadFileExportCustodianPrecedenceAndOmissionDisclosure(t *testing.T) {
	ctx := t.Context()
	env := newPackageImportTestEnvOptions(t, 1, false, true, true)
	worker, err := NewPackageImportWorker(env.config())
	require.NoError(t, err)
	_, err = worker.ProcessOnce(ctx)
	require.NoError(t, err)
	pkg := mustPackage(t, env)
	snapshot, err := env.Catalog.CollectionSnapshot(ctx, pkg.SnapshotID)
	require.NoError(t, err)
	members, err := env.Catalog.SnapshotMembers(ctx, pkg.SnapshotID, 0, 10)
	require.NoError(t, err)
	require.Len(t, members, 1)
	record, err := env.Catalog.PackageRecordByOccurrence(ctx, pkg.PackageID, members[0].OccurrenceID)
	require.NoError(t, err)

	create := func(scope store.CustodianScope, label, rank, basis string) store.CustodianAssignment {
		t.Helper()
		claim, err := env.Catalog.SetCustodian(ctx, store.CustodianRequest{Scope: scope, RawLabel: label,
			Rank: rank, Basis: basis, SourceRef: "synthetic", IfMatchRevision: 1})
		require.NoError(t, err)
		return claim
	}
	collection := create(store.CustodianScope{Kind: "collection", IngestID: pkg.IngestID},
		"Collection Primary", "primary", "transfer_record")
	document := create(store.CustodianScope{Kind: "document", NodeID: members[0].NodeID,
		ContentVersionID: members[0].ContentVersionID}, "Document Primary", "primary", "operator_assigned")
	packageDefault := create(store.CustodianScope{Kind: "package", PackageID: pkg.PackageID},
		"Package Primary", "primary", "transfer_record")
	recordScope := store.CustodianScope{Kind: "package", PackageID: pkg.PackageID,
		PackageRecordID: record.RowID, HasPackageRecordID: true}
	recordPrimary := create(recordScope, "Record Primary", "primary", "package_column")
	recordAdditional := create(recordScope, "Record Additional", "additional", "package_column")

	primary, claims, omitted, err := loadExportCustodians(ctx, env.Catalog, snapshot, pkg.PackageID, members[0])
	require.NoError(t, err)
	require.Equal(t, recordPrimary.AssignmentID, primary.AssignmentID)
	require.Len(t, claims, 5)
	require.Contains(t, omitted, recordAdditional.AssignmentID)

	for _, step := range []struct {
		retire store.CustodianAssignment
		want   store.CustodianAssignment
	}{
		{recordPrimary, packageDefault},
		{packageDefault, document},
		{document, collection},
	} {
		require.NoError(t, env.Catalog.RetireCustodian(ctx, step.retire.AssignmentID, step.retire.Revision))
		primary, _, _, err = loadExportCustodians(ctx, env.Catalog, snapshot, pkg.PackageID, members[0])
		require.NoError(t, err)
		require.Equal(t, step.want.AssignmentID, primary.AssignmentID)
	}
}

func snapshotHasIndexedSuppliedText(member store.CollectionSnapshotMember) bool {
	for _, representation := range member.Representations {
		if representation.Role == "supplied_text" && representation.Status == "available" &&
			representation.LexicalGenerationID != "" {
			return true
		}
	}
	return false
}

func loadFileArchiveWithExtraEntry(t *testing.T, source []byte) []byte {
	t.Helper()
	input, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
	require.NoError(t, err)
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range input.File {
		reader, err := entry.Open()
		require.NoError(t, err)
		destination, err := writer.CreateHeader(&entry.FileHeader)
		require.NoError(t, err)
		_, err = io.Copy(destination, reader) //nolint:gosec // The source is this test's bounded in-memory archive.
		require.NoError(t, err)
		require.NoError(t, reader.Close())
	}
	extra, err := writer.Create("unexpected.txt")
	require.NoError(t, err)
	_, err = extra.Write([]byte("unexpected"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return output.Bytes()
}

func readLoadFileZIPEntry(t *testing.T, source []byte, name string) []byte {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
	require.NoError(t, err)
	for _, entry := range archive.File {
		if entry.Name != name {
			continue
		}
		reader, err := entry.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		return data
	}
	require.FailNow(t, "ZIP entry is missing", name)
	return nil
}

func TestSafeExportExtensionPreservesASCIIAlphanumericSuffixes(t *testing.T) {
	for name, want := range map[string]string{
		"clip.mp4": ".mp4", "archive.7z": ".7z", "scan.g4": ".g4", "unsafe.my file": ".bin",
	} {
		require.Equal(t, want, safeExportExtension(name, ".bin"), name)
	}
}

func TestApplyLoadFileExportLabelsKeepsEndpointsInOneAssignedSet(t *testing.T) {
	values := make([]string, 13)
	applyLoadFileExportLabels(values, []LoadFileExportLabel{
		{Provenance: "assigned", LabelSet: "REVIEW", Label: "REV000001", Endpoint: "begin"},
		{Provenance: "assigned", LabelSet: "BATES", Label: "BAT000001", Endpoint: "page", PageNumber: 1},
		{Provenance: "assigned", LabelSet: "BATES", Label: "BAT000001", Endpoint: "begin"},
		{Provenance: "assigned", LabelSet: "REVIEW", Label: "REV000099", Endpoint: "end"},
	})
	require.Equal(t, []string{"REVIEW", "REV000001", "REV000099"}, values[6:9])
}
