package processing

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

const xmpRaw = "capture.ARW"

// xmpEnv is one imported photo whose sidecar the worker syncs.
type xmpEnv struct {
	t       *testing.T
	catalog *store.Store
	blobs   *blob.Store
	root    string
	assetID string
}

func newXMPEnv(t *testing.T, files ...photoGroupFile) *xmpEnv {
	t.Helper()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	e := &xmpEnv{t: t, catalog: catalog, blobs: blobs, root: root}
	e.assetID = e.importFiles(append([]photoGroupFile{{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}}, files...)...).Asset.ID
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "image.exif.orientation", Namespace: "image.exif", SourceField: "Orientation", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(int64(1))}}}})
	require.NoError(t, err)
	_, err = catalog.PublishSourceMetadata(t.Context(), e.node(e.raw().NodeID).BlobHash, SourceMetadataExtractorFingerprint, canonical)
	require.NoError(t, err)
	return e
}

func xmpSidecar(attrs, body string) photoGroupFile {
	return photoGroupFile{"capture.xmp", store.PhotoRoleSidecar, "application/rdf+xml", photoSidecarHeader + ` xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/" crs:Exposure="1.25"` + attrs + `>` + body + photoSidecarFooter}
}

// importFiles imports or rescans files from the same camera folder.
func (e *xmpEnv) importFiles(files ...photoGroupFile) store.PhotoImportResult {
	e.t.Helper()
	ctx := e.t.Context()
	group := store.PhotoImportGroup{DestinationID: e.catalog.RootID()}
	for _, file := range files {
		receipt, err := e.blobs.WriteDetailedContext(ctx, strings.NewReader(file.payload))
		require.NoError(e.t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(e.t, err)
		group.Members = append(group.Members, store.PhotoImportMember{Name: file.name, Role: file.role, BlobHash: receipt.Hash, Size: receipt.Size, MediaType: file.mime, OriginalPath: filepath.Join(e.root, "camera", file.name), Physical: store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created}})
	}
	run, err := e.catalog.BeginIngest(ctx, "photo-import", filepath.Join(e.root, "camera"))
	require.NoError(e.t, err)
	imported, err := e.catalog.IngestPhotoGroup(ctx, run, group)
	require.NoError(e.t, err)
	return imported
}

// drain runs the sidecar worker until no original is Pending.
func (e *xmpEnv) drain() {
	e.t.Helper()
	ing := &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}
	for range 5 {
		targets, err := e.catalog.PhotoXMPTargetsAfter(e.t.Context(), "", 100)
		require.NoError(e.t, err)
		if len(targets) == 0 {
			return
		}
		for _, target := range targets {
			require.NoError(e.t, SyncPhotoXMPBackground(e.t.Context(), ing, target.FileID))
		}
	}
	e.t.Fatal("XMP sync did not settle")
}

func (e *xmpEnv) node(id int64) store.Node {
	e.t.Helper()
	n, err := e.catalog.NodeByID(e.t.Context(), id)
	require.NoError(e.t, err)
	return n
}

func (e *xmpEnv) asset() store.PhotoAsset {
	e.t.Helper()
	asset, err := e.catalog.PhotoAssetByID(e.t.Context(), e.assetID)
	require.NoError(e.t, err)
	return asset
}

func (e *xmpEnv) raw() store.PhotoFile {
	e.t.Helper()
	for _, f := range e.asset().Files {
		if f.Role == store.PhotoRoleRAW {
			return f
		}
	}
	e.t.Fatal("photo has no RAW")
	return store.PhotoFile{}
}

