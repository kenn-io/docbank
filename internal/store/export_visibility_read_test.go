package store

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
)

// RED: frozen source membership remains readable after a new head, but a
// withdrawn ancestor or missing retained member must withhold the archive.
func TestExportSourceVisibilityReadChecksOwnerAndLiveAncestry(t *testing.T) {
	s := newTestStore(t)
	dir, err := s.Mkdir(t.Context(), s.RootID(), "synthetic folder")
	require.NoError(t, err)
	node, err := s.CreateFile(t.Context(), dir.ID, "synthetic.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "explicit",
		Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID))
	require.ErrorIs(t, s.CheckExportSourceVisibility(t.Context(), "other", source.ID), ErrNotFound)

	_, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision, fakeHash("b2"), 14, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID),
		"frozen historical versions remain visible while their file is live")

	dir, err = s.NodeByID(t.Context(), dir.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), dir.ID, dir.Revision)
	require.NoError(t, err)
	require.ErrorIs(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID), ErrExportVisibilityChanged)
}

func TestExportSourceVisibilityReadFailsClosedOnMissingRetainedMember(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "explicit",
		Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}},
	}, nil)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `DELETE FROM export_members WHERE source_id=?`, source.ID)
	require.NoError(t, err)
	require.ErrorIs(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID), ErrExportVisibilityChanged)
}

func TestExportSourceVisibilityReadRejectsRetainedAuthorityDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *Store, bundle.Source, Node, Node)
	}{
		{"replaced intact member", func(t *testing.T, s *Store, source bundle.Source, original, replacement Node) {
			t.Helper()
			raw, err := canonical.Marshal(bundle.Member{NodeID: replacement.ID, VersionID: replacement.CurrentVersionID, SHA256: replacement.BlobHash, Size: replacement.Size})
			require.NoError(t, err)
			_, err = s.db.ExecContext(t.Context(), `UPDATE export_members SET node_id=?,version_id=?,blob_hash=?,canonical_json=? WHERE source_id=?`,
				replacement.ID, replacement.CurrentVersionID, replacement.BlobHash, raw, source.ID)
			require.NoError(t, err)
		}},
		{"canonical size drift", func(t *testing.T, s *Store, source bundle.Source, original, _ Node) {
			t.Helper()
			raw, err := canonical.Marshal(bundle.Member{NodeID: original.ID, VersionID: original.CurrentVersionID, SHA256: original.BlobHash, Size: original.Size + 1})
			require.NoError(t, err)
			_, err = s.db.ExecContext(t.Context(), `UPDATE export_members SET canonical_json=? WHERE source_id=?`, raw, source.ID)
			require.NoError(t, err)
		}},
		{"sealed byte total drift", func(t *testing.T, s *Store, source bundle.Source, _, _ Node) {
			t.Helper()
			source.SourceBytes++
			raw, err := canonical.Marshal(source)
			require.NoError(t, err)
			_, err = s.db.ExecContext(t.Context(), `UPDATE export_sources SET canonical_json=? WHERE id=?`, raw, source.ID)
			require.NoError(t, err)
		}},
		{"sealed member hash drift", func(t *testing.T, s *Store, source bundle.Source, _, _ Node) {
			t.Helper()
			source.MemberHash = fakeHash("f1")
			raw, err := canonical.Marshal(source)
			require.NoError(t, err)
			_, err = s.db.ExecContext(t.Context(), `UPDATE export_sources SET canonical_json=? WHERE id=?`, raw, source.ID)
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			original, err := s.CreateFile(t.Context(), s.RootID(), "original.txt", fakeHash("a1"), 12, "text/plain")
			require.NoError(t, err)
			source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
				OperationID: uuid.NewString(), Kind: "explicit",
				Members: []bundle.Member{{NodeID: original.ID, VersionID: original.CurrentVersionID, SHA256: original.BlobHash, Size: original.Size}},
			}, nil)
			require.NoError(t, err)
			replacement, err := s.CreateFile(t.Context(), s.RootID(), "replacement.txt", fakeHash("b2"), 12, "text/plain")
			require.NoError(t, err)
			require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID))
			tc.mutate(t, s, source, original, replacement)
			require.ErrorIs(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID), ErrExportVisibilityChanged)
		})
	}
}

func TestExportSourceVisibilityReadChecksEveryBoundedPage(t *testing.T) {
	s := newTestStore(t)
	members := seedExportMembers(t, s, 0, 251)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "upload", Total: len(members), MemberHash: ExportMemberHash(members),
	}, nil)
	require.NoError(t, err)
	require.NoError(t, s.PutExportChunk(t.Context(), "owner", source.ID, 0, members))
	_, err = s.SealExportSource(t.Context(), "owner", source.ID)
	require.NoError(t, err)
	require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID))
	last, err := s.NodeByID(t.Context(), members[len(members)-1].NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), last.ID, last.Revision)
	require.NoError(t, err)
	require.ErrorIs(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID), ErrExportVisibilityChanged)
}

func TestExportPlanVisibilityWithholdsTrashedAttachmentChild(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "synthetic-parent.eml")
	view, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	publication, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "synthetic-child"))
	require.NoError(t, err)
	require.NotEmpty(t, publication.Relations)
	require.NotNil(t, publication.Relations[0].Child)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{view.Version.NodeID},
	}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{
		OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "attachment_original"}},
	})
	require.NoError(t, err)
	require.NoError(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID))
	child, err := s.NodeByID(t.Context(), publication.Relations[0].Child.NodeID)
	require.NoError(t, err)
	_, _, err = s.Trash(t.Context(), child.ID, child.Revision)
	require.NoError(t, err)
	require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID),
		"the parent source stays visible after its published child is trashed")
	require.ErrorIs(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID), ErrExportVisibilityChanged)
}

