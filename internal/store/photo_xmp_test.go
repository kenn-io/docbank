package store

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func publishXMPOrientation(t *testing.T, s *Store, nodeID int64, fingerprint string) {
	t.Helper()
	n, err := s.NodeByID(t.Context(), nodeID)
	require.NoError(t, err)
	_, err = s.PublishSourceMetadata(t.Context(), n.BlobHash, fingerprint, photoCanonical(t, photoMetadataField("image.exif.orientation", "image.exif", "Orientation", photoInteger(1))))
	require.NoError(t, err)
}

// xmpSidecarPhoto is a RAW+JPEG photo whose extracted RAW has a sidecar.
func xmpSidecarPhoto(t *testing.T) (*Store, PhotoAsset, PhotoFile, Node) {
	t.Helper()
	s, asset, raw, sidecar := authoredSidecar(t)
	publishXMPOrientation(t, s, raw.NodeID, fakeHash("ee"))
	return s, asset, raw, sidecar
}

// importXMP syncs an original as the worker would after reading a sidecar
// that holds values, and returns the undoable import.
func importXMP(t *testing.T, s *Store, fileID string, values PhotoAuthored, keywords ...string) PhotoAuthoredReceipt {
	t.Helper()
	f, err := photoFileByIDQuery(t.Context(), s.db, fileID)
	require.NoError(t, err)
	publishXMPOrientation(t, s, f.NodeID, fakeHash("ee"))
	work, run, err := s.ClassifyPhotoXMP(t.Context(), fileID)
	require.NoError(t, err)
	require.True(t, run)
	packet := PhotoXMPPacket{Values: values, Keywords: PhotoXMPKeywords(keywords)}
	state, err := s.PublishPhotoXMP(t.Context(), PhotoXMPPlan{Work: work, Sidecar: packet, Packet: packet, Values: values, Keywords: packet.Keywords, Import: true}, "", 0, BlobPhysical{})
	require.NoError(t, err)
	require.Equal(t, PhotoXMPStateSynced, state.State)
	require.NotNil(t, state.Receipt)
	return *state.Receipt
}

func xmpState(t *testing.T, s *Store, fileID string) PhotoXMPState {
	t.Helper()
	state, err := s.PhotoXMPState(t.Context(), fileID)
	require.NoError(t, err)
	return state
}

// settleXMP classifies a Pending original and reports its stored state, or
// Pending when it still needs the worker to merge sidecar bytes.
func settleXMP(t *testing.T, s *Store, fileID string) PhotoXMPState {
	t.Helper()
	_, _, err := s.ClassifyPhotoXMP(t.Context(), fileID)
	require.NoError(t, err)
	return xmpState(t, s, fileID)
}