// sidecars returns the live sidecar nodes attached to the RAW.
func (e *xmpEnv) sidecars() []store.Node {
	e.t.Helper()
	nodes := []store.Node{}
	raw := e.raw()
	for _, f := range e.asset().Files {
		if f.Role == store.PhotoRoleSidecar && f.SidecarOfID != nil && *f.SidecarOfID == raw.ID {
			if n := e.node(f.NodeID); n.TrashedAt == nil {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes
}

// packet reads the current version of the RAW's only sidecar.
func (e *xmpEnv) packet() (store.Node, string, photoSidecarPacket) {
	e.t.Helper()
	nodes := e.sidecars()
	require.Len(e.t, nodes, 1)
	data, err := readPhotoXMPBytes(e.t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, nodes[0].BlobHash)
	require.NoError(e.t, err)
	packet, err := readPhotoSidecar(e.t.Context(), data)
	require.NoError(e.t, err)
	return nodes[0], string(data), packet
}

func (e *xmpEnv) state(want string) store.PhotoXMPState {
	e.t.Helper()
	prepared, err := e.catalog.PreparePhotoAuthored(e.t.Context(), store.PhotoSetSelection{AssetIDs: []string{e.assetID}})
	require.NoError(e.t, err)
	var state store.PhotoXMPState
	for _, target := range prepared.Targets {
		if target.FileID == e.raw().ID {
			state = *target.XMP
		}
	}
	require.Equal(e.t, want, state.State, state.Reason)
	return state
}

func (e *xmpEnv) edit(patch store.PhotoAuthoredPatch, keywords ...store.PhotoKeywordEdit) {
	e.t.Helper()
	raw, asset := e.raw(), e.asset()
	_, err := e.catalog.EditPhotoAuthoredBatch(e.t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: raw.ID, Revision: raw.Revision, NodeRevision: e.node(raw.NodeID).Revision}}, Patch: patch, Keywords: keywords})
	require.NoError(e.t, err)
}

func (e *xmpEnv) keywords() []string {
	e.t.Helper()
	tags, _, err := e.catalog.NodeTags(e.t.Context(), e.raw().NodeID, 100, 0)
	require.NoError(e.t, err)
	names := []string{}
	for _, tag := range tags {
		names = append(names, tag.Name)
	}
	return names
}

func TestPhotoXMPImportThenWritesNewVersion(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, `<dc:subject><rdf:Bag><rdf:li>Travel</rdf:li></rdf:Bag></dc:subject>`))
	e.drain()
	imported := e.state(store.PhotoXMPStateSynced)
	require.NotNil(t, imported.Receipt, "the import stays undoable")
	assert.Equal(t, store.PhotoAuthored{Rating: 4, Confirmed: store.PhotoConfirmedRating}, e.raw().Authored())
	assert.Equal(t, []string{"Travel"}, e.keywords())
	before, _, _ := e.packet()

	e.edit(store.PhotoAuthoredPatch{Rating: new(5), Caption: new("Harbor")})
	e.state(store.PhotoXMPStatePending)
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	after, data, packet := e.packet()
	assert.Equal(t, before.ID, after.ID)
	assert.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID, "Backfill writes a new sidecar version")
	assert.Equal(t, 5, packet.Rating)
	assert.Equal(t, "Harbor", packet.Caption)
	assert.Equal(t, []string{"Travel"}, packet.Keywords)
	assert.Contains(t, data, `crs:Exposure="1.25"`, "unrelated XMP survives the write")
}

