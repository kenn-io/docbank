package store

import (
	"database/sql"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrowserPhotoGroupKey(t *testing.T) {
	t.Parallel()
	members := []BrowserPhotoGroupMember{
		{Path: "Trip/café.ARW", Role: PhotoRoleRAW},
		{Path: "Trip/café.xmp", Role: PhotoRoleSidecar},
	}
	key := BrowserPhotoGroupKey(members)
	assert.Equal(t, key, BrowserPhotoGroupKey([]BrowserPhotoGroupMember{
		{Path: "Trip/cafe\u0301.ARW", Role: PhotoRoleRAW},
		{Path: "Trip/cafe\u0301.xmp", Role: PhotoRoleSidecar},
	}))
	assert.NotEqual(t, key, BrowserPhotoGroupKey(members[:1]))
	assert.NotEqual(t, key, BrowserPhotoGroupKey([]BrowserPhotoGroupMember{
		{Path: "Trip/café.ARW", Role: PhotoRoleImage},
		{Path: "Trip/café.xmp", Role: PhotoRoleSidecar},
	}))
	assert.NotEqual(t, BrowserPhotoGroupKey([]BrowserPhotoGroupMember{{Path: "ab", Role: "c"}}),
		BrowserPhotoGroupKey([]BrowserPhotoGroupMember{{Path: "a", Role: "bc"}}))
}

func checkBrowserPhotoGroupReceiptReplay(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	members := []BrowserPhotoGroupMember{{Path: "Trip/capture.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/Trip/capture.xmp", Size: 4}}
	key := BrowserPhotoGroupKey(members)
	id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
		SourceID: uuid.New(), PickerKind: "folder", Label: "Trip", Destination: "/photos", Directory: directory,
		Groups: []BrowserPhotoImportGroup{{Key: key, Members: members}},
	})
	require.NoError(t, err)
	digest := fakeHash("a1")
	require.NoError(t, s.PinBrowserPhotoGroupHashes(ctx, id, key, []string{digest}))
	require.Error(t, s.PinBrowserPhotoGroupHashes(ctx, id, key, []string{fakeHash("b2")}))
	run, err := s.BeginIngest(ctx, "photo-import", "browser: Trip")
	require.NoError(t, err)
	group := PhotoImportGroup{
		BrowserGroup: &BrowserPhotoGroupRef{ImportID: id, GroupKey: key},
		Members: []PhotoImportMember{{Name: "capture.xmp", Role: PhotoRoleSidecar, BlobHash: digest,
			Size: 4, OriginalPath: members[0].SourcePath}},
		CreateOnly: true,
	}
	first, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	require.True(t, first.Added)
	require.Len(t, first.Nodes, 1)
	replay, err := s.IngestPhotoGroup(ctx, run, group)
	require.NoError(t, err)
	assert.True(t, replay.Added)
	require.Len(t, replay.Nodes, 1)
	assert.Equal(t, first.Nodes[0].ID, replay.Nodes[0].ID)
}