func TestPhotoXMPInvalidation(t *testing.T) {
	t.Parallel()
	type fixture struct {
		t       *testing.T
		s       *Store
		asset   PhotoAsset
		raw     PhotoFile
		sidecar Node
		tag     Tag
	}
	node := func(t *testing.T, s *Store, id int64) Node {
		t.Helper()
		n, err := s.NodeByID(t.Context(), id)
		require.NoError(t, err)
		return n
	}
	for _, tc := range []struct {
		name    string
		mutate  func(fixture)
		pending bool
		settled string
	}{
		{"decision", func(f fixture) {
			t := f.t
			_, err := editPhotoAuthored(t.Context(), f.s, []PhotoAuthoredTarget{{FileID: f.raw.ID, Revision: 2, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
			require.NoError(t, err)
		}, true, PhotoXMPStatePending},
		{"own keyword", func(f fixture) {
			t := f.t
			tag, err := f.s.CreateTag(t.Context(), "Harbor")
			require.NoError(t, err)
			_, err = f.s.AssignTag(t.Context(), tag.ID, f.raw.NodeID, node(t, f.s, f.raw.NodeID).Revision)
			require.NoError(t, err)
		}, true, PhotoXMPStatePending},
		{"keyword renamed", func(f fixture) {
			t := f.t
			_, err := f.s.RenameTag(t.Context(), f.tag.ID, f.tag.Revision, "Journey")
			require.NoError(t, err)
		}, true, PhotoXMPStatePending},
		{"shared keyword assigned elsewhere", func(f fixture) {
			t := f.t
			other, err := f.s.CreateFile(t.Context(), f.s.RootID(), "other.jpg", fakeHash("e5"), 4, "image/jpeg")
			require.NoError(t, err)
			_, err = f.s.AssignTag(t.Context(), f.tag.ID, other.ID, other.Revision)
			require.NoError(t, err)
			tag, err := f.s.TagByID(t.Context(), f.tag.ID)
			require.NoError(t, err)
			require.Greater(t, tag.Revision, f.tag.Revision, "the shared tag revision advanced")
		}, false, PhotoXMPStateSynced},
		{"sidecar version", func(f fixture) {
			t := f.t
			_, _, err := f.s.ReplaceContent(t.Context(), f.sidecar.ID, node(t, f.s, f.sidecar.ID).Revision, fakeHash("d4"), 4, "application/rdf+xml")
			require.NoError(t, err)
		}, true, PhotoXMPStatePending},
		{"sidecar detached", func(f fixture) {
			t := f.t
			asset, err := f.s.PhotoAssetByID(t.Context(), f.asset.ID)
			require.NoError(t, err)
			for _, file := range asset.Files {
				if file.NodeID == f.sidecar.ID {
					_, err = f.s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, file.ID, PhotoDetachOptions{})
					require.NoError(t, err)
				}
			}
		}, true, PhotoXMPStateNone},
		{"sidecar trashed", func(f fixture) {
			t := f.t
			_, _, err := f.s.Trash(t.Context(), f.sidecar.ID, node(t, f.s, f.sidecar.ID).Revision)
			require.NoError(t, err)
		}, true, PhotoXMPStateNone},
		{"original renamed", func(f fixture) {
			t := f.t
			_, _, err := f.s.Move(t.Context(), f.raw.NodeID, f.s.RootID(), "renamed.cr2", node(t, f.s, f.raw.NodeID).Revision)
			require.NoError(t, err)
		}, true, PhotoXMPStateSynced},
		{"original version", func(f fixture) {
			t := f.t
			_, _, err := f.s.ReplaceContent(t.Context(), f.raw.NodeID, node(t, f.s, f.raw.NodeID).Revision, fakeHash("f6"), 4, "application/octet-stream")
			require.NoError(t, err)
		}, true, PhotoXMPStatePending},
		{"member attached", func(f fixture) {
			t := f.t
			extra, err := f.s.CreateFile(t.Context(), f.s.RootID(), "capture-2.jpg", fakeHash("e6"), 4, "image/jpeg")
			require.NoError(t, err)
			owned, err := f.s.PhotoAssetForNode(t.Context(), extra.ID)
			require.NoError(t, err)
			_, err = f.s.DetachPhotoFile(t.Context(), owned.ID, owned.Revision, owned.Files[0].ID, PhotoDetachOptions{})
			require.NoError(t, err)
			asset, err := f.s.PhotoAssetByID(t.Context(), f.asset.ID)
			require.NoError(t, err)
			_, err = f.s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, extra.ID, PhotoRoleImage, nil)
			require.NoError(t, err)
		}, true, PhotoXMPStateSynced},
		{"extraction", func(f fixture) {
			t := f.t
			publishXMPOrientation(t, f.s, f.raw.NodeID, fakeHash("ff"))
		}, true, PhotoXMPStateSynced},
		{"audit scope", func(f fixture) {
			t := f.t
			// Auditing starts in another folder, leaving this photo outside the scope.
			dir, err := f.s.Mkdir(t.Context(), f.s.RootID(), "audited")
			require.NoError(t, err)
			seedInitialAuditAuthority(t, f.s, dir.ID)
		}, true, PhotoXMPStateUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, asset, raw, sidecar := xmpSidecarPhoto(t)
			importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating}, "Travel")
			tag, err := s.TagByName(t.Context(), "Travel")
			require.NoError(t, err)
			raw, err = photoFileByIDQuery(t.Context(), s.db, raw.ID)
			require.NoError(t, err)
			tc.mutate(fixture{t, s, asset, raw, sidecar, tag})
			want := PhotoXMPStateSynced
			if tc.pending {
				want = PhotoXMPStatePending
			}
			assert.Equal(t, want, xmpState(t, s, raw.ID).State)
			assert.Equal(t, tc.settled, settleXMP(t, s, raw.ID).State)
		})
	}
}