func TestPhotoXMPOutsideEditsMergeAfterManagedWrite(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
	e.drain()
	e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
	e.drain()
	managed, _, _ := e.packet()

	// A rescan of the unchanged outside file keeps Docbank's newer version,
	// even after the version holding those bytes is pruned.
	pruned, err := e.catalog.PruneContentVersions(t.Context(), managed.ID, managed.Revision, store.VersionPruneSelector{KeepNewest: 1}, true)
	require.NoError(t, err)
	require.Equal(t, 1, pruned.DeletedVersions)
	e.importFiles(xmpSidecar(` xmp:Rating="3"`, ""))
	e.drain()
	rescanned, _, _ := e.packet()
	assert.Equal(t, managed.CurrentVersionID, rescanned.CurrentVersionID)
	assert.Equal(t, 5, e.raw().Rating)

	// An outside edit of those bytes merges by property against them.
	e.importFiles(xmpSidecar(` xmp:Rating="3"`, `<dc:description>River</dc:description>`))
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	assert.Equal(t, store.PhotoAuthored{Rating: 5, Caption: "River", Confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedCaption}, e.raw().Authored())
	_, _, packet := e.packet()
	assert.Equal(t, 5, packet.Rating)
	assert.Equal(t, "River", packet.Caption)

	// Changing the same property differently on both sides needs a choice.
	e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
	e.importFiles(xmpSidecar(` xmp:Rating="1"`, `<dc:description>River</dc:description>`))
	e.drain()
	conflict := e.state(store.PhotoXMPStateConflict)
	assert.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "2", Sidecar: "1"}}, conflict.Rows)
	err = ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, "00000000-0000-4000-8000-000000000000", "sidecar")
	require.ErrorIs(t, err, store.ErrStaleRevision)
	err = ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, "sidecar")
	require.NoError(t, err)
	e.state(store.PhotoXMPStateSynced)
	assert.Equal(t, 1, e.raw().Rating)
	assert.Equal(t, "River", e.raw().Caption)
}

func TestPhotoXMPConvergenceThenOneSidedChange(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
	e.drain()
	// Both sides reach rating 4 independently; agreeing needs no write but
	// becomes the new baseline.
	e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
	e.importFiles(xmpSidecar(` xmp:Rating="4"`, ""))
	e.drain()
	agreed, _, _ := e.packet()
	e.state(store.PhotoXMPStateSynced)
	e.importFiles(xmpSidecar(` xmp:Rating="2"`, ""))
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	assert.Equal(t, 2, e.raw().Rating)
	current, _, _ := e.packet()
	assert.NotEqual(t, agreed.CurrentVersionID, current.CurrentVersionID)
}

func TestPhotoXMPUndoImportRewritesSidecar(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, ""))
	e.drain()
	state := e.state(store.PhotoXMPStateSynced)
	_, err := e.catalog.UndoPhotoAuthored(t.Context(), state.Receipt.ReceiptID)
	require.NoError(t, err)
	assert.Equal(t, store.PhotoAuthored{}, e.raw().Authored())
	e.drain()
	_, data, packet := e.packet()
	assert.Zero(t, packet.Confirmed&store.PhotoConfirmedRating)
	assert.NotContains(t, data, "Rating")
	assert.Nil(t, e.state(store.PhotoXMPStateSynced).Receipt)
}

func TestPhotoXMPUnrelatedOutsideEditKeepsImportUndo(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, ""))
	e.drain()
	imported := e.state(store.PhotoXMPStateSynced).Receipt
	require.NotNil(t, imported)
	outside := xmpSidecar(` xmp:Rating="4"`, "")
	outside.payload = strings.ReplaceAll(outside.payload, `crs:Exposure="1.25"`, `crs:Exposure="2.5"`)
	e.importFiles(outside)
	e.drain()
	current := e.state(store.PhotoXMPStateSynced).Receipt
	require.NotNil(t, current, "an unrelated outside edit keeps the imported rating undoable")
	require.Equal(t, imported.ReceiptID, current.ReceiptID)
	_, err := e.catalog.UndoPhotoAuthored(t.Context(), current.ReceiptID)
	require.NoError(t, err)
	e.drain()
	_, data, packet := e.packet()
	assert.Zero(t, packet.Confirmed&store.PhotoConfirmedRating)
	assert.Contains(t, data, `crs:Exposure="2.5"`)
	assert.Nil(t, e.state(store.PhotoXMPStateSynced).Receipt)
}

