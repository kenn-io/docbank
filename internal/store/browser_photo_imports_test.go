package store

import (
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkBrowserPhotoGroupReceiptReplay(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	members := []BrowserPhotoGroupMember{{Path: "Trip/capture.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/Trip/capture.xmp", Size: 4}}
	plan := BrowserPhotoImport{
		SourceID: uuid.New(), PickerKind: "folder", Label: "Trip", Destination: "/photos", Directory: directory,
		Groups: []BrowserPhotoImportGroup{{Members: members}},
	}
	id, err := s.RegisterBrowserPhotoImport(ctx, plan)
	require.NoError(t, err)
	digest := fakeHash("a1")
	require.NoError(t, s.PinBrowserPhotoGroupHashes(ctx, id, 0, []string{digest}))
	require.Error(t, s.PinBrowserPhotoGroupHashes(ctx, id, 0, []string{fakeHash("b2")}))
	run, err := s.BeginIngest(ctx, "photo-import", "browser: Trip")
	require.NoError(t, err)
	group := PhotoImportGroup{
		BrowserGroup: &BrowserPhotoGroupRef{ImportID: id, Ordinal: 0},
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
	directoryNode, err := s.NodeByPath(ctx, "/photos")
	require.NoError(t, err)
	children, err := s.Children(ctx, directoryNode.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, first.Nodes[0].ID, children[0].ID)
}

func checkBrowserPhotoReceiptOutcomes(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	commit := func(declarations []BrowserPhotoGroupMember, inputs []PhotoImportMember) PhotoImportResult {
		t.Helper()
		directory, err := s.PrepareIngestDirectory(ctx, "/photos")
		require.NoError(t, err)
		id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
			SourceID: uuid.New(), PickerKind: "files", Label: "Choose files", Destination: "/photos", Directory: directory,
			Groups: []BrowserPhotoImportGroup{{Members: declarations}},
		})
		require.NoError(t, err)
		run, err := s.BrowserPhotoImportRun(ctx, id)
		require.NoError(t, err)
		hashes := make([]string, len(inputs))
		for i, input := range inputs {
			hashes[i] = input.BlobHash
		}
		require.NoError(t, s.PinBrowserPhotoGroupHashes(ctx, id, 0, hashes))
		group := PhotoImportGroup{BrowserGroup: &BrowserPhotoGroupRef{ImportID: id, Ordinal: 0}, Members: inputs, CreateOnly: true}
		result, err := s.IngestPhotoGroup(ctx, run, group)
		require.NoError(t, err)
		replay, err := s.IngestPhotoGroup(ctx, run, group)
		require.NoError(t, err)
		assert.Equal(t, result.Added, replay.Added)
		assert.Equal(t, result.Skipped, replay.Skipped)
		if result.Ambiguity != nil {
			require.NotNil(t, replay.Ambiguity)
			assert.Equal(t, result.Ambiguity.Reason, replay.Ambiguity.Reason)
		} else {
			assert.Nil(t, replay.Ambiguity)
		}
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
	id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
		SourceID: uuid.New(), PickerKind: "files", Label: "Choose files", Destination: "/photos", Directory: directory,
		Groups: []BrowserPhotoImportGroup{{Members: members}},
	})
	require.NoError(t, err)
	digest := fakeHash("a1")
	require.NoError(t, s.PinBrowserPhotoGroupHashes(ctx, id, 0, []string{digest}))
	_, err = s.db.ExecContext(ctx, `CREATE TRIGGER fail_browser_receipt BEFORE UPDATE OF receipt_json ON browser_photo_import_groups BEGIN SELECT RAISE(ABORT, 'receipt failed'); END`)
	require.NoError(t, err)
	run, err := s.BeginIngest(ctx, "photo-import", "browser: Choose files")
	require.NoError(t, err)
	_, err = s.IngestPhotoGroup(ctx, run, PhotoImportGroup{
		BrowserGroup: &BrowserPhotoGroupRef{ImportID: id, Ordinal: 0},
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

func TestBrowserPhotoInspectionRejectsUnknownReceipts(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	members := []BrowserPhotoGroupMember{{Path: "capture.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/capture.xmp", Size: 4}}
	id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
		SourceID: uuid.New(), PickerKind: "files", Label: "Choose files", Destination: "/photos", Directory: directory,
		Groups: []BrowserPhotoImportGroup{{Members: members}},
	})
	require.NoError(t, err)
	for _, invalid := range []string{`{"version":2,"status":"added"}`, `{"version":1,"status":"future"}`} {
		_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_import_groups SET receipt_json=$1 WHERE import_id=$2 AND ordinal=$3`, invalid, id.String(), 0)
		require.NoError(t, err)
		_, err = s.InspectBrowserPhotoImport(ctx, id)
		require.Error(t, err)
		receipt, err := s.BrowserPhotoGroupReceipt(ctx, BrowserPhotoGroupRef{ImportID: id, Ordinal: 0})
		require.Error(t, err)
		assert.Nil(t, receipt)
	}
}

func TestBrowserPhotoImportMissing(t *testing.T) {
	s := newTestStore(t)
	_, err := s.InspectBrowserPhotoImport(t.Context(), uuid.New())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestBrowserPhotoImportRejectsChangedOrFutureDeclaration(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	plan := BrowserPhotoImport{SourceID: uuid.New(), PickerKind: "files", Label: "Photos", Destination: "/photos", Directory: directory, Groups: []BrowserPhotoImportGroup{{Members: []BrowserPhotoGroupMember{{Path: "a.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/a.xmp", Size: 1}}}}}
	id, err := s.RegisterBrowserPhotoImport(ctx, plan)
	require.NoError(t, err)
	again, err := s.RegisterBrowserPhotoImport(ctx, plan)
	require.NoError(t, err)
	require.Equal(t, id, again)
	plan.Groups[0].Members[0].Size = 2
	_, err = s.RegisterBrowserPhotoImport(ctx, plan)
	require.Error(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_imports SET format_version=2 WHERE import_id=$1`, id.String())
	require.NoError(t, err)
	_, err = s.InspectBrowserPhotoImport(ctx, id)
	require.Error(t, err)
}