func TestPhotoXMPClassificationWaitsAndRetries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	t.Run("extraction", func(t *testing.T) {
		t.Parallel()
		s, _, raw, _ := authoredSidecar(t)
		state := settleXMP(t, s, raw.ID)
		assert.Equal(t, PhotoXMPStatePending, state.State)
		assert.Contains(t, state.Reason, "extraction is pending")
		targets, err := s.PhotoXMPTargetsAfter(ctx, "", 10)
		require.NoError(t, err)
		assert.Empty(t, targets, "the worker skips an original waiting for extraction")
		publishXMPOrientation(t, s, raw.NodeID, fakeHash("ee"))
		targets, err = s.PhotoXMPTargetsAfter(ctx, "", 10)
		require.NoError(t, err)
		assert.Equal(t, []PhotoSidecarTarget{{FileID: raw.ID, NodeID: raw.NodeID}}, targets)
	})
	t.Run("capacity", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		asset := authoredPair(t, s)
		raw := fileByRole(asset.Files, PhotoRoleRAW)
		publishXMPOrientation(t, s, raw.NodeID, fakeHash("ee"))
		for i := len(asset.Files); i < PhotoMaxFiles; i++ {
			n, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("member-%03d.dat", i), fakeHash(fmt.Sprintf("%02x", i%256)), 4, "application/octet-stream")
			require.NoError(t, err)
			asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, n.ID, PhotoRoleSidecar, &raw.ID)
			require.NoError(t, err)
			n, err = s.NodeByID(ctx, n.ID)
			require.NoError(t, err)
			_, _, err = s.Trash(ctx, n.ID, n.Revision)
			require.NoError(t, err)
		}
		_, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(3)}}})
		require.NoError(t, err)
		state := settleXMP(t, s, raw.ID)
		assert.Equal(t, PhotoXMPStateInvalid, state.State)
		assert.Contains(t, state.Reason, "256 members")
		asset, err = s.PhotoAssetByID(ctx, asset.ID)
		require.NoError(t, err)
		_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, fileByRole(asset.Files, PhotoRoleImage).ID, PhotoDetachOptions{})
		require.NoError(t, err)
		_, run, err := s.ClassifyPhotoXMP(ctx, raw.ID)
		require.NoError(t, err)
		assert.True(t, run, "leaving the full photo lets sync create its sidecar")
	})
	t.Run("filename occupancy and duplicates", func(t *testing.T) {
		t.Parallel()
		s := newTestStore(t)
		asset := authoredPair(t, s)
		raw := fileByRole(asset.Files, PhotoRoleRAW)
		publishXMPOrientation(t, s, raw.NodeID, fakeHash("ee"))
		blocker, err := s.CreateFile(ctx, s.RootID(), "capture.cr2.xmp", fakeHash("c3"), 4, "application/rdf+xml")
		require.NoError(t, err)
		_, err = editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: 1, Patch: PhotoAuthoredPatch{Rating: new(3)}}})
		require.NoError(t, err)
		assert.Contains(t, settleXMP(t, s, raw.ID).Reason, "already occupied")
		_, _, err = s.Move(ctx, blocker.ID, s.RootID(), "elsewhere.xmp", blocker.Revision)
		require.NoError(t, err)
		_, run, err := s.ClassifyPhotoXMP(ctx, raw.ID)
		require.NoError(t, err)
		assert.True(t, run)
		for _, name := range []string{"capture.xmp", "capture-copy.xmp"} {
			n, err := s.CreateFile(ctx, s.RootID(), name, fakeHash("d4"), 4, "application/rdf+xml")
			require.NoError(t, err)
			current, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			_, err = s.AttachPhotoFile(ctx, asset.ID, current.Revision, n.ID, PhotoRoleSidecar, &raw.ID)
			require.NoError(t, err)
		}
		state := settleXMP(t, s, raw.ID)
		assert.Equal(t, PhotoXMPStateInvalid, state.State)
		assert.Contains(t, state.Reason, "capture-copy.xmp, capture.xmp")
	})
}