func TestPhotoXMPMissingSidecarBecomesUnavailable(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, ""))
	e.drain()
	sidecar, data, _ := e.packet()
	e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
	require.NoError(t, e.blobs.Remove(sidecar.BlobHash))
	e.drain()
	assert.Equal(t, "The sidecar content is missing. Restore its data, then refresh XMP.", e.state(store.PhotoXMPStateUnavailable).Reason)
	assert.Equal(t, 5, e.raw().Rating)
	_, err := e.blobs.WriteDetailedContext(t.Context(), strings.NewReader(data))
	require.NoError(t, err)
	e.edit(store.PhotoAuthoredPatch{Rating: new(3)})
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	_, _, packet := e.packet()
	assert.Equal(t, 3, packet.Rating)
}

func TestPhotoXMPReadPermissionFailureRetries(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix file permissions enforced for the current user")
	}
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, ""))
	e.drain()
	sidecar, _, _ := e.packet()
	e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
	path := filepath.Join(e.root, "blobs", sidecar.BlobHash[:2], sidecar.BlobHash)
	require.NoError(t, os.Chmod(path, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0o600)) })
	err := SyncPhotoXMPBackground(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID)
	require.ErrorIs(t, err, os.ErrPermission)
	e.state(store.PhotoXMPStatePending)
	require.NoError(t, os.Chmod(path, 0o600))
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	_, _, packet := e.packet()
	assert.Equal(t, 5, packet.Rating)
}

func TestPhotoXMPGeneratedSidecarStaysDetached(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t)
	e.state(store.PhotoXMPStateNone)
	tag, err := e.catalog.CreateTag(t.Context(), "Harbor")
	require.NoError(t, err)
	e.edit(store.PhotoAuthoredPatch{Rating: new(3)}, store.PhotoKeywordEdit{TagID: tag.ID, Assign: true})
	e.drain()
	generated, _, packet := e.packet()
	assert.Equal(t, xmpRaw+".xmp", generated.Name)
	assert.Equal(t, []string{"Harbor"}, packet.Keywords)

	_, _, err = e.catalog.Trash(t.Context(), generated.ID, generated.Revision)
	require.NoError(t, err)
	raw := e.node(e.raw().NodeID)
	_, _, err = e.catalog.Move(t.Context(), raw.ID, *raw.ParentID, "renamed.ARW", raw.Revision)
	require.NoError(t, err)
	e.drain()
	e.state(store.PhotoXMPStateNone)
	assert.Empty(t, e.sidecars(), "a removed sidecar is not recreated for unchanged decisions")

	e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
	e.drain()
	recreated, _, packet := e.packet()
	assert.Equal(t, "renamed.ARW.xmp", recreated.Name)
	assert.Equal(t, 4, packet.Rating)
}

func TestPhotoXMPRefusesMalformedSidecar(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, photoGroupFile{"capture.xmp", store.PhotoRoleSidecar, "application/rdf+xml", photoSidecarHeader + ` xmp:Rating="4"><xmp:Rating>3</xmp:Rating>` + photoSidecarFooter})
	e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
	e.drain()
	state := e.state(store.PhotoXMPStateInvalid)
	assert.Contains(t, state.Reason, "malformed")
	assert.Equal(t, 2, e.raw().Rating)
	node := e.sidecars()[0]
	assert.Equal(t, int64(1), node.Revision, "a refused packet is not replaced")
}

func TestPhotoXMPExtensionClaimNamesAnotherMember(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t,
		photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"},
		xmpSidecar(` xmlns:ps="http://ns.adobe.com/photoshop/1.0/" ps:SidecarForExtension="JPG" xmp:Rating="2"`, ""))
	e.drain()
	state := e.state(store.PhotoXMPStateInvalid)
	var jpeg string
	for _, f := range e.asset().Files {
		if f.Role == store.PhotoRoleImage {
			jpeg = f.ID
			assert.Zero(t, f.Rating)
		}
	}
	assert.Contains(t, state.Reason, "--sidecar-of-file-id "+jpeg)
	assert.Zero(t, e.raw().Rating)
}

