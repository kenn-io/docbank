package processing

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
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
	e := newXMPStore(t)
	e.assetID = e.importFiles(append([]photoGroupFile{{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}}, files...)...).Asset.ID
	e.extract()
	return e
}

// newXMPStore opens an empty catalog and blob store.
func newXMPStore(t *testing.T) *xmpEnv {
	t.Helper()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	return &xmpEnv{t: t, catalog: catalog, blobs: blobs, root: root}
}

// extract publishes source orientation for every original, as the extractor does.
func (e *xmpEnv) extract() {
	e.t.Helper()
	for _, f := range e.asset().Files {
		if f.Role != store.PhotoRoleSidecar {
			e.orient(e.node(f.NodeID).BlobHash, SourceMetadataExtractorFingerprint, 1)
		}
	}
}

func (e *xmpEnv) legacyExec(statement string, args ...any) {
	e.t.Helper()
	db, err := e.catalog.SQLiteDriver().Open(filepath.Join(e.root, "docbank.db"), sqlite.OpenOptions{Access: sqlite.ReadWriteExisting})
	require.NoError(e.t, err)
	_, err = db.ExecContext(e.t.Context(), statement, args...)
	require.NoError(e.t, err)
	require.NoError(e.t, db.Close())
}

// orient publishes extracted source metadata with an EXIF orientation.
func (e *xmpEnv) orient(hash, fingerprint string, orientation int64) {
	e.t.Helper()
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{{Key: "image.exif.orientation", Namespace: "image.exif", SourceField: "Orientation", Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: new(orientation)}}}})
	require.NoError(e.t, err)
	_, err = e.catalog.PublishSourceMetadata(e.t.Context(), hash, fingerprint, canonical)
	require.NoError(e.t, err)
}

func xmpSidecar(attrs, body string) photoGroupFile {
	return photoGroupFile{"capture.xmp", store.PhotoRoleSidecar, "application/rdf+xml", photoSidecarHeader + ` xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/" crs:Exposure="1.25"` + attrs + `>` + body + photoSidecarFooter}
}

// importFiles imports or rescans files from the same camera folder.
func (e *xmpEnv) importFiles(files ...photoGroupFile) store.PhotoImportResult {
	e.t.Helper()
	return e.importFilesFrom("camera", files...)
}

func (e *xmpEnv) importFilesFrom(folder string, files ...photoGroupFile) store.PhotoImportResult {
	e.t.Helper()
	run, err := e.catalog.BeginIngest(e.t.Context(), "photo-import", filepath.Join(e.root, folder))
	require.NoError(e.t, err)
	return e.importRun(run, folder, files...)
}

// importRun imports files from folder inside an ingest run the caller began.
func (e *xmpEnv) importRun(run store.IngestRun, folder string, files ...photoGroupFile) store.PhotoImportResult {
	e.t.Helper()
	ctx := e.t.Context()
	group := store.PhotoImportGroup{DestinationID: e.catalog.RootID()}
	for _, file := range files {
		receipt, err := e.blobs.WriteDetailedContext(ctx, strings.NewReader(file.payload))
		require.NoError(e.t, err)
		encoding, err := receipt.EncodingName()
		require.NoError(e.t, err)
		group.Members = append(group.Members, store.PhotoImportMember{Name: file.name, Role: file.role, BlobHash: receipt.Hash, Size: receipt.Size, MediaType: file.mime, OriginalPath: filepath.Join(e.root, folder, file.name), Physical: store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, PackEligible: receipt.PackEligible, Created: receipt.Created}})
	}
	imported, err := e.catalog.IngestPhotoGroup(ctx, run, group)
	require.NoError(e.t, err)
	return imported
}

// observe imports an outside packet and runs the worker to settlement.
func (e *xmpEnv) observe(files ...photoGroupFile) {
	e.t.Helper()
	e.importFiles(files...)
	e.drain()
}

// drain runs the sidecar worker until no original is Pending.
func (e *xmpEnv) drain() {
	e.t.Helper()
	ing := &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}
	lane := Backfill[store.PhotoSidecarTarget]{Name: "photo-sidecars", Page: 10, IdleDelay: time.Second, DrainOnce: true, List: e.catalog.PhotoXMPTargetsAfter, Key: store.PhotoSidecarTarget.Cursor, Process: func(ctx context.Context, t store.PhotoSidecarTarget) error {
		return SyncPhotoXMPBackground(ctx, ing, t.FileID)
	}}
	for range 5 {
		require.NoError(e.t, lane.Run(e.t.Context()))
		pending, err := e.catalog.PhotoXMPTargetsAfter(e.t.Context(), "", 10)
		require.NoError(e.t, err)
		if len(pending) == 0 {
			return
		}
	}
	e.t.Fatal("XMP work did not settle across complete Backfill scans")
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
	return e.member(store.PhotoRoleRAW)
}

// sidecars returns live sidecar nodes for the RAW.
func (e *xmpEnv) sidecars() []store.Node {
	e.t.Helper()
	nodes := []store.Node{}
	id := e.raw().ID
	for _, f := range e.asset().Files {
		if f.Role == store.PhotoRoleSidecar && f.SidecarOfID != nil && *f.SidecarOfID == id {
			if n := e.node(f.NodeID); n.TrashedAt == nil {
				nodes = append(nodes, n)
			}
		}
	}
	return nodes
}

// packet reads an original's current sidecar, defaulting to the RAW's.
func (e *xmpEnv) packet() (store.Node, string, photoSidecarPacket) {
	e.t.Helper()
	nodes := e.sidecars()
	require.Len(e.t, nodes, 1)
	reader, _, err := e.blobs.OpenStreamContext(e.t.Context(), nodes[0].BlobHash)
	require.NoError(e.t, err)
	data, err := io.ReadAll(reader)
	require.NoError(e.t, reader.Close())
	require.NoError(e.t, err)
	packet, err := readPhotoSidecar(e.t.Context(), data, photoSidecarEditing)
	require.NoError(e.t, err)
	return nodes[0], string(data), packet
}

func (e *xmpEnv) state(want string) store.PhotoXMPState {
	e.t.Helper()
	return e.stateOf(e.raw().ID, want)
}

func (e *xmpEnv) stateOf(fileID, want string) store.PhotoXMPState {
	e.t.Helper()
	prepared, err := e.catalog.PreparePhotoAuthored(e.t.Context(), store.PhotoSetSelection{AssetIDs: []string{e.assetID}})
	require.NoError(e.t, err)
	var state store.PhotoXMPState
	for _, target := range prepared.Targets {
		if target.FileID == fileID {
			state = *target.XMP
		}
	}
	require.Equal(e.t, want, state.State, state.Reason)
	return state
}

func (e *xmpEnv) member(role string) store.PhotoFile {
	e.t.Helper()
	for _, f := range e.asset().Files {
		if f.Role == role {
			return f
		}
	}
	e.t.Fatalf("photo has no %s", role)
	return store.PhotoFile{}
}

func (e *xmpEnv) edit(patch store.PhotoAuthoredPatch, keywords ...store.PhotoKeywordEdit) store.PhotoAuthoredReceipt {
	e.t.Helper()
	raw, asset := e.raw(), e.asset()
	receipt, err := e.catalog.EditPhotoAuthoredBatch(e.t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: raw.ID, Revision: raw.Revision, NodeRevision: e.node(raw.NodeID).Revision}}, Patch: patch, Keywords: keywords})
	require.NoError(e.t, err)
	return receipt
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

func TestPhotoXMPSyncClearsUndoneRatingAndKeywords(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t)
	tag, err := e.catalog.CreateTag(t.Context(), "Travel")
	require.NoError(t, err)
	receipt := e.edit(store.PhotoAuthoredPatch{Rating: new(4)}, store.PhotoKeywordEdit{TagID: tag.ID, Assign: true})
	e.drain()
	before, _, packet := e.packet()
	assert.Equal(t, 4, packet.Rating)
	assert.Equal(t, []string{"Travel"}, packet.Keywords)
	_, err = e.catalog.UndoPhotoAuthored(t.Context(), receipt.ReceiptID)
	require.NoError(t, err)
	e.drain()
	after, data, packet := e.packet()
	assert.Equal(t, before.ID, after.ID)
	assert.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
	assert.Zero(t, packet.Rating)
	assert.Empty(t, packet.Keywords)
	assert.NotContains(t, data, "Travel")
	assert.Zero(t, e.raw().Rating)
	assert.Empty(t, e.keywords())
	e.state(store.PhotoXMPStateSynced)
}

func (e *xmpEnv) removeSidecarForSync(mode string) func() {
	e.t.Helper()
	t := e.t
	sidecar := e.sidecars()[0]
	switch mode {
	case "trash":
		_, _, err := e.catalog.Trash(t.Context(), sidecar.ID, sidecar.Revision)
		require.NoError(t, err)
		return func() {
			_, _, err := e.catalog.Restore(t.Context(), sidecar.ID, store.UnconditionalRev)
			require.NoError(t, err)
			e.drain()
			require.Len(t, e.sidecars(), 1)
		}
	case "detach":
		asset, file := e.asset(), e.member(store.PhotoRoleSidecar)
		_, err := e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, file.ID, store.PhotoDetachOptions{})
		require.NoError(t, err)
	case "original":
		raw, asset := e.raw(), e.asset()
		asset, err := e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, raw.ID, store.PhotoDetachOptions{ClearDependentSidecars: true})
		require.NoError(t, err)
		_, err = e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, raw.NodeID, store.PhotoRoleRAW, nil)
		require.NoError(t, err)
	}
	return func() {}
}

func xmpRemovalFixture(t *testing.T, first bool) *xmpEnv {
	t.Helper()
	e := newXMPEnv(t, xmpSidecar("", ""))
	e.state(store.PhotoXMPStatePending)
	tag, err := e.catalog.CreateTag(t.Context(), "Harbor")
	require.NoError(t, err)
	e.edit(store.PhotoAuthoredPatch{Rating: new(3)}, store.PhotoKeywordEdit{TagID: tag.ID, Assign: true})
	if !first {
		e.drain()
	}
	return e
}

// An edit around the removal of an unsynced or synced sidecar settles on the edit.
func TestPhotoXMPSidecarRemoval(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ first, editBefore bool }{{true, true}, {false, true}, {true, false}} {
		t.Run("first="+strconv.FormatBool(tc.first)+"/editBeforeRemoval="+strconv.FormatBool(tc.editBefore), func(t *testing.T) {
			e := xmpRemovalFixture(t, tc.first)
			if tc.editBefore {
				e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
			}
			restore := e.removeSidecarForSync("trash")
			if !tc.editBefore {
				e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
			}
			e.drain()
			if tc.editBefore {
				e.state(store.PhotoXMPStateNone)
				assert.Equal(t, 5, e.raw().Rating)
				assert.Empty(t, e.sidecars())
				restore()
				return
			}
			e.state(store.PhotoXMPStateSynced)
			_, _, packet := e.packet()
			assert.Equal(t, 5, packet.Rating)
		})
	}
}