func checkBrowserPhotoReceiptOutcomes(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	commit := func(declarations []BrowserPhotoGroupMember, inputs []PhotoImportMember) PhotoImportResult {
		t.Helper()
		directory, err := s.PrepareIngestDirectory(ctx, "/photos")
		require.NoError(t, err)
		key := BrowserPhotoGroupKey(declarations)
		id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
			SourceID: uuid.New(), PickerKind: "files", Label: "Choose files", Destination: "/photos", Directory: directory,
			Groups: []BrowserPhotoImportGroup{{Key: key, Members: declarations}}, Skipped: []BrowserPhotoSkippedFile{{Path: "notes.txt", Reason: "Unsupported type"}},
		})
		require.NoError(t, err)
		run, err := s.BrowserPhotoImportRun(ctx, id)
		require.NoError(t, err)
		require.Equal(t, id.String(), run.ID())
		saved, err := s.InspectBrowserPhotoImport(ctx, id)
		require.NoError(t, err)
		require.Equal(t, []BrowserPhotoSkippedFile{{Path: "notes.txt", Reason: "Unsupported type"}}, saved.Skipped)
		hashes := make([]string, len(inputs))
		for i, input := range inputs {
			hashes[i] = input.BlobHash
		}
		require.NoError(t, s.PinBrowserPhotoGroupHashes(ctx, id, key, hashes))
		group := PhotoImportGroup{BrowserGroup: &BrowserPhotoGroupRef{ImportID: id, GroupKey: key}, Members: inputs, CreateOnly: true}
		result, err := s.IngestPhotoGroup(ctx, run, group)
		require.NoError(t, err)
		replay, err := s.IngestPhotoGroup(ctx, run, group)
		require.NoError(t, err)
		assert.Equal(t, result.Added, replay.Added)
		assert.Equal(t, result.Skipped, replay.Skipped)
		assert.Equal(t, result.Ambiguity, replay.Ambiguity)
		return result
	}
	image := PhotoImportMember{Name: "capture.JPG", Role: PhotoRoleImage, BlobHash: fakeHash("a1"), Size: 4, OriginalPath: "browser:first/capture.JPG"}
	first := BrowserPhotoGroupMember{Path: "capture.JPG", Role: PhotoRoleImage, SourcePath: image.OriginalPath, Size: 4}
	require.True(t, commit([]BrowserPhotoGroupMember{first}, []PhotoImportMember{image}).Added)
	image.OriginalPath = "browser:second/capture.JPG"
	first.SourcePath = image.OriginalPath
	require.True(t, commit([]BrowserPhotoGroupMember{first}, []PhotoImportMember{image}).Skipped)
	rawA := PhotoImportMember{Name: "ambiguous.ARW", Role: PhotoRoleRAW, BlobHash: fakeHash("b1"), Size: 4, OriginalPath: "browser:third/ambiguous.ARW"}
	rawB := PhotoImportMember{Name: "ambiguous.DNG", Role: PhotoRoleRAW, BlobHash: fakeHash("b2"), Size: 4, OriginalPath: "browser:third/ambiguous.DNG"}
	ambiguous := commit([]BrowserPhotoGroupMember{
		{Path: rawA.Name, Role: rawA.Role, SourcePath: rawA.OriginalPath, Size: rawA.Size},
		{Path: rawB.Name, Role: rawB.Role, SourcePath: rawB.OriginalPath, Size: rawB.Size},
	}, []PhotoImportMember{rawA, rawB})
	require.NotNil(t, ambiguous.Ambiguity)
	assert.Equal(t, PhotoImportMultipleRAW, ambiguous.Ambiguity.Reason)
}

func TestBrowserPhotoReceiptFailureRollsBackGroup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	members := []BrowserPhotoGroupMember{{Path: "capture.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/capture.xmp", Size: 4}}
	key := BrowserPhotoGroupKey(members)
	id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
		SourceID: uuid.New(), PickerKind: "files", Label: "Choose files", Destination: "/photos", Directory: directory,
		Groups: []BrowserPhotoImportGroup{{Key: key, Members: members}},
	})
	require.NoError(t, err)
	digest := fakeHash("a1")
	require.NoError(t, s.PinBrowserPhotoGroupHashes(ctx, id, key, []string{digest}))
	_, err = s.db.ExecContext(ctx, `CREATE TRIGGER fail_browser_receipt BEFORE UPDATE OF receipt_json ON browser_photo_import_groups BEGIN SELECT RAISE(ABORT, 'receipt failed'); END`)
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "photo-import", "browser: Choose files")
	require.NoError(t, err)
	_, err = s.IngestPhotoGroup(ctx, run, PhotoImportGroup{
		BrowserGroup: &BrowserPhotoGroupRef{ImportID: id, GroupKey: key},
		Members:      []PhotoImportMember{{Name: "capture.xmp", Role: PhotoRoleSidecar, BlobHash: digest, Size: 4, OriginalPath: members[0].SourcePath}},
		CreateOnly:   true,
	})
	require.Error(t, err)
	_, err = s.NodeByPath(ctx, "/photos")
	require.ErrorIs(t, err, ErrNotFound)
	node, err := s.Mkdir(ctx, s.RootID(), "after")
	require.NoError(t, err)
	assert.Equal(t, int64(2), node.ID)
}