func TestPhotoXMPRotationComposesWithOrientation(t *testing.T) {
	t.Parallel()
	input := store.PhotoExportInput{Authored: store.PhotoAuthored{Rotation: 90, Confirmed: store.PhotoConfirmedRotation}}
	_, written, err := mergePhotoXMPSidecar(t.Context(), nil, input, 6, store.PhotoConfirmedRotation)
	require.NoError(t, err)
	assert.Equal(t, 90, written.Rotation)
	assert.Equal(t, 3, written.Orientation)
	// An editor that rotates only the TIFF orientation reads as a new rotation.
	packet := photoXMPSnapshot(written)
	base := packet
	packet.Orientation = 8
	resolvePhotoXMPRotation(&packet, base, 6)
	assert.Equal(t, 180, packet.Values.Rotation)
	assert.Zero(t, packet.Ignored&store.PhotoConfirmedRotation)
	// A mirrored orientation has no rotation of the original and stays unread.
	packet.Orientation = 4
	resolvePhotoXMPRotation(&packet, base, 6)
	assert.NotZero(t, packet.Ignored&store.PhotoConfirmedRotation)
}

func TestPhotoXMPRejectSentinelIsNeverWritten(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="-1"`, ""))
	e.drain()
	assert.Equal(t, "reject", e.raw().Flag)
	e.edit(store.PhotoAuthoredPatch{Caption: new("Harbor")})
	e.drain()
	_, data, packet := e.packet()
	assert.NotContains(t, data, "-1")
	assert.Equal(t, "reject", packet.Flag)
	assert.Equal(t, "Harbor", packet.Caption)
	assert.Contains(t, data, "Pick")
}

func TestPhotoXMPRepeatedWritesStayBounded(t *testing.T) {
	t.Parallel()
	data := []byte(xmpSidecar(` xmp:Rating="1"`, "").payload)
	var size int
	for i := range 20 {
		input := store.PhotoExportInput{Authored: store.PhotoAuthored{Rating: 3 + i%2, Confirmed: store.PhotoConfirmedRating}}
		written, packet, err := mergePhotoXMPSidecar(t.Context(), data, input, 1, store.PhotoConfirmedRating)
		require.NoError(t, err)
		if i == 0 {
			size = len(written)
		}
		assert.Len(t, written, size, "rewriting a property keeps the existing RDF description")
		data = written
		if i == 19 {
			assert.Equal(t, 4, packet.Rating)
			assert.Contains(t, string(data), `crs:Exposure="1.25"`)
		}
	}
}

func TestPhotoXMPOutsideImportReplacesGeneratedSidecar(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t)
	e.edit(store.PhotoAuthoredPatch{Rating: new(3)})
	e.drain()
	created, _, _ := e.packet()
	before, err := e.catalog.NodeProvenance(t.Context(), created.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, before.Items, 1)
	require.Equal(t, "photo-xmp", before.Items[0].SourceKind)
	outside := xmpSidecar(` xmp:Rating="4"`, "")
	result := e.importFiles(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}, outside)
	require.True(t, result.Added)
	require.Equal(t, created.ID, result.Nodes[1].ID)
	require.NotEqual(t, created.CurrentVersionID, result.Nodes[1].CurrentVersionID)
	require.Len(t, e.sidecars(), 1)
	provenance, err := e.catalog.NodeProvenance(t.Context(), created.ID, 10, 0)
	require.NoError(t, err)
	found := false
	for _, fact := range provenance.Items {
		found = found || fact.SourceKind == "photo-import" && fact.OriginalPath == filepath.Join(e.root, "camera", outside.name)
	}
	require.True(t, found, "outside bytes retain their source path")
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	assert.Equal(t, 4, e.raw().Rating)
	e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
	e.drain()
	e.importFiles(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}, outside)
	e.drain()
	current, _, packet := e.packet()
	assert.Equal(t, created.ID, current.ID)
	assert.Equal(t, 5, packet.Rating)
}