// A removed sidecar stays removed through a rename and reconfirmed defaults,
// and comes back, named for the renamed original, on the next real change.
func TestPhotoXMPSidecarCreationAndRecreation(t *testing.T) {
	t.Parallel()
	rating := store.PhotoConfirmedRating | store.PhotoXMPKeywordsBit
	for _, r := range []struct {
		name, mode               string
		initial, pending, change store.PhotoAuthoredPatch
		keywords                 bool
		present                  store.PhotoAuthoredFields
	}{
		{name: "rating zero", initial: store.PhotoAuthoredPatch{Rating: new(0)}},
		{name: "unflag", initial: store.PhotoAuthoredPatch{Flag: new("")}},
		{name: "empty caption", initial: store.PhotoAuthoredPatch{Caption: new("")}},
		{name: "empty label", initial: store.PhotoAuthoredPatch{Label: new("")}},
		{name: "rating three then zero", initial: store.PhotoAuthoredPatch{Rating: new(3)}, pending: store.PhotoAuthoredPatch{Rating: new(0)}},
		{name: "pick only", initial: store.PhotoAuthoredPatch{Flag: new("pick")}, present: store.PhotoConfirmedFlag},
		{"recreate after trash", "trash", store.PhotoAuthoredPatch{Rating: new(3)}, store.PhotoAuthoredPatch{}, store.PhotoAuthoredPatch{Rating: new(0)}, true, rating},
		{"detach, edit without rename", "detach without rename", store.PhotoAuthoredPatch{Rating: new(3)}, store.PhotoAuthoredPatch{}, store.PhotoAuthoredPatch{Rating: new(5)}, false, store.PhotoConfirmedRating},
		{"recreate after detach", "detach", store.PhotoAuthoredPatch{Rating: new(3)}, store.PhotoAuthoredPatch{}, store.PhotoAuthoredPatch{Rating: new(0)}, true, rating},
		{"recreate after the original changes", "original", store.PhotoAuthoredPatch{Rating: new(3)}, store.PhotoAuthoredPatch{Rating: new(5)}, store.PhotoAuthoredPatch{Rating: new(0)}, true, rating},
		{"recreate after confirmed defaults", "trash", store.PhotoAuthoredPatch{Caption: new("Before")}, store.PhotoAuthoredPatch{Rating: new(0), Caption: new("")}, store.PhotoAuthoredPatch{Label: new("blue")}, false, store.PhotoConfirmedLabel},
	} {
		t.Run(r.name, func(t *testing.T) {
			e := newXMPEnv(t, photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"})
			var assigned, cleared []store.PhotoKeywordEdit
			if r.keywords {
				tag, err := e.catalog.CreateTag(t.Context(), "Harbor")
				require.NoError(t, err)
				assigned = []store.PhotoKeywordEdit{{TagID: tag.ID, Assign: true}}
				cleared = []store.PhotoKeywordEdit{{TagID: tag.ID, Assign: false}}
			}
			e.edit(r.initial, assigned...)
			if r.mode == "" {
				if r.pending.Rating != nil {
					e.edit(r.pending)
				}
				e.drain()
				if r.present == 0 {
					assert.Empty(t, e.sidecars())
					e.state(store.PhotoXMPStateNone)
				} else {
					e.state(store.PhotoXMPStateSynced)
					_, data, packet := e.packet()
					assert.Equal(t, "pick", packet.Flag)
					assert.Equal(t, r.present, packet.Present)
					assert.NotContains(t, data, "xmp:Rating")
				}
				return
			}
			e.drain()
			if r.pending.Rating != nil {
				e.edit(r.pending)
			}
			before, _, _ := e.packet()
			file := e.member(store.PhotoRoleSidecar)
			e.removeSidecarForSync(strings.TrimSuffix(r.mode, " without rename"))
			e.drain()
			e.state(store.PhotoXMPStateNone)
			require.Empty(t, e.sidecars())
			if r.mode == "detach without rename" {
				require.Equal(t, xmpRaw+".xmp", e.node(before.ID).Name)
				e.edit(r.change)
				e.drain()
				e.state(store.PhotoXMPStateSynced)
				after, _, packet := e.packet()
				require.Equal(t, 5, packet.Rating)
				require.Equal(t, before.ID, after.ID)
				require.Equal(t, file.ID, e.member(store.PhotoRoleSidecar).ID)
				require.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
				return
			}
			// Reconfirming the same defaults and renaming do not recreate a sidecar.
			if !r.keywords {
				e.edit(store.PhotoAuthoredPatch{Rating: new(0), Caption: new("")})
			}
			raw := e.node(e.raw().NodeID)
			_, _, err := e.catalog.Move(t.Context(), raw.ID, *raw.ParentID, "renamed.ARW", raw.Revision)
			require.NoError(t, err)
			e.drain()
			e.state(store.PhotoXMPStateNone)
			assert.Empty(t, e.sidecars())
			e.edit(r.change, cleared...)
			e.drain()
			e.state(store.PhotoXMPStateSynced)
			recreated, _, packet := e.packet()
			assert.Equal(t, "renamed.ARW.xmp", recreated.Name)
			assert.Zero(t, packet.Rating)
			assert.Empty(t, packet.Caption)
			assert.Empty(t, packet.Keywords)
			assert.Equal(t, r.present, packet.Present&r.present)
			if r.change.Label != nil {
				assert.Equal(t, *r.change.Label, packet.Label)
			}
		})
	}
}

func TestPhotoXMPRotationComposesWithOrientation(t *testing.T) {
	t.Parallel()
	input := store.PhotoExportInput{Authored: store.PhotoAuthored{Rotation: 90, Confirmed: store.PhotoConfirmedRotation}}
	_, written, err := rewritePhotoXMPSidecar(t.Context(), nil, input, 6, store.PhotoConfirmedRotation)
	require.NoError(t, err)
	_, generated, err := rewritePhotoXMPSidecar(t.Context(), nil, input, 1, store.PhotoConfirmedRotation)
	require.NoError(t, err)
	for _, test := range []struct {
		name                          string
		rotation, orientation, source int
		base                          store.PhotoXMPPacket
		want                          *int
		omitted                       bool
	}{
		{"orientation-only edit", 90, 8, 6, written.PhotoXMPPacket, new(180), false},
		{"mirrored orientation", 90, 4, 6, written.PhotoXMPPacket, nil, false},
		{"teststrip180", 180, 6, 1, store.PhotoXMPPacket{Rotation: new(90), Orientation: 6, PhotoAuthored: store.PhotoAuthored{Rotation: 90}}, new(180), false},
		{"teststrip0", 0, 6, 1, store.PhotoXMPPacket{Rotation: new(90), Orientation: 6, PhotoAuthored: store.PhotoAuthored{Rotation: 90}}, new(0), false},
		{"conflicting orientation", 90, 8, 6, store.PhotoXMPPacket{}, nil, false},
		{"ts:Rotation removed, orientation changed", 0, 3, 1, generated.PhotoXMPPacket, new(180), true},
		{"ts:Rotation kept, orientation changed", 90, 3, 1, generated.PhotoXMPPacket, new(180), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			packet := store.PhotoXMPPacket{Rotation: new(test.rotation), Orientation: test.orientation, PhotoAuthored: store.PhotoAuthored{Rotation: test.rotation, Confirmed: store.PhotoConfirmedRotation}}
			if test.omitted {
				packet.Rotation, packet.Confirmed = nil, 0
			}
			err := resolvePhotoXMPRotation(&packet, test.base, test.source)
			if test.want == nil {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, *test.want, packet.PhotoAuthored.Rotation)
			}
		})
	}
}

func TestPhotoXMPOutsideImportReplacesGeneratedSidecar(t *testing.T) {
	t.Parallel()
	raw := photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}
	e := newXMPEnv(t)
	e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
	e.drain()
	created, _, _ := e.packet()
	before, err := e.catalog.NodeProvenance(t.Context(), created.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, before.Items, 1)
	require.Equal(t, "photo-xmp", before.Items[0].SourceKind)
	outside := xmpSidecar(` xmp:Rating="4"`, "")
	result := e.importFiles(raw, outside)
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
	for _, rating := range []int{5, 2} {
		outside = xmpSidecar(` xmp:Rating="`+strconv.Itoa(rating)+`"`, "")
		result = e.importFiles(raw, outside)
		require.Equal(t, created.ID, result.Nodes[1].ID, "later outside edits keep the attachment")
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		current, _, packet := e.packet()
		assert.Equal(t, created.ID, current.ID)
		assert.Equal(t, rating, packet.Rating)
		assert.Equal(t, packet.Rating, e.raw().Rating)
	}
}

func TestPhotoXMPSourceReplacementKeepsAuthoredRotation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, attrs, body, caption                     string
		rotation, sourceOrientation, packetOrientation int
		importSource                                   bool
	}{
		{"import", ` ts:Rotation="90"`, "", "River", 90, 0, 0, true},
		{"replace", ` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" tiff:Orientation="6" ts:Rotation="0"`, `<dc:description>First</dc:description>`, "Second", 0, 6, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			e := newXMPEnv(t, xmpSidecar(test.attrs, test.body))
			if test.sourceOrientation != 0 {
				e.orient(e.node(e.raw().NodeID).BlobHash, strings.Repeat("b", 64), int64(test.sourceOrientation))
			}
			e.drain()
			require.Equal(t, test.rotation, e.raw().Rotation)
			before := e.node(e.raw().NodeID)
			if test.importSource {
				e.importFiles(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "replacement RAW"})
				e.orient(e.node(e.raw().NodeID).BlobHash, SourceMetadataExtractorFingerprint, 6)
			} else {
				hash, _, err := e.blobs.Write(strings.NewReader("replacement source"))
				require.NoError(t, err)
				_, _, err = e.catalog.ReplaceContent(t.Context(), before.ID, before.Revision, hash, 18, "image/x-sony-arw")
				require.NoError(t, err)
				canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{}})
				require.NoError(t, err)
				_, err = e.catalog.PublishSourceMetadata(t.Context(), hash, SourceMetadataExtractorFingerprint, canonical)
				require.NoError(t, err)
			}
			current := e.node(e.raw().NodeID)
			require.Equal(t, before.ID, current.ID)
			require.NotEqual(t, before.CurrentVersionID, current.CurrentVersionID)
			e.drain()
			_, _, packet := e.packet()
			assert.Equal(t, test.rotation, packet.PhotoAuthored.Rotation)
			assert.Equal(t, test.rotation, e.raw().Rotation)
			e.observe(xmpSidecar(test.attrs, `<dc:description>`+test.caption+`</dc:description>`))
			e.state(store.PhotoXMPStateSynced)
			assert.Equal(t, test.caption, e.raw().Caption)
			assert.Equal(t, test.rotation, e.raw().Rotation)
			_, _, packet = e.packet()
			require.NotNil(t, packet.Rotation)
			assert.Equal(t, test.rotation, *packet.Rotation)
			if test.packetOrientation != 0 {
				assert.Equal(t, test.packetOrientation, packet.Orientation)
			}
		})
	}
}

func TestPhotoXMPSidecarWritesScalars(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		packet               []byte
		flag, label, caption string
	}{
		"new":      {flag: "pick", label: "red", caption: "Harbor"},
		"cleared":  {packet: []byte(photoSidecarHeader + ` ts:Pick="pick" xmp:Label="red">` + photoSidecarFooter)},
		"reject":   {flag: "reject", label: "red", packet: []byte(photoSidecarHeader + ` ts:Pick="reject">` + photoSidecarFooter)},
		"elements": {flag: "pick", label: "red", packet: []byte(photoSidecarHeader + `><xmp:Rating>2</xmp:Rating><ts:Pick>reject</ts:Pick><dc:description><rdf:Alt><rdf:li xml:lang="x-default">River</rdf:li></rdf:Alt></dc:description>` + photoSidecarFooter)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			values := store.PhotoAuthored{Rating: 4, Flag: test.flag, Label: test.label, Rotation: 90, Confirmed: store.PhotoConfirmedRating | store.PhotoConfirmedFlag | store.PhotoConfirmedLabel | store.PhotoConfirmedRotation}
			values.Caption, values.Confirmed = test.caption, values.Confirmed|store.PhotoConfirmedCaption
			data, parsed, err := rewritePhotoXMPSidecar(t.Context(), test.packet, store.PhotoExportInput{Authored: values}, 6, store.PhotoConfirmedAll)
			require.NoError(t, err)
			assert.Equal(t, test.caption, parsed.Caption)
			// Teststrip reads these properties from rdf:Description attributes.
			decoder := xml.NewDecoder(bytes.NewReader(data))
			attributes := map[xml.Name]string{}
			for {
				token, err := decoder.Token()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				if start, ok := token.(xml.StartElement); ok && start.Name == (xml.Name{Space: rdfNamespace, Local: "Description"}) {
					for _, attribute := range start.Attr {
						attributes[attribute.Name] = attribute.Value
					}
				}
			}
			rating := "4"
			if test.flag == "reject" {
				rating = "-1"
			}
			assert.Equal(t, rating, attributes[xml.Name{Space: xmpBasicNamespace, Local: "Rating"}])
			if test.flag == "pick" {
				assert.Equal(t, "pick", attributes[xml.Name{Space: teststripXMPNamespace, Local: "Pick"}])
			}
			assert.Equal(t, test.label, attributes[xml.Name{Space: xmpBasicNamespace, Local: "Label"}])
			assert.Equal(t, "90", attributes[xml.Name{Space: teststripXMPNamespace, Local: "Rotation"}])
			if test.flag != "pick" {
				assert.NotContains(t, attributes, xml.Name{Space: teststripXMPNamespace, Local: "Pick"})
			}
			if test.label == "" {
				assert.NotContains(t, attributes, xml.Name{Space: xmpBasicNamespace, Local: "Label"})
			}
		})
	}
}

// An outside editor changes rating 3 to 4 after Docbank wrote 5 and the
// sidecar's history was pruned or its folder moved.
func TestPhotoXMPOutsideEditConflictsAfterDisturbance(t *testing.T) {
	t.Parallel()
	for _, disturbance := range []string{"prune", "move"} {
		t.Run(disturbance, func(t *testing.T) {
			initial := xmpSidecar(` xmp:Rating="3"`, "")
			e := newXMPEnv(t, initial)
			e.drain()
			e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
			e.drain()
			managed, _, _ := e.packet()
			if disturbance == "prune" {
				_, err := e.catalog.PruneContentVersions(t.Context(), managed.ID, managed.Revision, store.VersionPruneSelector{KeepNewest: 1}, true)
				require.NoError(t, err)
				e.drain()
				e.state(store.PhotoXMPStateSynced)
			} else {
				e.root = filepath.Join(e.root, "moved")
				e.observe(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}, initial)
				require.Len(t, e.sidecars(), 1)
			}
			e.observe(xmpSidecar(` xmp:Rating="4"`, ""))
			assert.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, e.state(store.PhotoXMPStateConflict).Rows)
			assert.Equal(t, 5, e.raw().Rating)
			if disturbance == "prune" {
				outside := e.sidecars()[0]
				e.importFiles(initial)
				assert.NotEqual(t, outside.CurrentVersionID, e.sidecars()[0].CurrentVersionID, "an outside head treats older bytes as a reversion")
				e.drain()
				e.state(store.PhotoXMPStateConflict)
			} else {
				require.Len(t, e.sidecars(), 1, "a moved source must replace its accepted sidecar")
				current, _, _ := e.packet()
				assert.Equal(t, managed.ID, current.ID)
				assert.NotEqual(t, managed.CurrentVersionID, current.CurrentVersionID)
			}
		})
	}
}

func TestPhotoXMPLightroomKeywordAliases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, leaf, normalizedLeaf string
		changed, normalized        bool
	}{
		{"unchanged aliases", "Old", "Old", false, false},
		{"changed aliases", "Old", "Old", true, false},
		{"decomposed leaf", "Cafe\u0301", "Café", true, true},
		{"leading space", " Leading", " Leading", true, true},
		{"trailing space", "Trailing ", "Trailing ", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packet := xmpSidecar(` xmp:Rating="3" xmlns:lr="http://ns.adobe.com/lightroom/1.0/" lr:hierarchicalSubject="Places|Old" lr:weightedFlatSubject="Old"`, `<dc:subject><rdf:Bag><rdf:li>Old</rdf:li><rdf:li>Gone</rdf:li></rdf:Bag></dc:subject><lr:hierarchicalSubject><rdf:Bag><rdf:li>Places|Old</rdf:li><rdf:li>Places|Gone</rdf:li></rdf:Bag></lr:hierarchicalSubject><lr:weightedFlatSubject><rdf:Bag><rdf:li>Old</rdf:li><rdf:li>Gone</rdf:li></rdf:Bag></lr:weightedFlatSubject><crs:History lr:hierarchicalSubject="Nested hierarchy"/>`)
			packet.payload = strings.Replace(packet.payload, photoSidecarFooter, `</rdf:Description><rdf:Description rdf:about="other" xmlns:lr="http://ns.adobe.com/lightroom/1.0/" lr:hierarchicalSubject="Other hierarchy"><lr:weightedFlatSubject>Other weighted</lr:weightedFlatSubject>`+photoSidecarFooter, 1)
			if tc.normalized {
				packet.payload = strings.ReplaceAll(packet.payload, "Old", tc.leaf)
				packet.payload = strings.ReplaceAll(packet.payload, "Gone", "Stale")
				packet.payload = strings.Replace(packet.payload, `<rdf:li>Stale</rdf:li>`, "", 1)
			}
			before, err := readPhotoSidecar(t.Context(), []byte(packet.payload), photoSidecarEditing)
			require.NoError(t, err)
			input := store.PhotoExportInput{Authored: before.PhotoAuthored, Keywords: before.Keywords}
			mask := store.PhotoConfirmedRating
			input.Authored.Rating = 5
			if tc.changed {
				mask = store.PhotoXMPKeywordsBit
				input.Keywords = store.PhotoXMPKeywords([]string{"New", tc.normalizedLeaf})
			}
			data, parsed, err := rewritePhotoXMPSidecar(t.Context(), []byte(packet.payload), input, 1, mask)
			require.NoError(t, err)
			output := string(data)
			assert.Contains(t, output, `hierarchicalSubject="Places|`+tc.leaf+`"`)
			assert.Contains(t, output, `weightedFlatSubject="`+tc.leaf+`"`)
			assert.Contains(t, output, "Places|"+tc.leaf+"</rdf:li>")
			if !tc.normalized {
				assert.Equal(t, !tc.changed, strings.Contains(output, "Places|Gone"), output)
			}
			for _, value := range []string{"Nested hierarchy", "Other hierarchy", "Other weighted"} {
				assert.Contains(t, output, value)
			}
			if tc.normalized {
				assert.Contains(t, output, `>`+tc.leaf+`</rdf:li>`)
				assert.NotContains(t, output, "Stale")
			} else if tc.changed {
				assert.Equal(t, []string{"New", "Old"}, parsed.Keywords)
			}
		})
	}
}