func TestBrowserPhotoRecoveryStateCopy(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	target := newTestStore(t)
	ctx := t.Context()
	directory, err := source.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	first := []BrowserPhotoGroupMember{{Path: "first.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/first.xmp", Size: 4}}
	second := []BrowserPhotoGroupMember{{Path: "second.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/second.xmp", Size: 4}}
	firstKey, secondKey := BrowserPhotoGroupKey(first), BrowserPhotoGroupKey(second)
	id, err := source.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
		SourceID: uuid.New(), PickerKind: "files", Label: "Choose files", Destination: "/photos", Directory: directory,
		Groups:  []BrowserPhotoImportGroup{{Key: firstKey, Members: first}, {Key: secondKey, Members: second}},
		Skipped: []BrowserPhotoSkippedFile{{Path: "notes.txt", Reason: "Unsupported type"}},
	})
	require.NoError(t, err)
	digest := fakeHash("a1")
	require.NoError(t, source.PinBrowserPhotoGroupHashes(ctx, id, firstKey, []string{digest}))
	require.NoError(t, source.PinBrowserPhotoGroupHashes(ctx, id, secondKey, []string{digest}))
	run, err := source.BeginIngest(ctx, "photo-import", "browser: Choose files")
	require.NoError(t, err)
	_, err = source.IngestPhotoGroup(ctx, run, PhotoImportGroup{
		BrowserGroup: &BrowserPhotoGroupRef{ImportID: id, GroupKey: firstKey},
		Members:      []PhotoImportMember{{Name: "first.xmp", Role: PhotoRoleSidecar, BlobHash: digest, Size: 4, OriginalPath: first[0].SourcePath}},
		CreateOnly:   true,
	})
	require.NoError(t, err)
	require.NoError(t, restoreBrowserPhotoImportState(ctx, source.db, target))
	var sourceSkipped, targetSkipped string
	require.NoError(t, source.db.QueryRowContext(ctx, `SELECT skipped_json FROM browser_photo_imports WHERE import_id=$1`, id.String()).Scan(&sourceSkipped))
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT skipped_json FROM browser_photo_imports WHERE import_id=$1`, id.String()).Scan(&targetSkipped))
	assert.Equal(t, sourceSkipped, targetSkipped)
	for _, key := range []string{firstKey, secondKey} {
		var sourceHashes, targetHashes string
		var sourceReceipt, targetReceipt sql.NullString
		require.NoError(t, source.db.QueryRowContext(ctx, `SELECT hashes_json,receipt_json FROM browser_photo_import_groups WHERE import_id=$1 AND group_key=$2`, id.String(), key).Scan(&sourceHashes, &sourceReceipt))
		require.NoError(t, target.db.QueryRowContext(ctx, `SELECT hashes_json,receipt_json FROM browser_photo_import_groups WHERE import_id=$1 AND group_key=$2`, id.String(), key).Scan(&targetHashes, &targetReceipt))
		assert.Equal(t, sourceHashes, targetHashes)
		assert.Equal(t, sourceReceipt, targetReceipt)
	}
}

func TestBrowserPhotoInspectionRejectsMalformedReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	members := []BrowserPhotoGroupMember{{Path: "capture.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/capture.xmp", Size: 4}}
	key := BrowserPhotoGroupKey(members)
	id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
		SourceID: uuid.New(), PickerKind: "files", Label: "Choose files", Destination: "/photos", Directory: directory,
		Groups: []BrowserPhotoImportGroup{{Key: key, Members: members}},
	})
	require.NoError(t, err)
	for _, invalid := range []string{"{", "{}"} {
		_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_import_groups SET receipt_json=$1 WHERE import_id=$2 AND group_key=$3`, invalid, id.String(), key)
		require.NoError(t, err)
		_, err = s.InspectBrowserPhotoImport(ctx, id)
		require.Error(t, err)
		receipt, err := s.BrowserPhotoGroupReceipt(ctx, BrowserPhotoGroupRef{ImportID: id, GroupKey: key})
		require.Error(t, err)
		assert.Nil(t, receipt)
	}
}