func TestExportPlanVisibilityRejectsSourceIdentityDrift(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{node.ID},
	}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{
		OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "original"}},
	})
	require.NoError(t, err)
	require.NoError(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID))
	source.Kind = "explicit"
	raw, err := canonical.Marshal(source)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_sources SET canonical_json=? WHERE id=?`, raw, source.ID)
	require.NoError(t, err)
	require.ErrorIs(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID), ErrExportVisibilityChanged)
}

func TestExportPlanVisibilityTreatsOversizedRetainedRowAsWithdrawal(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "synthetic.txt", fakeHash("a1"), 12, "text/plain")
	require.NoError(t, err)
	source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
		OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{node.ID},
	}, nil)
	require.NoError(t, err)
	plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{
		OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles: []bundle.RolePolicy{{Role: "original"}},
	})
	require.NoError(t, err)
	require.NoError(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID))
	_, err = s.db.ExecContext(t.Context(), `UPDATE export_documents SET canonical_json=? WHERE plan_id=?`,
		bytes.Repeat([]byte("x"), bundle.MaxMemberBytes+1), plan.ID)
	require.NoError(t, err)
	require.ErrorIs(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID), ErrExportVisibilityChanged)
}

func makeExportNodeParentMissing(t *testing.T, s *Store, id int64) {
	t.Helper()
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `UPDATE nodes SET parent_id=? WHERE id=?`, int64(1<<60), id)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=ON`)
	require.NoError(t, err)
}

func TestExportVisibilityReadRejectsMissingAncestors(t *testing.T) {
	t.Run("sealed source member", func(t *testing.T) {
		s := newTestStore(t)
		dir, err := s.Mkdir(t.Context(), s.RootID(), "synthetic folder")
		require.NoError(t, err)
		node, err := s.CreateFile(t.Context(), dir.ID, "synthetic.txt", fakeHash("a1"), 12, "text/plain")
		require.NoError(t, err)
		source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
			OperationID: uuid.NewString(), Kind: "explicit",
			Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}},
		}, nil)
		require.NoError(t, err)
		require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID))
		makeExportNodeParentMissing(t, s, dir.ID)
		require.ErrorIs(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID), ErrExportVisibilityChanged)
	})

	t.Run("planned attachment child", func(t *testing.T) {
		s := newTestStore(t)
		f := newEmailFixture(t, s, "synthetic-parent.eml")
		view, err := s.PublishEmailGeneration(t.Context(), f.publication)
		require.NoError(t, err)
		publication, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "synthetic-child"))
		require.NoError(t, err)
		require.NotEmpty(t, publication.Relations)
		require.NotNil(t, publication.Relations[0].Child)
		source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
			OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{view.Version.NodeID},
		}, nil)
		require.NoError(t, err)
		plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{
			OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
			Roles: []bundle.RolePolicy{{Role: "attachment_original"}},
		})
		require.NoError(t, err)
		require.NoError(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID))
		makeExportNodeParentMissing(t, s, publication.Relations[0].Child.NodeID)
		require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID))
		require.ErrorIs(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID), ErrExportVisibilityChanged)
	})
}

func TestExportVisibilityReadRejectsParentCycles(t *testing.T) {
	t.Run("sealed source member", func(t *testing.T) {
		s := newTestStore(t)
		outer, err := s.Mkdir(t.Context(), s.RootID(), "synthetic outer")
		require.NoError(t, err)
		inner, err := s.Mkdir(t.Context(), outer.ID, "synthetic inner")
		require.NoError(t, err)
		node, err := s.CreateFile(t.Context(), inner.ID, "synthetic.txt", fakeHash("a1"), 12, "text/plain")
		require.NoError(t, err)
		source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
			OperationID: uuid.NewString(), Kind: "explicit",
			Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}},
		}, nil)
		require.NoError(t, err)
		require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID))
		_, err = s.db.ExecContext(t.Context(), `UPDATE nodes SET parent_id=? WHERE id=?`, inner.ID, outer.ID)
		require.NoError(t, err, "the foreign key permits a live parent cycle")
		require.ErrorIs(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID), ErrExportVisibilityChanged)
	})

	t.Run("planned attachment child", func(t *testing.T) {
		s := newTestStore(t)
		f := newEmailFixture(t, s, "synthetic-parent.eml")
		view, err := s.PublishEmailGeneration(t.Context(), f.publication)
		require.NoError(t, err)
		publication, err := s.PublishEmailDocuments(t.Context(), attachmentRequest(t, s, view, "synthetic-child"))
		require.NoError(t, err)
		require.NotEmpty(t, publication.Relations)
		require.NotNil(t, publication.Relations[0].Child)
		source, err := s.CreateExportSource(t.Context(), "owner", bundle.SourceRequest{
			OperationID: uuid.NewString(), Kind: "nodes", NodeIDs: []int64{view.Version.NodeID},
		}, nil)
		require.NoError(t, err)
		plan, err := s.CreateExportPlan(t.Context(), "owner", bundle.PlanRequest{
			OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash,
			Roles: []bundle.RolePolicy{{Role: "attachment_original"}},
		})
		require.NoError(t, err)
		require.NoError(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID))
		childID := publication.Relations[0].Child.NodeID
		_, err = s.db.ExecContext(t.Context(), `UPDATE nodes SET parent_id=? WHERE id=?`, childID, childID)
		require.NoError(t, err, "the foreign key permits a live parent cycle")
		require.NoError(t, s.CheckExportSourceVisibility(t.Context(), "owner", source.ID))
		require.ErrorIs(t, s.CheckExportPlanVisibility(t.Context(), "owner", plan.ID), ErrExportVisibilityChanged)
	})
}