// A flat keyword may contain "|"; only lr:hierarchicalSubject paths reduce to their leaf.
func TestPhotoXMPLightroomFlatKeywordsAreNotPaths(t *testing.T) {
	t.Parallel()
	packet := func(items ...string) photoGroupFile {
		var bag strings.Builder
		for _, item := range items {
			bag.WriteString("<rdf:li>" + item + "</rdf:li>")
		}
		return xmpSidecar(` xmp:Rating="3" xmlns:lr="http://ns.adobe.com/lightroom/1.0/"`, `<dc:subject><rdf:Bag>`+bag.String()+`</rdf:Bag></dc:subject><lr:weightedFlatSubject><rdf:Bag>`+bag.String()+`</rdf:Bag></lr:weightedFlatSubject>`)
	}
	t.Run("a worker edit keeps a flat keyword with a bar", func(t *testing.T) {
		e := newXMPEnv(t, packet("A|B", "Garden"))
		e.drain()
		require.ElementsMatch(t, []string{"A|B", "Garden"}, e.keywords())
		tags, _, err := e.catalog.NodeTags(t.Context(), e.raw().NodeID, 100, 0)
		require.NoError(t, err)
		var edits []store.PhotoKeywordEdit
		for _, tag := range tags {
			if tag.Name == "Garden" {
				edits = append(edits, store.PhotoKeywordEdit{TagID: tag.ID, Assign: false})
			}
		}
		tree, err := e.catalog.CreateTag(t.Context(), "Tree")
		require.NoError(t, err)
		e.edit(store.PhotoAuthoredPatch{}, append(edits, store.PhotoKeywordEdit{TagID: tree.ID, Assign: true})...)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		_, data, parsed := e.packet()
		require.ElementsMatch(t, []string{"A|B", "Tree"}, e.keywords())
		assert.Contains(t, data, "<lr:weightedFlatSubject><rdf:Bag><rdf:li>A|B</rdf:li>")
		assert.Contains(t, data, "Tree")
		assert.NotContains(t, data, "Garden")
		assert.Equal(t, []string{"A|B", "Tree"}, parsed.Keywords)
	})
	t.Run("a remaining leaf does not keep a removed flat keyword", func(t *testing.T) {
		source := packet("A|B")
		input := store.PhotoExportInput{Authored: store.PhotoAuthored{Rating: 3}, Keywords: []string{"B"}}
		data, _, err := rewritePhotoXMPSidecar(t.Context(), []byte(source.payload), input, 1, store.PhotoXMPKeywordsBit)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "A|B")
	})
}

func TestPhotoXMPUnrepresentableTextRefusal(t *testing.T) {
	t.Parallel()
	t.Run("caption control", func(t *testing.T) {
		e := newXMPEnv(t)
		e.legacyExec(`UPDATE photo_files SET caption=?,confirmed_fields=?,revision=2 WHERE file_id=?`, "legacy\x01", store.PhotoConfirmedCaption, e.raw().ID)
		e.legacyExec(`UPDATE nodes SET revision=revision+1,modified_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), e.raw().NodeID)
		e.drain()
		e.state(store.PhotoXMPStateInvalid)
		assert.Equal(t, "legacy\x01", e.raw().Caption)
		assert.Empty(t, e.sidecars())
		e.edit(store.PhotoAuthoredPatch{Caption: new("Corrected")})
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		_, _, packet := e.packet()
		assert.Equal(t, "Corrected", packet.Caption)
	})
	t.Run("keyword U+FFFE", func(t *testing.T) {
		e := newXMPEnv(t)
		tag, err := e.catalog.CreateTag(t.Context(), "keyword\ufffe")
		require.NoError(t, err)
		e.edit(store.PhotoAuthoredPatch{Rating: new(4)}, store.PhotoKeywordEdit{TagID: tag.ID, Assign: true})
		e.drain()
		e.state(store.PhotoXMPStateInvalid)
		e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
		e.drain()
		assert.Equal(t, 5, e.raw().Rating)
		assert.Equal(t, []string{"keyword\ufffe"}, e.keywords())
		e.state(store.PhotoXMPStateInvalid)
		assert.Empty(t, e.sidecars())
	})
	t.Run("oversized keyword correction", func(t *testing.T) {
		e := newXMPEnv(t)
		tag, err := e.catalog.CreateTag(t.Context(), strings.Repeat("x", store.MaxPhotoAuthoredTextBytes+1))
		require.NoError(t, err)
		raw := e.raw()
		_, err = e.catalog.AssignTag(t.Context(), tag.ID, raw.NodeID, e.node(raw.NodeID).Revision)
		require.NoError(t, err)
		e.edit(store.PhotoAuthoredPatch{Rating: new(3)})
		e.drain()
		e.state(store.PhotoXMPStateInvalid)
		tag, err = e.catalog.TagByID(t.Context(), tag.ID)
		require.NoError(t, err)
		_, err = e.catalog.RenameTag(t.Context(), tag.ID, tag.Revision, "Short keyword")
		require.NoError(t, err)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		_, err = e.catalog.UnassignTag(t.Context(), tag.ID, raw.NodeID, e.node(raw.NodeID).Revision)
		require.NoError(t, err)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		_, _, packet := e.packet()
		require.Empty(t, packet.Keywords)
		require.NotZero(t, packet.Present&store.PhotoXMPKeywordsBit)
	})
}

func TestPhotoXMPConflictSurvivesRetryOutcomes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		retry func(*testing.T, *xmpEnv, photoGroupFile, store.PhotoXMPState)
	}{
		{"invalid packet", func(t *testing.T, e *xmpEnv, outside photoGroupFile, conflict store.PhotoXMPState) {
			t.Helper()
			broken := outside
			broken.payload = "<broken>"
			e.observe(broken)
			invalid := e.state(store.PhotoXMPStateInvalid)
			assert.Equal(t, conflict.Rows, invalid.Rows)
			assert.NotEqual(t, conflict.ConflictID, invalid.ConflictID)
		}},
		{"unavailable bytes", func(t *testing.T, e *xmpEnv, outside photoGroupFile, conflict store.PhotoXMPState) {
			t.Helper()
			sc := e.sidecars()[0]
			require.NoError(t, e.blobs.Remove(sc.BlobHash))
			e.edit(store.PhotoAuthoredPatch{Caption: new("Retry")})
			require.Error(t, SyncPhotoXMPBackground(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID))
			pending := e.state(store.PhotoXMPStatePending)
			assert.NotEmpty(t, pending.Reason)
			assert.Equal(t, conflict.Rows, pending.Rows)
			assert.NotEqual(t, conflict.ConflictID, pending.ConflictID)
			for range 2 {
				require.Error(t, SyncPhotoXMPBackground(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID))
				require.Equal(t, pending, e.state(store.PhotoXMPStatePending))
			}
			_, err := e.blobs.WriteDetailedContext(t.Context(), strings.NewReader(outside.payload))
			require.NoError(t, err)
			e.drain()
			e.state(store.PhotoXMPStateConflict)
		}},
		{"removed sidecar", func(t *testing.T, e *xmpEnv, outside photoGroupFile, conflict store.PhotoXMPState) {
			t.Helper()
			sc := e.sidecars()[0]
			_, _, err := e.catalog.Trash(t.Context(), sc.ID, sc.Revision)
			require.NoError(t, err)
			e.drain()
			require.Equal(t, conflict.Rows, e.state(store.PhotoXMPStateUnavailable).Rows)
			_, _, err = e.catalog.Restore(t.Context(), sc.ID, store.UnconditionalRev)
			require.NoError(t, err)
			e.drain()
			current := e.state(store.PhotoXMPStateConflict)
			_, err = ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, current.ConflictID, "catalog")
			require.NoError(t, err)
			e.state(store.PhotoXMPStateSynced)
		}},
		{"two sidecars on one original", func(t *testing.T, e *xmpEnv, outside photoGroupFile, conflict store.PhotoXMPState) {
			t.Helper()
			extraSidecar := xmpSidecar(` xmp:Rating="2"`, "")
			extraSidecar.name = "capture.ARW.xmp"
			e.importFiles(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}, outside, extraSidecar)
			e.drain()
			refused := e.state(store.PhotoXMPStateInvalid)
			require.NotEmpty(t, refused.Reason)
			require.Equal(t, conflict.Rows, refused.Rows)
			require.Equal(t, 5, e.raw().Rating)
			asset := e.asset()
			var extraFile store.PhotoFile
			for _, f := range asset.Files {
				if f.Role == store.PhotoRoleSidecar && e.node(f.NodeID).Name == extraSidecar.name {
					extraFile = f
				}
			}
			_, err := e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, extraFile.ID, store.PhotoDetachOptions{})
			require.NoError(t, err)
			e.drain()
			require.Equal(t, conflict.Rows, e.state(store.PhotoXMPStateConflict).Rows)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
			e.drain()
			e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
			e.drain()
			outside := xmpSidecar(` xmp:Rating="4"`, "")
			e.observe(outside)
			conflict := e.state(store.PhotoXMPStateConflict)
			test.retry(t, e, outside, conflict)
		})
	}
}

// photoXMPResourceTargets lists every rdf:resource; sync refuses xml:base, so each is final as written.
func photoXMPResourceTargets(t *testing.T, data string) []string {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(data))
	var targets []string
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return targets
		}
		require.NoError(t, err)
		if element, ok := token.(xml.StartElement); ok {
			for _, attr := range element.Attr {
				if attr.Name == (xml.Name{Space: rdfNamespace, Local: "resource"}) {
					targets = append(targets, attr.Value)
				}
			}
		}
	}
}

func TestPhotoXMPPreservesUnmodeledCollectionItems(t *testing.T) {
	t.Parallel()
	// later wraps properties in a second description that follows the first.
	later := func(attrs, properties string) string {
		return `</rdf:Description><rdf:Description` + attrs + ` xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/" crs:Tint="1">` + properties
	}
	for _, tc := range []struct {
		name, property, body, retained string
		value                          func(store.PhotoAuthored) string
		written                        func(*testing.T, string)
		initial                        string
	}{
		{"caption", "description", `<rdf:Alt><!--translations--><rdf:li xml:lang="fr">Bonjour</rdf:li><rdf:li xml:lang="x-default">Hello</rdf:li><rdf:li xml:lang="de">Guten Tag</rdf:li></rdf:Alt>`, "Bonjour", func(v store.PhotoAuthored) string { return v.Caption }, func(t *testing.T, written string) {
			t.Helper()
			assert.Contains(t, written, "<!--translations-->")
			assert.Less(t, strings.Index(written, "Bonjour"), strings.Index(written, "Guten Tag"))
			assert.Less(t, strings.Index(written, "Changed"), strings.Index(written, "Guten Tag"))
		}, ""},
		{"copyright", "rights", `<rdf:Alt><rdf:li xml:lang="fr">Droits</rdf:li><rdf:li xml:lang="x-default">Rights</rdf:li></rdf:Alt>`, "Droits", func(v store.PhotoAuthored) string { return v.Copyright }, nil, ""},
		{"case varied default", "description", `<rdf:Alt><rdf:li xml:lang="fr">Bonjour</rdf:li><rdf:li xml:lang="X-DeFaUlT">Hello</rdf:li></rdf:Alt>`, "Bonjour", func(v store.PhotoAuthored) string { return v.Caption }, nil, ""},
		{"missing default", "description", `<rdf:Alt><rdf:li xml:lang="fr">Bonjour</rdf:li></rdf:Alt>`, "Bonjour", func(v store.PhotoAuthored) string { return v.Caption }, nil, ""},
		{"creator", "creator", `<rdf:Seq><rdf:li>Creator A</rdf:li><rdf:li>Creator B</rdf:li><rdf:li>Creator C</rdf:li></rdf:Seq>`, "Creator B", func(v store.PhotoAuthored) string { return v.Creator }, func(t *testing.T, written string) {
			t.Helper()
			assert.Contains(t, written, "<dc:creator><rdf:Seq>")
			assert.Less(t, strings.Index(written, "Creator B"), strings.Index(written, "Creator C"))
		}, ""},
		// Teststrip reads only the first description, so a changed collection moves there from a
		// later description, keeping its other alternatives and the description's own properties.
		{"caption held in a later description", "description", later("", `<dc:description><rdf:Alt><rdf:li xml:lang="fr">Bonjour</rdf:li><rdf:li xml:lang="x-default">Old</rdf:li></rdf:Alt></dc:description>`), "Bonjour", func(v store.PhotoAuthored) string { return v.Caption }, func(t *testing.T, written string) {
			t.Helper()
			assert.Contains(t, written, `crs:Tint="1"`)
			assert.Less(t, strings.Index(written, "<dc:description"), strings.Index(written, "</rdf:Description>"))
		}, ""},
		{"copyright held in a later description", "rights", later("", `<dc:rights><rdf:Alt><rdf:li xml:lang="fr">Droits</rdf:li><rdf:li xml:lang="x-default">Old rights</rdf:li></rdf:Alt></dc:rights>`), "Droits", func(v store.PhotoAuthored) string { return v.Copyright }, func(t *testing.T, written string) {
			t.Helper()
			assert.Less(t, strings.Index(written, "<dc:rights"), strings.Index(written, "</rdf:Description>"))
		}, ""},
		{"creator held in a later description", "creator", later("", `<dc:creator><rdf:Seq><rdf:li>Old author</rdf:li><rdf:li>Second author</rdf:li></rdf:Seq></dc:creator>`), "Second author", func(v store.PhotoAuthored) string { return v.Creator }, func(t *testing.T, written string) {
			t.Helper()
			assert.Less(t, strings.Index(written, "<dc:creator"), strings.Index(written, "</rdf:Description>"))
		}, ""},
		{"creator under a language the later description set", "creator", later(` xml:lang="fr"`, `<dc:creator><rdf:Seq><rdf:li>Old author</rdf:li><rdf:li>Second author</rdf:li></rdf:Seq></dc:creator>`), "Second author", func(v store.PhotoAuthored) string { return v.Creator }, func(t *testing.T, written string) {
			t.Helper()
			assert.Regexp(t, `<dc:creator xml:lang="fr">.*>Second author<`, written)
		}, ""},
		{"scalar creator", "creator", `Creator A`, "", func(v store.PhotoAuthored) string { return v.Creator }, func(t *testing.T, written string) { t.Helper(); assert.Contains(t, written, "<dc:creator><rdf:Seq>") }, ""},
		{"missing creator", "creator", ``, "", func(v store.PhotoAuthored) string { return v.Creator }, func(t *testing.T, written string) { t.Helper(); assert.Contains(t, written, "<dc:creator><rdf:Seq>") }, ""},
		{"creator beside a resource", "creator", `<rdf:Seq><rdf:li>First author</rdf:li><rdf:li rdf:resource="https://example.org/credit"/></rdf:Seq>`, "", func(v store.PhotoAuthored) string { return v.Creator }, nil, "First author"},
		{"caption beside a resource", "description", `<rdf:Alt><rdf:li xml:lang="fr" rdf:resource="https://example.org/credit"/><rdf:li xml:lang="x-default">First author</rdf:li></rdf:Alt>`, "https://example.org/credit", func(v store.PhotoAuthored) string { return v.Caption }, func(t *testing.T, written string) {
			t.Helper()
			require.Equal(t, []string{"https://example.org/credit"}, photoXMPResourceTargets(t, written))
		}, "First author"},
		{"copyright beside a resource", "rights", `<rdf:Alt><rdf:li xml:lang="x-default">First author</rdf:li><rdf:li xml:lang="fr" rdf:resource="https://example.org/credit"/></rdf:Alt>`, "https://example.org/credit", func(v store.PhotoAuthored) string { return v.Copyright }, func(t *testing.T, written string) {
			t.Helper()
			require.Equal(t, []string{"https://example.org/credit"}, photoXMPResourceTargets(t, written))
		}, "First author"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if tc.body != "" && !strings.HasPrefix(tc.body, "</rdf:Description>") && !strings.HasPrefix(tc.body, "<x:xmpmeta") {
				body = "<dc:" + tc.property + ">" + tc.body + "</dc:" + tc.property + ">"
			}
			original := []byte(xmpSidecar(` xmp:Rating="3"`, body).payload)
			unchangedBytes, unchangedPacket, err := rewritePhotoXMPSidecar(t.Context(), original, store.PhotoExportInput{Authored: store.PhotoAuthored{Rating: 4, Confirmed: store.PhotoConfirmedRating}}, 1, store.PhotoConfirmedRating)
			require.NoError(t, err)
			if tc.initial != "" {
				assert.Equal(t, tc.initial, tc.value(unchangedPacket.PhotoAuthored))
			}
			unchanged := string(unchangedBytes)
			if tc.retained != "" {
				assert.Contains(t, unchanged, tc.retained)
			}
			mask := map[string]store.PhotoAuthoredFields{"description": store.PhotoConfirmedCaption, "creator": store.PhotoConfirmedCreator, "rights": store.PhotoConfirmedCopyright}[tc.property]
			values := store.PhotoAuthored{Confirmed: mask}
			switch tc.property {
			case "description":
				values.Caption = "Changed"
			case "creator":
				values.Creator = "Changed"
			case "rights":
				values.Copyright = "Changed"
			}
			writtenBytes, parsed, err := rewritePhotoXMPSidecar(t.Context(), original, store.PhotoExportInput{Authored: values}, 1, mask)
			require.NoError(t, err)
			written := string(writtenBytes)
			assert.Equal(t, "Changed", tc.value(parsed.PhotoAuthored))
			if tc.retained != "" {
				assert.Contains(t, written, tc.retained)
			}
			defaultFirst := func(data string) {
				t.Helper()
				if tc.property == "creator" {
					return
				}
				start := strings.Index(data, "<dc:"+tc.property)
				require.GreaterOrEqual(t, start, 0, data)
				langs := regexp.MustCompile(`xml:lang="([^"]+)"`).FindStringSubmatch(data[start:])
				require.NotNil(t, langs, data)
				assert.Equal(t, "x-default", strings.ToLower(langs[1]), "an edit writes the default alternative first")
			}
			defaultFirst(written)
			if tc.written != nil {
				tc.written(t, written)
			}

			clearedBytes, parsed, err := rewritePhotoXMPSidecar(t.Context(), writtenBytes, store.PhotoExportInput{}, 1, mask)
			require.NoError(t, err)
			assert.Empty(t, tc.value(parsed.PhotoAuthored))
			defaultFirst(string(clearedBytes))
			if tc.property == "creator" {
				assert.NotContains(t, string(clearedBytes), "dc:creator")
			} else {
				assert.Contains(t, string(clearedBytes), `xml:lang="x-default"></rdf:li>`)
				assert.Contains(t, string(clearedBytes), tc.retained)
			}
			_, parsed, err = rewritePhotoXMPSidecar(t.Context(), clearedBytes, store.PhotoExportInput{Authored: store.PhotoAuthored{Rating: 5, Confirmed: store.PhotoConfirmedRating}}, 1, store.PhotoConfirmedRating)
			require.NoError(t, err)
			assert.Empty(t, tc.value(parsed.PhotoAuthored), "later writes must preserve a cleared selected value")
		})
	}
	// Adobe and Teststrip never write xml:base, so sync refuses a packet that sets one rather than resolve it.
	t.Run("xml:base refuses editing", func(t *testing.T) {
		for _, attrs := range []string{
			` xml:base="people/"`,
			` rdf:about="capture" xml:base="https://example.org/photos/"`,
		} {
			packet := xmpSidecar(` xmp:Rating="3"`, `</rdf:Description><rdf:Description`+attrs+` xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:creator><rdf:Seq><rdf:li>First author</rdf:li><rdf:li rdf:resource="credit"/></rdf:Seq></dc:creator>`)
			_, _, err := rewritePhotoXMPSidecar(t.Context(), []byte(packet.payload), store.PhotoExportInput{Authored: store.PhotoAuthored{Creator: "Changed", Confirmed: store.PhotoConfirmedCreator}}, 1, store.PhotoConfirmedCreator)
			require.ErrorContains(t, err, "xml:base", attrs)
		}
	})
	t.Run("subject held in a later description moves once", func(t *testing.T) {
		held := `</rdf:Description><rdf:Description xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:subject><rdf:Bag><rdf:li>Beach</rdf:li></rdf:Bag></dc:subject>`
		packet := []byte(xmpSidecar(` xmp:Rating="3"`, held).payload)
		for _, keyword := range []string{"Harbor", "Pier"} {
			var err error
			packet, _, err = rewritePhotoXMPSidecar(t.Context(), packet, store.PhotoExportInput{Keywords: []string{keyword}}, 1, store.PhotoXMPKeywordsBit)
			require.NoError(t, err)
			data := string(packet)
			assert.Equal(t, 1, strings.Count(data, "<dc:subject"), data)
			assert.Less(t, strings.Index(data, "<dc:subject"), strings.Index(data, "</rdf:Description>"))
			assert.Contains(t, data, ">"+keyword+"<")
			assert.NotContains(t, data, ">Beach<")
		}
	})
}
func TestPhotoXMPAtomicRefusalThenValidClear(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar("", `<dc:description>Harbor</dc:description>`))
	e.drain()
	before := e.raw().Authored()
	history, _, err := e.catalog.ClassifyPhotoXMP(t.Context(), e.raw().ID)
	require.NoError(t, err)
	e.observe(xmpSidecar(` xmp:Rating="4"`, `<dc:description rdf:parseType="Resource"/>`))
	e.state(store.PhotoXMPStateInvalid)
	assert.Equal(t, before, e.raw().Authored())
	current, _, err := e.catalog.ClassifyPhotoXMP(t.Context(), e.raw().ID)
	require.NoError(t, err)
	assert.Equal(t, history.History, current.History, "refusal preserves both histories")
	_, data, _ := e.packet()
	facts, err := readPhotoSidecarValues(t.Context(), []byte(data))
	require.NoError(t, err)
	assert.Equal(t, 4, facts.Rating)
	e.edit(store.PhotoAuthoredPatch{Creator: new("Local creator")})
	e.drain()
	e.state(store.PhotoXMPStateInvalid)
	e.observe(xmpSidecar("", `<dc:description rdf:parseType="Resource"><rdf:value></rdf:value></dc:description>`))
	e.state(store.PhotoXMPStateSynced)
	assert.Empty(t, e.raw().Caption)
}

