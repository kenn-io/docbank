package store

import (
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func checkBrowserPhotoReceiptOutcomes(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	s := open(t)
	ctx := t.Context()
	commit := func(declarations []BrowserPhotoGroupMember, inputs []PhotoImportMember) PhotoImportResult {
		t.Helper()
		directory, err := s.PrepareIngestDirectory(ctx, "/photos")
		require.NoError(t, err)
		id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
			SourceID: uuid.New(), Label: "Choose files", Destination: "/photos", Directory: directory,
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
		wrong := append([]string(nil), hashes...)
		wrong[0] = fakeHash("ff")
		require.Error(t, s.PinBrowserPhotoGroupHashes(ctx, id, 0, wrong))
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
	directory, err := s.NodeByPath(ctx, "/photos")
	require.NoError(t, err)
	children, err := s.Children(ctx, directory.ID)
	require.NoError(t, err)
	require.Len(t, children, 1)
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
		SourceID: uuid.New(), Label: "Choose files", Destination: "/photos", Directory: directory,
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

func TestBrowserPhotoInspectionRejectsFutureReceipt(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	members := []BrowserPhotoGroupMember{{Path: "capture.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/capture.xmp", Size: 4}}
	id, err := s.RegisterBrowserPhotoImport(ctx, BrowserPhotoImport{
		SourceID: uuid.New(), Label: "Choose files", Destination: "/photos", Directory: directory,
		Groups: []BrowserPhotoImportGroup{{Members: members}},
	})
	require.NoError(t, err)
	// A newer writer may save a receipt this reader cannot interpret.
	_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_import_groups SET receipt_json=$1 WHERE import_id=$2 AND ordinal=$3`, `{"version":2,"status":"added"}`, id.String(), 0)
	require.NoError(t, err)
	_, err = s.InspectBrowserPhotoImport(ctx, id)
	require.Error(t, err)
	receipt, err := s.BrowserPhotoGroupReceipt(ctx, BrowserPhotoGroupRef{ImportID: id, Ordinal: 0})
	require.Error(t, err)
	assert.Nil(t, receipt)
}

func TestBrowserPhotoImportMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.InspectBrowserPhotoImport(t.Context(), uuid.New())
	require.ErrorIs(t, err, ErrNotFound)
}

func TestBrowserPhotoImportRejectsFutureDeclaration(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	plan := BrowserPhotoImport{SourceID: uuid.New(), Label: "Photos", Destination: "/photos", Directory: directory, Groups: []BrowserPhotoImportGroup{{Members: []BrowserPhotoGroupMember{{Path: "a.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/a.xmp", Size: 1}}}}}
	id, err := s.RegisterBrowserPhotoImport(ctx, plan)
	require.NoError(t, err)
	// A newer writer may save a declaration this reader cannot interpret.
	_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_imports SET format_version=2 WHERE import_id=$1`, id.String())
	require.NoError(t, err)
	_, err = s.InspectBrowserPhotoImport(ctx, id)
	require.Error(t, err)
}

func TestBrowserPhotoImportListExcludesFutureDeclarations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	directory, err := s.PrepareIngestDirectory(ctx, "/photos")
	require.NoError(t, err)
	plan := BrowserPhotoImport{Label: "Photos", Destination: "/photos", Directory: directory, Groups: []BrowserPhotoImportGroup{{Members: []BrowserPhotoGroupMember{{Path: "a.xmp", Role: PhotoRoleSidecar, SourcePath: "browser:source/a.xmp", Size: 1}}}}}
	ids := make([]uuid.UUID, 3)
	for i := range ids {
		plan.SourceID = uuid.New()
		ids[i], err = s.RegisterBrowserPhotoImport(ctx, plan)
		require.NoError(t, err)
	}
	// A newer declaration must not hide supported imports from paginated results.
	_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_imports SET format_version=2,created_at='2026-01-03T00:00:00Z' WHERE import_id=$1`, ids[0].String())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_imports SET created_at='2026-01-02T00:00:00Z' WHERE import_id=$1`, ids[1].String())
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE browser_photo_imports SET created_at='2026-01-01T00:00:00Z' WHERE import_id=$1`, ids[2].String())
	require.NoError(t, err)
	for offset, id := range ids[1:] {
		page, err := s.ListBrowserPhotoImports(ctx, offset, 1)
		require.NoError(t, err)
		require.Len(t, page, 1)
		assert.Equal(t, id, page[0].ID)
	}
}