func TestPhotoXMPRestoreRebuildsStatus(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	restore := func(t *testing.T, s *Store) *Store {
		t.Helper()
		var backup bytes.Buffer
		require.NoError(t, s.ExportMetadata(ctx, &backup))
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
		require.NoError(t, restored.ValidateMetadata(ctx))
		return restored
	}
	s, asset, raw, sidecar := xmpSidecarPhoto(t)
	receipt := importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	_, err := s.db.Exec(`UPDATE photo_files SET source_sha256=? WHERE file_id=?`, sidecar.BlobHash, receipt.Sidecar.FileID)
	require.NoError(t, err)

	restored := restore(t, s)
	assert.Equal(t, PhotoXMPStatePending, xmpState(t, restored, raw.ID).State, "restored status waits for the worker")
	state := settleXMP(t, restored, raw.ID)
	assert.Equal(t, PhotoXMPStateSynced, state.State, "the restored receipt is still the baseline")
	require.NotNil(t, state.Receipt)
	assert.Equal(t, receipt.ReceiptID, state.Receipt.ReceiptID)
	var source string
	require.NoError(t, restored.db.QueryRow(`SELECT source_sha256 FROM photo_files WHERE file_id=?`, receipt.Sidecar.FileID).Scan(&source))
	assert.Equal(t, sidecar.BlobHash, source)

	// A removed sidecar stays suppressed across restore until the catalog changes.
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, receipt.Sidecar.FileID, PhotoDetachOptions{})
	require.NoError(t, err)
	assert.Equal(t, PhotoXMPStateNone, settleXMP(t, s, raw.ID).State)
	restored = restore(t, s)
	assert.Equal(t, PhotoXMPStateNone, settleXMP(t, restored, raw.ID).State)
	_, err = editPhotoAuthored(ctx, restored, []PhotoAuthoredTarget{{FileID: raw.ID, Revision: 2, Patch: PhotoAuthoredPatch{Rating: new(5)}}})
	require.NoError(t, err)
	_, run, err := restored.ClassifyPhotoXMP(ctx, raw.ID)
	require.NoError(t, err)
	assert.True(t, run)

	// A duplicated receipt is not one original's single baseline.
	var backup bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &backup))
	duplicated := backup.Bytes()
	for line := range bytes.Lines(backup.Bytes()) {
		if bytes.Contains(line, []byte(`"operation":"xmp_sync"`)) {
			duplicated = append(duplicated, bytes.Replace(line, []byte(state.Receipt.ReceiptID), []byte("40000000-0000-4000-8000-0000000000aa"), 1)...)
		}
	}
	err = newTestStore(t).ImportMetadata(ctx, bytes.NewReader(duplicated))
	require.Error(t, err)
}

func TestPhotoXMPReceiptDecoderIsStrict(t *testing.T) {
	t.Parallel()
	s, _, raw, _ := xmpSidecarPhoto(t)
	importXMP(t, s, raw.ID, PhotoAuthored{Rating: 4, Confirmed: PhotoConfirmedRating})
	var data []byte
	require.NoError(t, s.db.QueryRow(`SELECT after_json FROM photo_change_receipts WHERE operation='xmp_sync'`).Scan(&data))
	_, err := decodePhotoXMPReceipt(data)
	require.NoError(t, err)
	for _, corrupt := range [][]byte{
		bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"outcome":"written"`), 1),
		bytes.Replace(data, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(data, []byte(`"rating":4`), []byte(`"rating":6`), 1),
	} {
		_, err := decodePhotoXMPReceipt(corrupt)
		require.ErrorIs(t, err, ErrInvalidPhotoAsset, string(corrupt))
	}
	_, err = s.db.Exec(`UPDATE photo_change_receipts SET after_json=json_set(after_json,'$.version',2) WHERE operation='xmp_sync'`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE photo_files SET xmp_state='pending' WHERE file_id=?`, raw.ID)
	require.NoError(t, err)
	state := settleXMP(t, s, raw.ID)
	assert.Equal(t, PhotoXMPStateUnavailable, state.State)
	assert.Contains(t, state.Reason, "could not be read")
}

func TestPhotoXMPQueryFilters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, asset, raw, _ := xmpSidecarPhoto(t)
	match := func(text string) []string {
		t.Helper()
		page, err := s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: text, Sort: query.Sort{Field: "name", Direction: "asc"}}}, nil)
		require.NoError(t, err)
		ids := []string{}
		for _, item := range page.Items {
			ids = append(ids, item.AssetID)
		}
		return ids
	}
	assert.Equal(t, []string{asset.ID}, match("xmp:pending"))
	assert.Empty(t, match("xmp:conflict"))
	work, run, err := s.ClassifyPhotoXMP(ctx, raw.ID)
	require.NoError(t, err)
	require.True(t, run)
	state, err := s.PublishPhotoXMP(ctx, PhotoXMPPlan{Work: work, Rows: []PhotoXMPRow{{Field: "Rating", Catalog: "", Sidecar: "4"}}}, "", 0, BlobPhysical{})
	require.NoError(t, err)
	assert.Equal(t, PhotoXMPStateConflict, state.State)
	assert.Equal(t, []string{asset.ID}, match("xmp:conflict"))
	assert.Empty(t, match("xmp:pending"))
	assert.Equal(t, []string{asset.ID}, match("NOT xmp:pending"))
	_, err = s.ListPhotoAssets(ctx, PhotoBrowseRequest{Query: query.Query{V: 1, Syntax: "advanced", Mode: "lexical", Text: "xmp:synced"}}, nil)
	require.Error(t, err)
}