func TestPhotoXMPExtensionClaimPreservesAttachment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ audited, qualified bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(strconv.FormatBool(tc.audited)+"/qualified="+strconv.FormatBool(tc.qualified), func(t *testing.T) {
			packet := xmpSidecar(` xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" photoshop:SidecarForExtension="JPG" xmp:Rating="4"`, "")
			if tc.qualified {
				packet.name = "capture.ARW.xmp"
			}
			e := newXMPEnv(t, photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}, packet)
			if tc.audited {
				plan, err := e.catalog.PreviewInitialAudit(t.Context(), e.catalog.RootID(), "cli", nil)
				require.NoError(t, err)
				_, err = e.catalog.EnableInitialAudit(t.Context(), plan)
				require.NoError(t, err)
			}
			before := e.sidecars()[0]
			e.drain()
			require.Equal(t, new(e.raw().ID), e.member(store.PhotoRoleSidecar).SidecarOfID)
			require.Equal(t, before.CurrentVersionID, e.sidecars()[0].CurrentVersionID)
			require.Zero(t, e.raw().Rating)
			require.Zero(t, e.member(store.PhotoRoleImage).Rating)
			require.Contains(t, e.state(store.PhotoXMPStateInvalid).Reason, "correct its attachment or filename")
		})
	}
}
func TestPhotoXMPIgnoredPropertiesAcrossWritesAndClear(t *testing.T) {
	t.Parallel()
	for _, known := range []bool{false, true} {
		t.Run(strconv.FormatBool(known), func(t *testing.T) {
			e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3" xmp:Label="Approved"`, `<dc:description rdf:resource="https://example.org/caption">Opaque text</dc:description>`))
			caption, label := "", ""
			if known {
				e.observe(xmpSidecar(` xmp:Rating="3" xmp:Label="blue"`, `<dc:description>Caption</dc:description>`))
				caption, label = "Caption", "blue"
				e.importFiles(xmpSidecar(` xmp:Rating="3" xmp:Label="Approved"`, `<dc:description rdf:resource="https://example.org/caption">Opaque text</dc:description>`))
			}
			e.drain()
			for _, rating := range []int{4, 5} {
				e.edit(store.PhotoAuthoredPatch{Rating: new(rating)})
				e.drain()
				_, data, _ := e.packet()
				require.Contains(t, data, "Opaque text")
				require.Contains(t, data, "Approved")
				require.Equal(t, caption, e.raw().Caption)
				require.Equal(t, label, e.raw().Label)
				require.Contains(t, e.state(store.PhotoXMPStateSynced).Reason, "caption")
			}
			e.observe(xmpSidecar(` xmp:Rating="3"`, ""))
			e.state(store.PhotoXMPStateSynced)
			require.Empty(t, e.raw().Caption)
			require.Empty(t, e.raw().Label)
		})
	}
	for _, tc := range []struct {
		name, attrs, body string
		patch             store.PhotoAuthoredPatch
	}{
		{"rating", ` xmp:Rating="9"`, "", store.PhotoAuthoredPatch{Rating: new(0)}},
		{"label", ` xmp:Label="Approved"`, "", store.PhotoAuthoredPatch{Label: new("")}},
		{"flag", ` ts:Pick="maybe"`, "", store.PhotoAuthoredPatch{Flag: new("")}},
		{"rotation", ` ts:Rotation="45"`, "", store.PhotoAuthoredPatch{Rotation: new(0)}},
		{"caption", "", `<dc:description rdf:nodeID="caption"/>`, store.PhotoAuthoredPatch{Caption: new("")}},
		{"creator", "", `<dc:creator rdf:resource="https://example.org/creator"/>`, store.PhotoAuthoredPatch{Creator: new("")}},
		{"copyright", "", `<dc:rights rdf:resource="https://example.org/rights"/>`, store.PhotoAuthoredPatch{Copyright: new("")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newXMPEnv(t, xmpSidecar(tc.attrs, tc.body))
			e.drain()
			e.edit(tc.patch)
			e.drain()
			_, _, packet := e.packet()
			require.Zero(t, packet.Ignored)
		})
	}
}

// A local replacement keeps the last readable packet as the field's ancestor, so removing
// a value after an unsupported one still clears the catalog.
func TestPhotoXMPLocalReplacementKeepsLastReadableAncestor(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
	e.drain()
	require.Equal(t, 3, e.raw().Rating)
	e.replaceSidecar(xmpSidecar(` xmp:Rating="9"`, "").payload)
	e.drain()
	require.Contains(t, e.state(store.PhotoXMPStateSynced).Reason, "rating")
	require.Equal(t, 3, e.raw().Rating)
	e.replaceSidecar(xmpSidecar("", "").payload)
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	require.Zero(t, e.raw().Rating)
	_, data, _ := e.packet()
	require.NotContains(t, data, "Rating=")
}

func rewritePhotoXMPSidecar(ctx context.Context, packet []byte, input store.PhotoExportInput, source int, mask store.PhotoAuthoredFields) ([]byte, photoSidecarPacket, error) {
	var parsed photoSidecarPacket
	if len(packet) > 0 {
		var err error
		parsed, err = readPhotoSidecar(ctx, packet, photoSidecarEditing)
		if err != nil || parsed.refusal != nil {
			return nil, parsed, errors.Join(err, parsed.refusal)
		}
	}
	return mergePhotoXMPSidecar(ctx, parsed, input, source, mask)
}

func TestPhotoXMPExplicitAttachmentCorrection(t *testing.T) {
	t.Parallel()
	packet := xmpSidecar(` xmp:Rating="4"`, "")
	packet.name = "capture.JPG.xmp"
	e := newXMPEnv(t, photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}, packet)
	sidecar, raw, jpeg := e.member(store.PhotoRoleSidecar), e.raw(), e.member(store.PhotoRoleImage)
	asset := e.asset()
	asset, err := e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, store.PhotoDetachOptions{})
	require.NoError(t, err)
	_, err = e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.NodeID, store.PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	e.drain()
	e.state(store.PhotoXMPStateInvalid)
	assert.Zero(t, e.raw().Rating)
	assert.Zero(t, e.member(store.PhotoRoleImage).Rating)
	asset = e.asset()
	asset, err = e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, store.PhotoDetachOptions{})
	require.NoError(t, err)
	_, err = e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.NodeID, store.PhotoRoleSidecar, &jpeg.ID)
	require.NoError(t, err)
	e.drain()
	assert.Zero(t, e.raw().Rating)
	assert.Equal(t, 4, e.member(store.PhotoRoleImage).Rating)
	e.stateOf(jpeg.ID, store.PhotoXMPStateSynced)
}

// A sidecar the worker creates for the first time must not stale the file and node
// revisions an open editor advanced from its edit receipt.
func TestPhotoXMPFirstSidecarKeepsPreparedEditorUsable(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t)
	prepared, err := e.catalog.PreparePhotoAuthored(t.Context(), store.PhotoSetSelection{AssetIDs: []string{e.assetID}})
	require.NoError(t, err)
	require.Empty(t, e.sidecars())
	var target store.PhotoAuthoredEditTarget
	for _, member := range prepared.Targets {
		if member.FileID == e.raw().ID {
			target.PhotoAuthoredTargetRevision = member.PhotoAuthoredTargetRevision
		}
	}
	first, err := e.catalog.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{target}, Patch: store.PhotoAuthoredPatch{Rating: new(4)}})
	require.NoError(t, err)
	require.Len(t, first.After, 1)
	target.NodeRevision += first.After[0].Revision - target.Revision
	target.Revision = first.After[0].Revision
	e.drain()
	require.Len(t, e.sidecars(), 1)
	require.Equal(t, target.Revision, e.raw().Revision)
	require.Equal(t, target.NodeRevision, e.node(e.raw().NodeID).Revision)
}

func (e *xmpEnv) commitCheckout(payload string) string {
	e.t.Helper()
	t := e.t
	c, _, _, err := e.catalog.CreatePhotoCheckout(t.Context(), store.PhotoCheckoutSelection{AssetIDs: []string{e.assetID}}, t.TempDir(), "", 2<<20)
	require.NoError(t, err)
	entries, err := e.catalog.PhotoCheckoutEntries(t.Context(), c.ID)
	require.NoError(t, err)
	var sidecar store.PhotoCheckoutEntry
	for _, entry := range entries {
		require.NoError(t, e.catalog.PublishPhotoCheckoutEntry(t.Context(), entry, func() (bool, error) { return true, nil }))
		if entry.SidecarOfID != nil {
			sidecar = entry
		}
	}
	require.NotEmpty(t, sidecar.FileID)
	require.NoError(t, e.catalog.FinishPhotoCheckout(t.Context(), c.ID, c.OperationID, store.StorageOperationCompleted, "{}", "", time.Now().Add(time.Hour)))
	c, op, err := e.catalog.PreparePhotoCheckoutOperation(t.Context(), store.PhotoCheckoutInspection{CheckoutID: c.ID}, store.StorageOperationKindPhotoCheckoutCommit)
	require.NoError(t, err)
	_, err = e.catalog.ClaimStorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	written, err := e.blobs.WriteDetailedContext(t.Context(), strings.NewReader(payload))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	require.NoError(t, e.catalog.CommitPhotoCheckoutEntry(t.Context(), c, sidecar, written.Hash, written.Size, "application/rdf+xml", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, Created: written.Created}))
	require.NoError(t, e.catalog.FinishPhotoCheckoutOperation(t.Context(), c, store.StorageOperationCompleted, "{}", ""))
	return c.ID
}

func TestPhotoXMPCheckoutEditsPublishedAncestor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		rating       int
		local, twice bool
		prune        bool
	}{
		{"single return", 3, false, false, false}, {"single change", 4, false, false, false}, {"checkout return", 3, false, true, false}, {"checkout change", 4, false, true, false}, {"local return", 3, true, true, false}, {"local change", 4, true, true, false},
		{"local change after pruning the published version", 4, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			packet := xmpSidecar(` xmp:Rating="3"`, `<dc:subject><rdf:Bag><rdf:li>Outside</rdf:li></rdf:Bag></dc:subject>`)
			packet.name = "capture.ARW.xmp"
			e := newXMPEnv(t, packet)
			e.drain()
			outside, err := e.catalog.TagByName(t.Context(), "Outside")
			require.NoError(t, err)
			published, err := e.catalog.CreateTag(t.Context(), "Published")
			require.NoError(t, err)
			e.edit(store.PhotoAuthoredPatch{Rating: new(5)}, store.PhotoKeywordEdit{TagID: outside.ID, Assign: false}, store.PhotoKeywordEdit{TagID: published.ID, Assign: true})
			e.drain()
			generated, data, _ := e.packet()
			changed := strings.Replace(data, `Rating="5"`, `Rating="`+strconv.Itoa(tc.rating)+`"`, 1)
			changed = strings.Replace(changed, ">Published<", ">Outside<", 1)
			require.NotEqual(t, data, changed)
			mutate := func(data string) { e.commitCheckout(data) }
			if tc.local {
				mutate = e.replaceSidecar
			}
			if tc.twice {
				mutate(strings.Replace(data, `Rating="5"`, `Rating="2"`, 1))
			}
			if tc.prune {
				e.drain()
				n := e.sidecars()[0]
				pruned, err := e.catalog.PruneContentVersions(t.Context(), n.ID, n.Revision, store.VersionPruneSelector{VersionIDs: []string{generated.CurrentVersionID}}, true)
				require.NoError(t, err)
				require.Equal(t, 1, pruned.DeletedVersions)
			}
			mutate(changed)
			e.drain()
			state := e.state(store.PhotoXMPStateSynced)
			require.Equal(t, tc.rating, e.raw().Rating)
			require.NotEmpty(t, state.ImportReceiptID)
			require.Equal(t, []string{"Outside"}, e.keywords())
			_, _, synced := e.packet()
			require.Equal(t, tc.rating, synced.Rating)
			require.Equal(t, []string{"Outside"}, synced.Keywords)
		})
	}
}

// Two folders hold a copy at rating 3. Both change before the worker runs.
func TestPhotoXMPEveryChangedCopyIsCompared(t *testing.T) {
	t.Parallel()
	raw := photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}
	three := xmpSidecar(` xmp:Rating="3"`, "")
	twoCopies := func(t *testing.T) *xmpEnv {
		t.Helper()
		e := newXMPEnv(t, three)
		e.drain()
		e.importFilesFrom("backup", raw, three)
		e.drain()
		return e
	}
	t.Run("renaming the original before the first sync keeps its copies' claims valid", func(t *testing.T) {
		e := newXMPEnv(t)
		claim := ` xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" photoshop:SidecarForExtension="ARW" xmp:Rating="3"`
		e.importFilesFrom("backup", raw, xmpSidecar(claim, ""))
		e.importFiles(raw, xmpSidecar(claim, `<dc:description>Harbor</dc:description>`))
		original := e.node(e.raw().NodeID)
		_, _, err := e.catalog.Move(t.Context(), original.ID, *original.ParentID, "renamed.dat", original.Revision)
		require.NoError(t, err)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 3, e.raw().Rating)
	})
	t.Run("copies of one photo named JPG and JPEG each keep their own claim", func(t *testing.T) {
		for _, tc := range []struct{ backupFirst, reverseStart bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			t.Run("backupFirst="+strconv.FormatBool(tc.backupFirst)+"/reverseStart="+strconv.FormatBool(tc.reverseStart), func(t *testing.T) {
				e := newXMPStore(t)
				claim := func(extension string) photoGroupFile {
					attrs, body := ` xmp:Rating="3"`, ""
					if extension == "JPEG" {
						attrs, body = "", `<dc:description>Harbor</dc:description>`
					}
					return xmpSidecar(` xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" photoshop:SidecarForExtension="`+extension+`"`+attrs, body)
				}
				jpg := photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}
				jpeg := photoGroupFile{"capture.JPEG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}
				folders := []string{"camera", "backup"}
				if tc.backupFirst != tc.reverseStart {
					folders[0], folders[1] = folders[1], folders[0]
				}
				runs := map[string]store.IngestRun{}
				for _, folder := range folders {
					run, err := e.catalog.BeginIngest(t.Context(), "photo-import", filepath.Join(e.root, folder))
					require.NoError(t, err)
					runs[folder] = run
				}
				if tc.backupFirst {
					e.assetID = e.importRun(runs["backup"], "backup", jpeg, claim("JPEG")).Asset.ID
					e.importRun(runs["camera"], "camera", jpg, claim("JPG"))
				} else {
					e.assetID = e.importRun(runs["camera"], "camera", jpg, claim("JPG")).Asset.ID
					e.importRun(runs["backup"], "backup", jpeg, claim("JPEG"))
				}
				e.extract()
				e.drain()
				image := e.member(store.PhotoRoleImage)
				e.stateOf(image.ID, store.PhotoXMPStateSynced)
				require.Equal(t, 3, e.member(store.PhotoRoleImage).Rating)
				require.Equal(t, "Harbor", e.member(store.PhotoRoleImage).Caption)
			})
		}
	})
	t.Run("a copy claiming another extension is refused before the first sync", func(t *testing.T) {
		e := newXMPEnv(t)
		e.importFilesFrom("backup", raw, xmpSidecar(` xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" photoshop:SidecarForExtension="JPG" xmp:Rating="5"`, ""))
		e.importFiles(raw, three)
		e.drain()
		e.state(store.PhotoXMPStateInvalid)
		require.Zero(t, e.raw().Rating, "a copy for another original supplies nothing")
	})
	t.Run("an unreadable head keeps the original pending", func(t *testing.T) {
		e := twoCopies(t)
		four := xmpSidecar(` xmp:Rating="4"`, "")
		e.importFiles(raw, four)
		head := e.sidecars()[0].BlobHash
		e.importFilesFrom("backup", raw, xmpSidecar(` xmp:Rating="3"`, `<dc:description>Harbor</dc:description>`))
		require.NoError(t, e.blobs.Remove(head))
		require.Error(t, SyncPhotoXMPBackground(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID))
		require.Contains(t, e.state(store.PhotoXMPStatePending).Reason, "unavailable")
		require.Equal(t, 3, e.raw().Rating)
		restored, err := e.blobs.WriteDetailedContext(t.Context(), strings.NewReader(four.payload))
		require.NoError(t, err)
		require.Equal(t, head, restored.Hash)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 4, e.raw().Rating)
		require.Equal(t, "Harbor", e.raw().Caption)
	})
	t.Run("both folders change before one drain", func(t *testing.T) {
		e := twoCopies(t)
		e.importFiles(raw, xmpSidecar(` xmp:Rating="4"`, ""))
		e.importFilesFrom("backup", raw, xmpSidecar(` xmp:Rating="3"`, `<dc:description>Harbor</dc:description>`))
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 4, e.raw().Rating)
		require.Equal(t, "Harbor", e.raw().Caption)
	})
	t.Run("copies that disagree ask for a choice", func(t *testing.T) {
		e := twoCopies(t)
		ing := &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}
		e.importFiles(raw, xmpSidecar(` xmp:Rating="4"`, ""))
		camera := e.sidecars()[0].CurrentVersionID
		e.importFilesFrom("backup", raw, xmpSidecar(` xmp:Rating="2"`, `<dc:description>Harbor</dc:description>`))
		e.drain()
		conflict := e.state(store.PhotoXMPStateConflict)
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "4", Sidecar: "2"}}, conflict.Rows, "the choice shows what the first copy left")
		require.Equal(t, 3, e.raw().Rating, "nothing reaches the catalog before the choice")
		imported, err := ResolvePhotoXMP(t.Context(), ing, e.raw().ID, conflict.ConflictID, "catalog")
		require.NoError(t, err)
		state := e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 4, e.raw().Rating)
		require.Equal(t, "Harbor", e.raw().Caption)
		require.Equal(t, camera, imported.Sidecar.VersionID, "the import names the copy that supplied the rating")
		require.Equal(t, state.ImportReceiptID, imported.ReceiptID)
		_, err = e.catalog.UndoPhotoAuthored(t.Context(), imported.ReceiptID)
		require.NoError(t, err)
		require.Equal(t, 3, e.raw().Rating)
		require.Empty(t, e.raw().Caption, "one undo reverses both copies' imports")
	})
	// After the first copy's choice, the second copy conflicts and the flow branches.
	firstChoiceMade := func(t *testing.T) (*xmpEnv, *ingest.Ingester, store.PhotoXMPState) {
		t.Helper()
		e := twoCopies(t)
		e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
		e.drain()
		e.importFiles(raw, xmpSidecar(` xmp:Rating="4"`, ""))
		e.importFilesFrom("backup", raw, xmpSidecar(` xmp:Rating="2"`, ""))
		e.drain()
		ing := &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}
		first := e.state(store.PhotoXMPStateConflict)
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, first.Rows)
		_, err := ResolvePhotoXMP(t.Context(), ing, e.raw().ID, first.ConflictID, "catalog")
		require.NoError(t, err)
		second := e.state(store.PhotoXMPStateConflict)
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "2"}}, second.Rows)
		return e, ing, second
	}
	t.Run("each copy is asked once", func(t *testing.T) {
		e, ing, second := firstChoiceMade(t)
		_, err := ResolvePhotoXMP(t.Context(), ing, e.raw().ID, second.ConflictID, "catalog")
		require.NoError(t, err)
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 5, e.raw().Rating)
	})
	t.Run("unavailable bytes keep the first copy's choice", func(t *testing.T) {
		e, ing, second := firstChoiceMade(t)
		head := e.sidecars()[0].BlobHash
		require.NoError(t, e.blobs.Remove(head))
		imported, err := ResolvePhotoXMP(t.Context(), ing, e.raw().ID, second.ConflictID, "catalog")
		require.Error(t, err)
		require.Nil(t, imported)
		targets, err := e.catalog.PhotoXMPTargetsAfter(t.Context(), "", 10)
		require.NoError(t, err)
		require.NotEmpty(t, targets)
		e.state(store.PhotoXMPStatePending)
		restored, err := e.blobs.WriteDetailedContext(t.Context(), strings.NewReader(xmpSidecar(` xmp:Rating="2"`, "").payload))
		require.NoError(t, err)
		require.Equal(t, head, restored.Hash)
		e.drain()
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "2"}}, e.state(store.PhotoXMPStateConflict).Rows)
	})
	t.Run("an input change between the choices asks the first copy again", func(t *testing.T) {
		e, _, _ := firstChoiceMade(t)
		e.edit(store.PhotoAuthoredPatch{Caption: new("Dock")})
		e.drain()
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, e.state(store.PhotoXMPStateConflict).Rows)
	})
	t.Run("a choice checks the readability of its own copy", func(t *testing.T) {
		e := twoCopies(t)
		e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
		e.drain()
		e.importFiles(raw, xmpSidecar(` xmp:Rating="4"`, ""))
		e.importFilesFrom("backup", raw, xmpSidecar(` xmp:Rating="-1"`, ""))
		e.drain()
		_, err := ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, e.state(store.PhotoXMPStateConflict).ConflictID, "catalog")
		require.NoError(t, err)
		require.Equal(t, 5, e.raw().Rating)
	})
	t.Run("a choice applies only to its own copy", func(t *testing.T) {
		e := twoCopies(t)
		e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
		e.drain()
		e.importFiles(raw, xmpSidecar(` xmp:Rating="4"`, ""))
		e.importFilesFrom("backup", raw, xmpSidecar(` xmp:Rating="3"`, `<dc:description>Harbor</dc:description>`))
		e.drain()
		conflict := e.state(store.PhotoXMPStateConflict)
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, conflict.Rows)
		_, err := ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, "sidecar")
		require.NoError(t, err)
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 4, e.raw().Rating, "the second copy's unchanged rating does not replay")
		require.Equal(t, "Harbor", e.raw().Caption)
	})
}

func TestPhotoXMPNewFolderComparesWithTheCatalog(t *testing.T) {
	t.Parallel()
	raw := photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}
	camera := xmpSidecar(` xmp:Rating="4"`, "")
	for choice, rating := range map[string]int{"catalog": 4, "sidecar": 3} {
		t.Run("a differing rating asks for a choice, then "+choice, func(t *testing.T) {
			e := newXMPEnv(t, camera)
			e.drain()
			backup := xmpSidecar(` xmp:Rating="3"`, "")
			e.importFilesFrom("backup", raw, backup)
			e.drain()
			require.Len(t, e.sidecars(), 1, "the new folder is another source of the one sidecar")
			conflict := e.state(store.PhotoXMPStateConflict)
			require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "4", Sidecar: "3"}}, conflict.Rows)
			require.Equal(t, 4, e.raw().Rating)
			imported, err := ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, choice)
			require.NoError(t, err)
			if choice == "sidecar" {
				require.NotNil(t, imported)
			} else {
				require.Nil(t, imported)
			}
			e.state(store.PhotoXMPStateSynced)
			require.Equal(t, rating, e.raw().Rating)
			e.importFiles(raw, camera)
			e.importFilesFrom("backup", raw, backup)
			e.drain()
			e.state(store.PhotoXMPStateSynced)
			require.Equal(t, rating, e.raw().Rating, "unchanged rescans of either folder keep the choice")
		})
	}
	t.Run("matching bytes add provenance", func(t *testing.T) {
		e := newXMPEnv(t, camera)
		e.drain()
		before := e.sidecars()[0].CurrentVersionID
		e.importFilesFrom("backup", raw, camera)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 4, e.raw().Rating)
		require.Equal(t, before, e.sidecars()[0].CurrentVersionID)
		sources, err := e.catalog.NodeProvenance(t.Context(), e.sidecars()[0].ID, 10, 0)
		require.NoError(t, err)
		require.Len(t, sources.Items, 2)
	})
}

func TestPhotoXMPUntouchedLightroomSidecarStaysOneVersion(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" xmp:Rating="0" tiff:Orientation="1"`, ""))
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	_, total, err := e.catalog.ContentVersions(t.Context(), e.sidecars()[0].ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
}

// The catalog rotation follows what a rescan finds in the sidecar's rotation
// and orientation properties: cleared, kept, or left out.
func TestPhotoXMPRotationFollowsSidecarEdits(t *testing.T) {
	t.Parallel()
	t.Run("removing either property from an outside sidecar", func(t *testing.T) {
		const tiff = ` xmlns:tiff="http://ns.adobe.com/tiff/1.0/"`
		for _, tc := range []struct {
			name, remaining, body string
			undo                  bool
		}{
			{"removing tiff:Orientation", ` ts:Rotation="90"`, "", false},
			{"removing ts:Rotation", ` tiff:Orientation="6"`, "", true},
			{"removing both", "", "", false},
			{"empty rotation attribute", ` ts:Rotation="" tiff:Orientation="6"`, "", false},
			{"empty orientation attribute", ` ts:Rotation="90" tiff:Orientation=""`, "", false},
			{"empty attributes", ` ts:Rotation=" " tiff:Orientation=" "`, "", false},
			{"empty rotation element", ` tiff:Orientation="6"`, `<ts:Rotation/>`, false},
			{"empty orientation element", ` ts:Rotation="90"`, `<tiff:Orientation/>`, false},
			{"empty elements", "", `<ts:Rotation> </ts:Rotation><tiff:Orientation> </tiff:Orientation>`, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newXMPEnv(t, xmpSidecar(tiff+` ts:Rotation="90" tiff:Orientation="6"`, ""))
				e.drain()
				require.Equal(t, 90, e.raw().Rotation)
				e.observe(xmpSidecar(tiff+tc.remaining, tc.body))
				state := e.state(store.PhotoXMPStateSynced)
				require.Zero(t, e.raw().Rotation)
				require.NotEmpty(t, state.ImportReceiptID)
				_, _, packet := e.packet()
				require.NotEqual(t, 6, packet.Orientation, "sync must not restore the removed rotation")
				if !tc.undo {
					return
				}
				require.Equal(t, new(0), packet.Rotation)
				require.Equal(t, 1, packet.Orientation)
				_, err := e.catalog.UndoPhotoAuthored(t.Context(), state.ImportReceiptID)
				require.NoError(t, err)
				require.Equal(t, 90, e.raw().Rotation)
			})
		}
	})
	t.Run("an outside sidecar without the rotation keeps the catalog's", func(t *testing.T) {
		e := newXMPEnv(t, xmpSidecar("", ""))
		e.drain()
		e.edit(store.PhotoAuthoredPatch{Rotation: new(90)})
		e.drain()
		sc, data, _ := e.packet()
		cleared := strings.Replace(data, ` ts:Rotation="90"`, "", 1)
		require.NotEqual(t, data, cleared)
		e.observe(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}, photoGroupFile{sc.Name, store.PhotoRoleSidecar, "application/rdf+xml", cleared})
		e.state(store.PhotoXMPStateSynced)
		_, _, packet := e.packet()
		require.Equal(t, 90, e.raw().Rotation)
		require.Equal(t, 6, packet.Orientation)
	})
	t.Run("a caption rescan keeps the rotation of a TIFF-only sidecar", func(t *testing.T) {
		const tiffOnly = ` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" tiff:Orientation="6"`
		e := newXMPEnv(t, xmpSidecar(tiffOnly, ""))
		e.drain()
		require.Equal(t, 90, e.raw().Rotation)
		e.observe(xmpSidecar(tiffOnly, `<dc:description>Harbor</dc:description>`))
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, "Harbor", e.raw().Caption)
		require.Equal(t, 90, e.raw().Rotation)
	})
}

func TestPhotoXMPLegacyRejectSurvivesUnrelatedWrites(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="-1"`, ""))
	e.drain()
	before, _, packet := e.packet()
	require.True(t, packet.LegacyReject)
	require.Equal(t, "reject", e.raw().Flag)
	tag, err := e.catalog.CreateTag(t.Context(), "Harbor")
	require.NoError(t, err)
	e.edit(store.PhotoAuthoredPatch{}, store.PhotoKeywordEdit{TagID: tag.ID, Assign: true})
	e.drain()
	sc, data, packet := e.packet()
	require.NotEqual(t, before.CurrentVersionID, sc.CurrentVersionID)
	require.True(t, packet.LegacyReject)
	for range 2 {
		e.observe(photoGroupFile{sc.Name, store.PhotoRoleSidecar, "application/rdf+xml", data})
		e.state(store.PhotoXMPStateSynced)
	}
	e.edit(store.PhotoAuthoredPatch{Flag: new("pick")})
	e.drain()
	_, _, packet = e.packet()
	require.False(t, packet.LegacyReject)
	require.Zero(t, packet.Rating)
	require.Equal(t, "pick", packet.Flag)
}

func TestPhotoXMPInitialImportCanUndo(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, ""))
	e.drain()
	state := e.state(store.PhotoXMPStateSynced)
	require.Equal(t, 4, e.raw().Rating)
	require.NotEmpty(t, state.ImportReceiptID)
	_, err := e.catalog.UndoPhotoAuthored(t.Context(), state.ImportReceiptID)
	require.NoError(t, err)
	e.drain()
	require.Zero(t, e.raw().Rating)
	require.Empty(t, e.state(store.PhotoXMPStateSynced).ImportReceiptID)
}

func (e *xmpEnv) replaceSidecar(payload string) {
	e.t.Helper()
	n := e.sidecars()[0]
	written, err := e.blobs.WriteDetailedContext(e.t.Context(), strings.NewReader(payload))
	require.NoError(e.t, err)
	_, _, err = e.catalog.ReplaceContent(e.t.Context(), n.ID, n.Revision, written.Hash, written.Size, "application/rdf+xml")
	require.NoError(e.t, err)
}

// A source orientation of 6 and a catalog rotation of 90 publish as orientation 3.
func TestPhotoXMPPublishedRotationThroughLocalEdits(t *testing.T) {
	t.Parallel()
	t.Run("orientation extracted before the edit", func(t *testing.T) {
		e := newXMPEnv(t, xmpSidecar("", ""))
		e.orient(e.node(e.raw().NodeID).BlobHash, strings.Repeat("b", 64), 6)
		e.drain()
		e.edit(store.PhotoAuthoredPatch{Rotation: new(90)})
		e.drain()
		_, data, packet := e.packet()
		require.Equal(t, 3, packet.Orientation)
		e.replaceSidecar(strings.Replace(data, "</rdf:Description>", "<dc:description>Caption</dc:description></rdf:Description>", 1))
		_, data, _ = e.packet()
		e.replaceSidecar(strings.Replace(data, `ts:Rotation="90"`, `ts:Rotation="180"`, 1))
		n := e.sidecars()[0]
		_, err := e.catalog.PruneContentVersions(t.Context(), n.ID, n.Revision, store.VersionPruneSelector{AllPrior: true}, true)
		require.NoError(t, err)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 180, e.raw().Rotation)
		_, _, packet = e.packet()
		require.Equal(t, 8, packet.Orientation)
		require.Equal(t, "Caption", e.raw().Caption)
	})
	t.Run("a generated sidecar is rewritten when the source is re-extracted with a new fingerprint", func(t *testing.T) {
		e := newXMPEnv(t)
		e.edit(store.PhotoAuthoredPatch{Rating: new(4), Rotation: new(90)})
		e.drain()
		before, _, _ := e.packet()
		e.orient(e.node(e.raw().NodeID).BlobHash, strings.Repeat("b", 64), 6)
		e.drain()
		after, _, packet := e.packet()
		assert.NotEqual(t, before.CurrentVersionID, after.CurrentVersionID)
		assert.Equal(t, 3, packet.Orientation)
	})
	t.Run("the written echo keeps the rotation when the source orientation changes", func(t *testing.T) {
		e := newXMPEnv(t, xmpSidecar("", ""))
		e.drain()
		e.orient(e.node(e.raw().NodeID).BlobHash, strings.Repeat("b", 64), 6)
		e.edit(store.PhotoAuthoredPatch{Rotation: new(90)})
		e.drain()
		sc, data, _ := e.packet()
		e.orient(e.node(e.raw().NodeID).BlobHash, strings.Repeat("c", 64), 1)
		e.observe(photoGroupFile{sc.Name, store.PhotoRoleSidecar, "application/rdf+xml", data})
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 90, e.raw().Rotation)
		_, _, packet := e.packet()
		require.Equal(t, 6, packet.Orientation)
	})
}

// A copy's rotation properties are read against that copy's own last packet.
func TestPhotoXMPCopyRotationComparesWithItsOwnPacket(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar("", ""))
	e.drain()
	e.edit(store.PhotoAuthoredPatch{Rotation: new(90)})
	e.drain()
	e.importFiles(xmpSidecar(` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" ts:Rotation="180" tiff:Orientation="3"`, ""))
	n := e.sidecars()[0]
	_, err := e.catalog.PruneContentVersions(t.Context(), n.ID, n.Revision, store.VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	e.drain()
	conflict := e.state(store.PhotoXMPStateConflict)
	require.Equal(t, []store.PhotoXMPRow{{Field: "Rotation", Catalog: "90", Sidecar: "180"}}, conflict.Rows)
	_, err = ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, "sidecar")
	require.NoError(t, err)
	e.state(store.PhotoXMPStateSynced)
	require.Equal(t, 180, e.raw().Rotation)
	_, _, packet := e.packet()
	require.Equal(t, new(180), packet.Rotation)
	require.Equal(t, 3, packet.Orientation)
}

// A copy with no packet of its own is read against the source orientation it was written for,
// not the one the settled bytes were written against.
func TestPhotoXMPNewCopyRotationUsesCurrentSourceOrientation(t *testing.T) {
	t.Parallel()
	raw := photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}
	e := newXMPEnv(t, xmpSidecar("", ""))
	hash := e.node(e.raw().NodeID).BlobHash
	e.orient(hash, strings.Repeat("b", 64), 6)
	e.edit(store.PhotoAuthoredPatch{Rotation: new(180)})
	e.drain()
	e.orient(hash, strings.Repeat("c", 64), 1)
	e.importFilesFrom("backup", raw, xmpSidecar(` xmlns:tiff="http://ns.adobe.com/tiff/1.0/" tiff:Orientation="6"`, ""))
	e.drain()
	require.Equal(t, []store.PhotoXMPRow{{Field: "Rotation", Catalog: "180", Sidecar: "90"}}, e.state(store.PhotoXMPStateConflict).Rows)
	require.Equal(t, 180, e.raw().Rotation)
}

func TestPhotoXMPSavedAttachmentSurvivesLaterClaims(t *testing.T) {
	t.Parallel()
	t.Run("rescan", func(t *testing.T) {
		e := newXMPEnv(t, photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}, xmpSidecar(` xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" photoshop:SidecarForExtension="ARW" xmp:Rating="3"`, ""))
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 3, e.raw().Rating)
		e.observe(xmpSidecar(` xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" photoshop:SidecarForExtension="JPG" xmp:Rating="4"`, ""))
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 4, e.raw().Rating)
		require.Equal(t, new(e.raw().ID), e.member(store.PhotoRoleSidecar).SidecarOfID)
	})
	for _, tc := range []struct{ name, claim string }{{"renamed.JPG", "JPG"}, {"renamed.bin", ".aRw"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
			e.drain()
			payload := xmpSidecar(` xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/" xmp:Rating="4" photoshop:SidecarForExtension="`+tc.claim+`"`, "").payload
			checkout := e.commitCheckout(payload)
			raw := e.raw()
			n := e.node(raw.NodeID)
			_, _, err := e.catalog.Move(t.Context(), n.ID, *n.ParentID, tc.name, n.Revision)
			require.NoError(t, err)
			e.drain()
			e.state(store.PhotoXMPStateSynced)
			_, err = e.catalog.RetirePhotoCheckout(t.Context(), checkout)
			require.NoError(t, err)
			e.edit(store.PhotoAuthoredPatch{Caption: new("Later")})
			e.drain()
			e.state(store.PhotoXMPStateSynced)
			require.Equal(t, 4, e.raw().Rating)
		})
	}
}

// A conflict belongs to the sidecar file that produced it.
func TestPhotoXMPConflictBelongsToItsSidecar(t *testing.T) {
	t.Parallel()
	conflicted := func(t *testing.T) (*xmpEnv, store.PhotoFile) {
		t.Helper()
		e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
		e.drain()
		e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
		e.replaceSidecar(xmpSidecar(` xmp:Rating="4"`, "").payload)
		e.drain()
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, e.state(store.PhotoXMPStateConflict).Rows)
		sidecar, asset := e.member(store.PhotoRoleSidecar), e.asset()
		_, err := e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, store.PhotoDetachOptions{})
		require.NoError(t, err)
		return e, sidecar
	}
	t.Run("a different sidecar does not inherit it", func(t *testing.T) {
		e, _ := conflicted(t)
		written, err := e.blobs.WriteDetailedContext(t.Context(), strings.NewReader(xmpSidecar("", `<dc:description>Harbor</dc:description>`).payload))
		require.NoError(t, err)
		other, err := e.catalog.CreateFile(t.Context(), e.catalog.RootID(), "other.xmp", written.Hash, written.Size, "application/rdf+xml")
		require.NoError(t, err)
		asset, raw := e.asset(), e.raw()
		_, err = e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, other.ID, store.PhotoRoleSidecar, &raw.ID)
		require.NoError(t, err)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 5, e.raw().Rating)
		require.Equal(t, "Harbor", e.raw().Caption)
	})
	t.Run("the same sidecar returning keeps it", func(t *testing.T) {
		e, sidecar := conflicted(t)
		asset, raw := e.asset(), e.raw()
		_, err := e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.NodeID, store.PhotoRoleSidecar, &raw.ID)
		require.NoError(t, err)
		e.drain()
		require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, e.state(store.PhotoXMPStateConflict).Rows)
	})
}

func TestPhotoXMPDetachedSidecarResumesSync(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	raw, sidecar := e.raw(), e.member(store.PhotoRoleSidecar)
	settled := e.node(sidecar.NodeID)
	asset := e.asset()
	asset, err := e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, store.PhotoDetachOptions{})
	require.NoError(t, err)
	packet := xmpSidecar(` xmp:Rating="4"`, "")
	written, err := e.blobs.WriteDetailedContext(t.Context(), strings.NewReader(packet.payload))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	replaced, _, err := e.catalog.ReplaceContent(t.Context(), settled.ID, settled.Revision, written.Hash, written.Size, "application/rdf+xml", store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, Created: written.Created})
	require.NoError(t, err)
	_, err = e.catalog.PruneContentVersions(t.Context(), replaced.ID, replaced.Revision, store.VersionPruneSelector{AllPrior: true}, true)
	require.NoError(t, err)
	_, err = e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, replaced.ID, store.PhotoRoleSidecar, &raw.ID)
	require.NoError(t, err)
	e.drain()
	e.state(store.PhotoXMPStateSynced)
	require.Equal(t, 4, e.raw().Rating)
}

func TestPhotoXMPOverlappingRunsPreserveObservationOrder(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t)
	folder := filepath.Join(e.root, "camera")
	older, err := e.catalog.BeginIngest(t.Context(), "photo-import", folder)
	require.NoError(t, err)
	newer, err := e.catalog.BeginIngest(t.Context(), "photo-import", folder)
	require.NoError(t, err)
	raw := photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}
	initial := xmpSidecar(` xmp:Rating="3"`, "")
	e.importRun(newer, "camera", raw, initial)
	e.importRun(older, "camera", raw, xmpSidecar(` xmp:Rating="5"`, ""))
	e.drain()
	require.Equal(t, 5, e.raw().Rating)
	// Seeing the current sidecar bytes again changes nothing, so the later outside change still conflicts.
	_, written, _ := e.packet()
	echo := initial
	echo.payload = written
	e.observe(echo)
	e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
	e.drain()
	e.observe(initial)
	assert.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "4", Sidecar: "3"}}, e.state(store.PhotoXMPStateConflict).Rows)
}

func TestPhotoXMPReassignedSidecarPreservesHistory(t *testing.T) {
	t.Parallel()
	for _, editWhileDetached := range []bool{false, true} {
		t.Run("editWhileDetached="+strconv.FormatBool(editWhileDetached), func(t *testing.T) {
			packet := xmpSidecar(` xmp:Rating="3"`, "")
			jpeg := photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}
			e := newXMPEnv(t, jpeg, packet)
			e.drain()
			outside := e.sidecars()[0]
			e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
			e.drain()
			e.state(store.PhotoXMPStateSynced)
			current := e.node(outside.ID)
			_, err := e.catalog.PruneContentVersions(t.Context(), current.ID, current.Revision, store.VersionPruneSelector{AllPrior: true}, true)
			require.NoError(t, err)

			first := e.raw()
			asset, second, sidecar := e.asset(), e.member(store.PhotoRoleImage), e.member(store.PhotoRoleSidecar)
			_, err = e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, store.PhotoDetachOptions{})
			require.NoError(t, err)
			if editWhileDetached {
				e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
				e.drain()
			}
			asset = e.asset()
			_, err = e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.NodeID, store.PhotoRoleSidecar, &second.ID)
			require.NoError(t, err)
			e.importFiles(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}, jpeg, packet)
			assert.Equal(t, current.CurrentVersionID, e.node(sidecar.NodeID).CurrentVersionID, "an unchanged rescan before settlement must preserve the published packet")
			e.drain()
			// The history stays with the first original, so the folder's rating is a first encounter for the second.
			conflict := e.stateOf(second.ID, store.PhotoXMPStateConflict)
			_, err = ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, second.ID, conflict.ConflictID, "sidecar")
			require.NoError(t, err)
			e.stateOf(second.ID, store.PhotoXMPStateSynced)
			require.Equal(t, 4, e.member(store.PhotoRoleImage).Rating)

			e.importFiles(photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}, jpeg, packet)
			e.drain()
			require.Equal(t, 4, e.member(store.PhotoRoleImage).Rating, "an unchanged source must not revert the reassigned original")
			if !editWhileDetached {
				asset, second = e.asset(), e.member(store.PhotoRoleImage)
				_, err = e.catalog.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: second.ID, Revision: second.Revision, NodeRevision: e.node(second.NodeID).Revision}}, Patch: store.PhotoAuthoredPatch{Caption: new("Second original")}})
				require.NoError(t, err)
				e.drain()
				e.stateOf(second.ID, store.PhotoXMPStateSynced)
				current = e.node(sidecar.NodeID)
				_, err = e.catalog.PruneContentVersions(t.Context(), current.ID, current.Revision, store.VersionPruneSelector{AllPrior: true}, true)
				require.NoError(t, err)

				asset = e.asset()
				asset, err = e.catalog.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.ID, store.PhotoDetachOptions{})
				require.NoError(t, err)
				_, err = e.catalog.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, sidecar.NodeID, store.PhotoRoleSidecar, &first.ID)
				require.NoError(t, err)
				e.drain()
				e.state(store.PhotoXMPStateSynced)
				require.Equal(t, 4, e.raw().Rating)
				require.Equal(t, "Second original", e.raw().Caption)
			}
		})
	}
}

// The importer attaches capture.xmp and capture.ARW.xmp to one RAW. Sync does
// not choose between them: it refuses until only one is left.
func TestPhotoXMPRefusesTwoSidecarsOnOneOriginal(t *testing.T) {
	t.Parallel()
	raw := photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "synthetic RAW"}
	qualified := func(rating string) photoGroupFile {
		f := xmpSidecar(` xmp:Rating="`+rating+`"`, "")
		f.name = "capture.ARW.xmp"
		return f
	}
	sidecarNamed := func(e *xmpEnv, name string) store.Node {
		for _, n := range e.sidecars() {
			if n.Name == name {
				return n
			}
		}
		t.Fatalf("no sidecar named %s", name)
		return store.Node{}
	}
	t.Run("imports nothing and clears when one is trashed", func(t *testing.T) {
		e := newXMPEnv(t)
		e.importFiles(raw, xmpSidecar(` xmp:Rating="3"`, ""), qualified("5"))
		require.Len(t, e.sidecars(), 2)
		e.drain()
		e.state(store.PhotoXMPStateInvalid)
		require.Len(t, e.sidecars(), 2)
		require.Zero(t, e.raw().Rating)
		extra := sidecarNamed(e, "capture.ARW.xmp")
		_, _, err := e.catalog.Trash(t.Context(), extra.ID, extra.Revision)
		require.NoError(t, err)
		e.drain()
		e.state(store.PhotoXMPStateSynced)
		require.Equal(t, 3, e.raw().Rating)
	})
	t.Run("counts sidecars per original", func(t *testing.T) {
		jpeg := photoGroupFile{"capture.JPG", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}
		second := xmpSidecar(` xmp:Rating="4"`, "")
		second.name = "capture.JPG.xmp"
		e := newXMPEnv(t, jpeg, xmpSidecar(` xmp:Rating="3"`, ""), second)
		e.drain()
		require.Len(t, e.sidecars(), 1)
		e.state(store.PhotoXMPStateSynced)
		e.stateOf(e.member(store.PhotoRoleImage).ID, store.PhotoXMPStateSynced)
		require.Equal(t, 3, e.raw().Rating)
		require.Equal(t, 4, e.member(store.PhotoRoleImage).Rating)
	})
}

// Outside apps can clear a Rating-only reject or explicitly retain it with Pick.
func TestPhotoXMPOutsideRatingRestoreKeepsPickReject(t *testing.T) {
	t.Parallel()
	// outside is an app saving the checked-out sidecar Docbank wrote, after the listed text changes.
	outside := func(edits string) func(*testing.T, *xmpEnv, string) {
		return func(t *testing.T, e *xmpEnv, data string) {
			t.Helper()
			edited := data
			for change := range strings.SplitSeq(edits, "; ") {
				from, to, _ := strings.Cut(change, "|")
				edited = strings.Replace(edited, from, to, 1)
			}
			require.NotEqual(t, data, edited)
			e.commitCheckout(edited)
			e.drain()
			e.state(store.PhotoXMPStateSynced)
		}
	}
	for _, tc := range []struct {
		name   string
		steps  func(*testing.T, *xmpEnv, string)
		flag   string
		rating int
	}{
		{"restoring a rating clears the reject", outside(`Rating="-1"|Rating="4"`), "", 4},
		{"restoring a rating after a catalog edit", func(t *testing.T, e *xmpEnv, _ string) {
			t.Helper()
			e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
			e.drain()
			_, data, _ := e.packet()
			require.Contains(t, data, `Rating="-1"`)
			outside(`Rating="-1"|Rating="4"`)(t, e, data)
		}, "", 4},
		{"restoring a rating after a catalog edit and unsupported rating", func(t *testing.T, e *xmpEnv, _ string) {
			t.Helper()
			e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
			e.drain()
			_, data, _ := e.packet()
			require.Contains(t, data, `Rating="-1"`)
			e.commitCheckout(strings.Replace(data, `Rating="-1"`, `Rating="unknown"`, 1))
			e.drain()
			outside(`Rating="-1"|Rating="4"`)(t, e, data)
		}, "", 4},
		{"a Teststrip save keeps the reject", outside(`Rating="-1"|Rating="0" ts:Pick="reject"`), "reject", 0},
		{"a caption edit keeps the rating", func(t *testing.T, e *xmpEnv, _ string) {
			t.Helper()
			e.edit(store.PhotoAuthoredPatch{Caption: new("Kept rating")})
			e.drain()
		}, "reject", 4},
		{"a rating edited while rejected survives an echo", func(t *testing.T, e *xmpEnv, _ string) {
			t.Helper()
			e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
			e.drain()
			_, data, _ := e.packet()
			e.observe(photoGroupFile{"capture.xmp", store.PhotoRoleSidecar, "application/rdf+xml", data})
		}, "reject", 2},
		{"clearing the flag writes the kept rating", func(t *testing.T, e *xmpEnv, _ string) {
			t.Helper()
			e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
			e.drain()
			e.edit(store.PhotoAuthoredPatch{Flag: new("")})
			e.drain()
			_, data, _ := e.packet()
			require.Contains(t, data, `Rating="2"`)
		}, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, ""))
			e.drain()
			e.edit(store.PhotoAuthoredPatch{Flag: new("reject")})
			e.drain()
			_, data, _ := e.packet()
			require.Contains(t, data, `Rating="-1"`)
			require.NotContains(t, data, `ts:Pick`)
			require.Empty(t, e.state(store.PhotoXMPStateSynced).Reason, "a reject Docbank wrote is no unsupported rating")
			tc.steps(t, e, data)
			require.Equal(t, tc.flag, e.raw().Flag)
			require.Equal(t, tc.rating, e.raw().Rating)
		})
	}
}

func TestPhotoXMPOutsideRatingRestoreAfterCatalogEdit(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"catalog", "sidecar"} {
		for _, unsupported := range []bool{false, true} {
			t.Run(choice+"/unsupported="+strconv.FormatBool(unsupported), func(t *testing.T) {
				e := newXMPEnv(t, xmpSidecar(` xmp:Rating="4"`, ""))
				e.drain()
				e.observe(xmpSidecar(` xmp:Rating="-1"`, ""))
				require.Equal(t, 4, e.raw().Rating)
				e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
				e.drain()
				if unsupported {
					e.observe(xmpSidecar(` xmp:Rating="unknown"`, ""))
				}
				before := e.raw()
				e.observe(xmpSidecar(` xmp:Rating="4" ts:Pick="reject"`, ""))
				conflict := e.state(store.PhotoXMPStateConflict)
				require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "2", Sidecar: "4"}}, conflict.Rows)
				imported, err := ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, choice)
				require.NoError(t, err)
				e.state(store.PhotoXMPStateSynced)
				require.Equal(t, "reject", e.raw().Flag)
				if choice == "catalog" {
					require.Equal(t, 2, e.raw().Rating)
				} else {
					require.Equal(t, 4, e.raw().Rating)
					require.NotNil(t, imported)
					_, err = e.catalog.UndoPhotoAuthored(t.Context(), imported.ReceiptID)
					require.NoError(t, err)
					require.Equal(t, before.Rating, e.raw().Rating)
					require.Equal(t, before.Flag, e.raw().Flag)
				}
			})
		}
	}
}

func TestPhotoXMPResolutionOverflowKeepsConflict(t *testing.T) {
	t.Parallel()
	// A refused choice leaves the conflict, its id, and the catalog as they were.
	t.Run("output exceeds the sidecar limit", func(t *testing.T) {
		e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
		e.drain()
		e.edit(store.PhotoAuthoredPatch{Rating: new(5), Caption: new(strings.Repeat("c", store.MaxPhotoAuthoredTextBytes))})
		e.drain()
		outside := xmpSidecar(` xmp:Rating="4"`, `<keep:Padding xmlns:keep="https://example.org/photo/">`+strings.Repeat("p", maxPhotoSidecarBytes-2048)+`</keep:Padding>`)
		require.Less(t, len(outside.payload), maxPhotoSidecarBytes)
		e.observe(outside)
		conflict := e.state(store.PhotoXMPStateConflict)
		before := e.sidecars()[0]
		for _, choice := range []string{"catalog", "sidecar"} {
			_, err := ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, choice)
			require.ErrorContains(t, err, "Correct the sidecar or catalog metadata and retry")
			require.ErrorIs(t, err, store.ErrInvalidPhotoAsset)
			require.Equal(t, conflict.ConflictID, e.state(store.PhotoXMPStateConflict).ConflictID)
		}
		require.Equal(t, before.CurrentVersionID, e.sidecars()[0].CurrentVersionID)
		require.Equal(t, 5, e.raw().Rating)
		e.observe(xmpSidecar(` xmp:Rating="4"`, ""))
		conflict = e.state(store.PhotoXMPStateConflict)
		_, err := ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, "catalog")
		require.NoError(t, err)
		e.state(store.PhotoXMPStateSynced)
	})
	t.Run("receipt exceeds its limit", func(t *testing.T) {
		bag := func(prefix string) string {
			var items strings.Builder
			for i := range 7000 {
				items.WriteString("<rdf:li>" + prefix + strconv.Itoa(i) + "</rdf:li>")
			}
			return `<dc:subject><rdf:Bag>` + items.String() + `</rdf:Bag></dc:subject>`
		}
		e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, bag("a")))
		e.drain()
		tags := func() int {
			_, total, err := e.catalog.NodeTags(t.Context(), e.raw().NodeID, 1, 0)
			require.NoError(t, err)
			return total
		}
		require.Equal(t, 7000, tags())
		e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
		e.drain()
		e.observe(xmpSidecar(` xmp:Rating="4"`, bag("b")))
		conflict := e.state(store.PhotoXMPStateConflict)
		_, err := ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, e.raw().ID, conflict.ConflictID, "sidecar")
		require.ErrorIs(t, err, store.ErrPhotoEditReceiptLimit)
		require.Equal(t, conflict.ConflictID, e.state(store.PhotoXMPStateConflict).ConflictID)
		require.Equal(t, 5, e.raw().Rating)
		require.Equal(t, 7000, tags())
	})
}
func TestPhotoXMPHiddenBackgroundAndLockedChoice(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
	rawID := e.raw().ID
	require.NoError(t, e.catalog.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	asset := e.asset()
	_, err := e.catalog.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
	require.NoError(t, err)
	e.drain()
	work, _, err := e.catalog.ClassifyPhotoXMP(t.Context(), rawID)
	require.NoError(t, err)
	require.Equal(t, 3, work.Original.Rating, "the worker imports a hidden photo without an interactive token")
	token, _, err := e.catalog.UnlockPhotoHidden(t.Context(), "synthetic-passcode")
	require.NoError(t, err)
	unlocked := store.WithPhotoHiddenToken(t.Context(), token)
	prepared, err := e.catalog.PreparePhotoAuthored(unlocked, store.PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: true})
	require.NoError(t, err)
	_, err = e.catalog.EditPhotoAuthoredBatch(unlocked, store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{PhotoAuthoredTargetRevision: prepared.Targets[0].PhotoAuthoredTargetRevision}}, Patch: store.PhotoAuthoredPatch{Rating: new(5)}})
	require.NoError(t, err)
	e.drain()
	e.importFiles(xmpSidecar(` xmp:Rating="4"`, ""))
	e.drain()
	prepared, err = e.catalog.PreparePhotoAuthored(unlocked, store.PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: true})
	require.NoError(t, err)
	conflict := prepared.Targets[0].XMP
	require.Equal(t, store.PhotoXMPStateConflict, conflict.State)
	_, err = ResolvePhotoXMP(t.Context(), &ingest.Ingester{Store: e.catalog, Blobs: e.blobs}, rawID, conflict.ConflictID, "catalog")
	require.ErrorIs(t, err, store.ErrHiddenLocked)
	after, err := e.catalog.PreparePhotoAuthored(unlocked, store.PhotoSetSelection{AssetIDs: []string{asset.ID}, Hidden: true})
	require.NoError(t, err)
	require.Equal(t, conflict, after.Targets[0].XMP, "a refused locked choice preserves the conflict")
}

func TestPhotoXMPRejectedConflictSurvivesTrashRestore(t *testing.T) {
	t.Parallel()
	e := newXMPEnv(t, xmpSidecar(` xmp:Rating="3"`, ""))
	e.drain()
	e.edit(store.PhotoAuthoredPatch{Flag: new("reject"), Rating: new(5)})
	e.drain()
	e.observe(xmpSidecar(` xmp:Rating="4" ts:Pick="reject"`, ""))
	before := e.state(store.PhotoXMPStateConflict)
	require.Equal(t, []store.PhotoXMPRow{{Field: "Rating", Catalog: "5", Sidecar: "4"}}, before.Rows)
	require.Equal(t, "reject", e.raw().Flag)
	asset := e.asset()
	_, err := e.catalog.TrashPhotoAsset(t.Context(), asset.ID, asset.Revision)
	require.NoError(t, err)
	for _, file := range asset.Files {
		require.NotNil(t, e.node(file.NodeID).TrashedAt)
	}
	_, _, err = e.catalog.Restore(t.Context(), e.raw().NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	e.drain()
	after := e.state(store.PhotoXMPStateConflict)
	require.Equal(t, before.Rows, after.Rows)
	require.Equal(t, "reject", e.raw().Flag)
}

// checkoutNames lists each checked-out photo's file names, by asset.
func (e *xmpEnv) checkoutNames(assetIDs ...string) map[string][]string {
	e.t.Helper()
	c, _, _, err := e.catalog.CreatePhotoCheckout(e.t.Context(), store.PhotoCheckoutSelection{AssetIDs: assetIDs}, e.t.TempDir(), "", 2<<20)
	require.NoError(e.t, err)
	entries, err := e.catalog.PhotoCheckoutEntries(e.t.Context(), c.ID)
	require.NoError(e.t, err)
	names := map[string][]string{}
	for _, entry := range entries {
		names[entry.AssetID] = append(names[entry.AssetID], path.Base(entry.Path))
	}
	for _, list := range names {
		slices.Sort(list)
	}
	return names
}

// A checkout names Docbank's sidecar after the original it delivers, so other apps pair them.
func TestPhotoXMPCheckoutSidecarNames(t *testing.T) {
	t.Parallel()
	rename := func(e *xmpEnv, name string) {
		raw := e.node(e.raw().NodeID)
		_, _, err := e.catalog.Move(t.Context(), raw.ID, *raw.ParentID, name, raw.Revision)
		require.NoError(t, err)
	}
	for _, name := range []string{"renamed.ARW", strings.Repeat("a", 248) + ".ARW"} {
		t.Run("a renamed RAW/"+name, func(t *testing.T) {
			e := newXMPEnv(t)
			rename(e, name)
			e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
			e.drain()
			e.state(store.PhotoXMPStateSynced)
			sidecar, _, _ := e.packet()
			require.Equal(t, name+".xmp", sidecar.Name)
			require.Equal(t, map[string][]string{e.assetID: {"capture.ARW", "capture.xmp"}}, e.checkoutNames(e.assetID))
		})
	}
	t.Run("two RAWs with one card name in one folder", func(t *testing.T) {
		e := newXMPEnv(t)
		e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
		first := e.assetID
		e.assetID = e.importFilesFrom("second card", photoGroupFile{xmpRaw, store.PhotoRoleRAW, "image/x-sony-arw", "another synthetic RAW"}).Asset.ID
		require.NotEqual(t, first, e.assetID)
		e.extract()
		e.edit(store.PhotoAuthoredPatch{Rating: new(2)})
		e.drain()
		want := []string{"capture.ARW", "capture.xmp"}
		require.Equal(t, map[string][]string{first: want, e.assetID: want}, e.checkoutNames(first, e.assetID))
	})
	t.Run("a RAW and JPEG whose stems differ only in case", func(t *testing.T) {
		e := newXMPStore(t)
		e.assetID = e.importFiles(photoGroupFile{"IMG_1.CR2", store.PhotoRoleRAW, "image/x-canon-cr2", "synthetic RAW"}, photoGroupFile{"img_1.jpg", store.PhotoRoleImage, "image/jpeg", "synthetic JPEG"}).Asset.ID
		e.extract()
		e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
		jpeg, asset := e.member(store.PhotoRoleImage), e.asset()
		_, err := e.catalog.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: jpeg.ID, Revision: jpeg.Revision, NodeRevision: e.node(jpeg.NodeID).Revision}}, Patch: store.PhotoAuthoredPatch{Rating: new(2)}})
		require.NoError(t, err)
		e.drain()
		vault := []string{}
		for _, f := range e.asset().Files {
			if f.SidecarOfID != nil {
				vault = append(vault, e.node(f.NodeID).Name)
			}
		}
		require.ElementsMatch(t, []string{"IMG_1.CR2.xmp", "img_1.jpg.xmp"}, vault)
		_, _, packet := e.packet()
		require.Equal(t, "cr2", packet.SidecarForExtension)
		require.Equal(t, map[string][]string{e.assetID: {"IMG_1.CR2", "IMG_1.xmp", "img_1.jpg", "img_1.jpg.xmp"}}, e.checkoutNames(e.assetID))
	})
	t.Run("a RAW renamed before its first sync", func(t *testing.T) {
		e := newXMPEnv(t)
		rename(e, "capture.RAW")
		e.edit(store.PhotoAuthoredPatch{Rating: new(4)})
		e.drain()
		_, _, packet := e.packet()
		require.Equal(t, "arw", packet.SidecarForExtension)
		require.Equal(t, map[string][]string{e.assetID: {"capture.ARW", "capture.xmp"}}, e.checkoutNames(e.assetID))
		e.edit(store.PhotoAuthoredPatch{Rating: new(5)})
		e.drain()
		e.state(store.PhotoXMPStateSynced)
	})
}
