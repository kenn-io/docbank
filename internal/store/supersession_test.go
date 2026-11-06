package store

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupersessionLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	a, err := s.CreateFile(ctx, s.RootID(), "a", fakeHash("a"), 1, "text/plain")
	require.NoError(t, err)
	b, err := s.CreateFile(ctx, s.RootID(), "b", fakeHash("b"), 1, "text/plain")
	require.NoError(t, err)
	c, err := s.PutSupersession(ctx, b.ID, a.ID, a.Revision, new("replacement"))
	require.NoError(t, err)
	require.True(t, c.Changed)
	unchanged, err := s.PutSupersession(ctx, b.ID, a.ID, c.Node.Revision, new("replacement"))
	require.NoError(t, err)
	require.False(t, unchanged.Changed)
	require.Equal(t, c.Node.Revision, unchanged.Node.Revision)
	_, err = s.PutSupersession(ctx, b.ID, a.ID, a.Revision, new(""))
	require.ErrorIs(t, err, ErrStaleRevision)
	v, err := s.NodeValidity(ctx, a.ID, "2025-01-01")
	require.NoError(t, err)
	require.Equal(t, "superseded", string(v.State))
	require.True(t, v.Undated)
	require.Len(t, v.Successors, 1)
	require.Equal(t, "replacement", v.Successors[0].Note)
	c, err = s.PutSupersession(ctx, b.ID, a.ID, c.Node.Revision, new(""))
	require.NoError(t, err)
	require.True(t, c.Changed)
	v, err = s.NodeValidity(ctx, a.ID, "2025-01-01")
	require.NoError(t, err)
	require.Len(t, v.Successors, 1)
	require.Empty(t, v.Successors[0].Note)
	_, err = s.PutEffectiveWindow(ctx, b.ID, b.Revision, "2026-01-01", "2026-12-31")
	require.NoError(t, err)
	v, err = s.NodeValidity(ctx, a.ID, "2025-01-01")
	require.NoError(t, err)
	require.Equal(t, "current", string(v.State))
	v, err = s.NodeValidity(ctx, a.ID, "2027-01-01")
	require.NoError(t, err)
	require.Equal(t, "superseded", string(v.State), "an expired successor still supersedes its predecessor")
	c, err = s.DeleteSupersession(ctx, b.ID, a.ID, c.Node.Revision)
	require.NoError(t, err)
	require.True(t, c.Changed)
	c, err = s.DeleteSupersession(ctx, b.ID, a.ID, c.Node.Revision)
	require.NoError(t, err)
	require.False(t, c.Changed)
}

func TestDeleteSupersessionTrashedPredecessorOmitsPath(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	old, err := s.CreateFile(ctx, s.RootID(), "old.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	next, err := s.CreateFile(ctx, s.RootID(), "next.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	link, err := s.PutSupersession(ctx, next.ID, old.ID, old.Revision, nil)
	require.NoError(t, err)
	trashed, _, err := s.Trash(ctx, old.ID, link.Node.Revision)
	require.NoError(t, err)
	reused, err := s.CreateFile(ctx, s.RootID(), "old.txt", fakeHash("c3"), 1, "text/plain")
	require.NoError(t, err)
	removed, err := s.DeleteSupersession(ctx, next.ID, old.ID, trashed.Revision)
	require.NoError(t, err)
	require.Equal(t, old.ID, removed.Node.ID)
	require.NotNil(t, removed.Node.TrashedAt)
	live, err := s.NodeByPath(ctx, "/old.txt")
	require.NoError(t, err)
	require.Equal(t, reused.ID, live.ID)
	require.Empty(t, removed.Path)
}

func TestDeleteSupersessionTrashedEndpoint(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"predecessor", "successor"} {
		t.Run(endpoint, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			old, err := s.CreateFile(ctx, s.RootID(), "old.txt", fakeHash("a1"), 1, "text/plain")
			require.NoError(t, err)
			next, err := s.CreateFile(ctx, s.RootID(), "next.txt", fakeHash("b2"), 1, "text/plain")
			require.NoError(t, err)
			link, err := s.PutSupersession(ctx, next.ID, old.ID, old.Revision, nil)
			require.NoError(t, err)
			toTrash := link.Node
			if endpoint == "successor" {
				toTrash = next
			}
			_, _, err = s.Trash(ctx, toTrash.ID, toTrash.Revision)
			require.NoError(t, err)
			predecessor, err := s.NodeByID(ctx, old.ID)
			require.NoError(t, err)
			if endpoint == "predecessor" {
				_, err = s.DeleteSupersession(ctx, next.ID, old.ID, link.Node.Revision)
				require.ErrorIs(t, err, ErrStaleRevision)
			}
			removed, err := s.DeleteSupersession(ctx, next.ID, old.ID, predecessor.Revision)
			require.NoError(t, err)
			require.True(t, removed.Changed)
			require.Equal(t, predecessor.Revision+1, removed.Node.Revision)
			require.Equal(t, predecessor.TrashedAt, removed.Node.TrashedAt)
			plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
			require.NoError(t, err)
			_, err = s.EnableInitialAudit(ctx, plan)
			require.NoError(t, err)
		})
	}
}

func TestSupersessionAuditRejected(t *testing.T) {
	t.Parallel()
	s, _, n := newAuditedTagStore(t)
	_, err := s.PutEffectiveWindow(t.Context(), n.ID, n.Revision, "2025-01-01", "")
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	after, err := s.NodeByID(t.Context(), n.ID)
	require.NoError(t, err)
	require.Equal(t, n.Revision, after.Revision)
	other, err := s.CreateFile(t.Context(), *n.ParentID, "successor.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.DeleteSupersession(t.Context(), other.ID, n.ID, n.Revision)
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
}

func TestSupersessionGraphValidation(t *testing.T) {
	t.Parallel()
	for _, trashedMiddle := range []bool{false, true} {
		t.Run(fmt.Sprintf("trashed-middle=%t", trashedMiddle), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			var nodes []Node
			for i := range 3 {
				n, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("file-%d", i), fakeHash(fmt.Sprintf("%x", i+100)), 1, "text/plain")
				require.NoError(t, err)
				nodes = append(nodes, n)
			}
			for i := range 2 {
				c, err := s.PutSupersession(ctx, nodes[i+1].ID, nodes[i].ID, nodes[i].Revision, new(""))
				require.NoError(t, err)
				nodes[i] = c.Node
			}
			if trashedMiddle {
				_, _, err := s.Trash(ctx, nodes[1].ID, nodes[1].Revision)
				require.NoError(t, err)
			}
			_, err := s.PutSupersession(ctx, nodes[0].ID, nodes[2].ID, nodes[2].Revision, new(""))
			require.ErrorIs(t, err, ErrInvalidSupersession)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestSupersessionDegreeLimit(t *testing.T) {
	t.Parallel()
	for _, direction := range []string{"out-degree", "in-degree"} {
		t.Run(direction, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			center, err := s.CreateFile(ctx, s.RootID(), "center.txt", fakeHash("ab"), 1, "text/plain")
			require.NoError(t, err)
			for i := range 17 {
				n, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("file-%d.txt", i), fakeHash(fmt.Sprintf("%x", i+200)), 1, "text/plain")
				require.NoError(t, err)
				successor, predecessor := n, center
				if direction == "in-degree" {
					successor, predecessor = center, n
				}
				change, err := s.PutSupersession(ctx, successor.ID, predecessor.ID, predecessor.Revision, new(""))
				if i == 16 {
					require.ErrorIs(t, err, ErrInvalidSupersession)
					require.ErrorContains(t, err, "too many direct links")
					after, err := s.NodeByID(ctx, predecessor.ID)
					require.NoError(t, err)
					require.Equal(t, predecessor.Revision, after.Revision)
				} else {
					require.NoError(t, err)
					if direction == "out-degree" {
						center = change.Node
					}
				}
			}
			v, err := s.NodeValidity(ctx, center.ID, "2025-01-01")
			require.NoError(t, err)
			if direction == "in-degree" {
				require.Len(t, v.Predecessors, 16)
			} else {
				require.Len(t, v.Successors, 16)
			}
		})
	}
}

// A binary tree stays within the degree limit at the node budget.
func TestSupersessionNodeBudget(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	nodes := make([]Node, 4097)
	for i := range nodes {
		n, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("node-%d.txt", i), fakeHash("ab"), 1, "text/plain")
		require.NoError(t, err)
		nodes[i] = n
	}
	// Seed the 4095-node tree in one transaction; each public write validates
	// the whole component, so using it for every edge would be quadratic.
	tx, err := s.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO node_supersessions(successor_node_id,predecessor_node_id,note,recorded_at) VALUES(?,?,?,?)`)
	require.NoError(t, err)
	defer func() { _ = stmt.Close() }()
	for i := 1; i < 4095; i++ {
		_, err = stmt.ExecContext(ctx, nodes[i].ID, nodes[(i-1)/2].ID, "", nowRFC3339())
		require.NoError(t, err)
		require.NoError(t, bumpRevisionTx(ctx, tx, nodes[(i-1)/2].ID, nowRFC3339()))
	}
	require.NoError(t, tx.Commit())

	parent := nodes[2047]
	change, err := s.PutSupersession(ctx, nodes[4095].ID, parent.ID, parent.Revision, new(""))
	require.NoError(t, err)
	require.True(t, change.Changed)
	require.Equal(t, parent.Revision+1, change.Node.Revision)

	_, err = s.PutSupersession(ctx, nodes[4096].ID, parent.ID, change.Node.Revision, new(""))
	require.ErrorIs(t, err, ErrInvalidSupersession)
	require.ErrorContains(t, err, "too many linked files")
	after, err := s.NodeByID(ctx, parent.ID)
	require.NoError(t, err)
	require.Equal(t, change.Node.Revision, after.Revision)
	var links int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_supersessions WHERE successor_node_id=? AND predecessor_node_id=?`, nodes[4096].ID, parent.ID).Scan(&links)
	require.NoError(t, err)
	require.Zero(t, links)
}

func TestSupersessionWindowRevisionAndClear(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "policy.txt", fakeHash("a3"), 1, "text/plain")
	require.NoError(t, err)
	change, err := s.PutEffectiveWindow(ctx, n.ID, n.Revision, "2024-01-01", "2024-12-31")
	require.NoError(t, err)
	require.True(t, change.Changed)
	require.Equal(t, n.Revision+1, change.Node.Revision)
	_, err = s.DeleteEffectiveWindow(ctx, n.ID, n.Revision)
	require.ErrorIs(t, err, ErrStaleRevision)
	unchanged, err := s.PutEffectiveWindow(ctx, n.ID, change.Node.Revision, "2024-01-01", "2024-12-31")
	require.NoError(t, err)
	require.False(t, unchanged.Changed)
	require.Equal(t, change.Node.Revision, unchanged.Node.Revision)
	v, err := s.NodeValidity(ctx, n.ID, "2025-01-01")
	require.NoError(t, err)
	require.Equal(t, "expired", string(v.State))
	cleared, err := s.DeleteEffectiveWindow(ctx, n.ID, change.Node.Revision)
	require.NoError(t, err)
	require.True(t, cleared.Changed)
	require.Equal(t, change.Node.Revision+1, cleared.Node.Revision)
	unchanged, err = s.DeleteEffectiveWindow(ctx, n.ID, cleared.Node.Revision)
	require.NoError(t, err)
	require.False(t, unchanged.Changed)
	require.Equal(t, cleared.Node.Revision, unchanged.Node.Revision)
	v, err = s.NodeValidity(ctx, n.ID, "2025-01-01")
	require.NoError(t, err)
	require.Equal(t, "current", string(v.State))
}

func TestDeleteEffectiveWindowTrashedFile(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "policy.txt", fakeHash("a3"), 1, "text/plain")
	require.NoError(t, err)
	window, err := s.PutEffectiveWindow(ctx, n.ID, n.Revision, "2024-01-01", "2024-12-31")
	require.NoError(t, err)
	trashed, _, err := s.Trash(ctx, n.ID, window.Node.Revision)
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "policy.txt", fakeHash("b4"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.DeleteEffectiveWindow(ctx, n.ID, window.Node.Revision)
	require.ErrorIs(t, err, ErrStaleRevision)
	cleared, err := s.DeleteEffectiveWindow(ctx, n.ID, trashed.Revision)
	require.NoError(t, err)
	require.True(t, cleared.Changed)
	require.Equal(t, n.ID, cleared.Node.ID)
	require.Equal(t, trashed.Revision+1, cleared.Node.Revision)
	require.Equal(t, trashed.TrashedAt, cleared.Node.TrashedAt)
	require.Empty(t, cleared.Path)
	unchanged, err := s.DeleteEffectiveWindow(ctx, n.ID, cleared.Node.Revision)
	require.NoError(t, err)
	require.False(t, unchanged.Changed)
	require.Equal(t, cleared.Node.Revision, unchanged.Node.Revision)
	plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
}

func TestSupersessionEndpointGuards(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "policy.txt", fakeHash("a4"), 1, "text/plain")
	require.NoError(t, err)
	dir, err := s.Mkdir(ctx, s.RootID(), "policies")
	require.NoError(t, err)
	other, err := s.CreateFile(ctx, s.RootID(), "replacement.txt", fakeHash("a8"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.PutSupersession(ctx, n.ID, n.ID, n.Revision, new(""))
	require.ErrorIs(t, err, ErrInvalidSupersession)
	_, err = s.PutSupersession(ctx, dir.ID, n.ID, n.Revision, new(""))
	require.ErrorIs(t, err, ErrNotFile)
	_, err = s.PutSupersession(ctx, n.ID, dir.ID, dir.Revision, new(""))
	require.ErrorIs(t, err, ErrNotFile)
	_, err = s.PutEffectiveWindow(ctx, dir.ID, dir.Revision, "2025-01-01", "")
	require.ErrorIs(t, err, ErrNotFile)
	_, err = s.PutEffectiveWindow(ctx, n.ID, n.Revision, "", "")
	require.ErrorIs(t, err, ErrInvalidSupersession)
	_, err = s.PutEffectiveWindow(ctx, n.ID, n.Revision, "2025-02-30", "")
	require.ErrorIs(t, err, ErrInvalidSupersession)
	require.ErrorContains(t, err, "invalid calendar date")
	longNote := strings.Repeat("x", 1025)
	_, err = s.PutSupersession(ctx, other.ID, n.ID, n.Revision, &longNote)
	require.ErrorIs(t, err, ErrInvalidSupersession)
	trashed, _, err := s.Trash(ctx, n.ID, n.Revision)
	require.NoError(t, err)
	_, err = s.PutSupersession(ctx, n.ID, other.ID, other.Revision, new(""))
	require.ErrorIs(t, err, ErrNotFound)
	_, err = s.PutEffectiveWindow(ctx, n.ID, trashed.Revision, "2025-01-01", "")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSupersessionTrashRestoreAndPurge(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	old, err := s.CreateFile(ctx, s.RootID(), "old.txt", fakeHash("a6"), 1, "text/plain")
	require.NoError(t, err)
	next, err := s.CreateFile(ctx, s.RootID(), "new.txt", fakeHash("a7"), 1, "text/plain")
	require.NoError(t, err)
	link, err := s.PutSupersession(ctx, next.ID, old.ID, old.Revision, new("replacement"))
	require.NoError(t, err)
	window, err := s.PutEffectiveWindow(ctx, next.ID, next.Revision, "2025-01-01", "")
	require.NoError(t, err)
	trashed, _, err := s.Trash(ctx, next.ID, window.Node.Revision)
	require.NoError(t, err)
	v, err := s.NodeValidity(ctx, old.ID, "2025-01-01")
	require.NoError(t, err)
	require.Equal(t, "current", string(v.State))
	require.Empty(t, v.Successors)
	var snapshot bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &snapshot))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
	var out bytes.Buffer
	require.NoError(t, target.ExportMetadata(ctx, &out))
	require.Equal(t, snapshot.String(), out.String())
	restored, _, err := target.Restore(ctx, trashed.ID, trashed.Revision)
	require.NoError(t, err)
	unchanged, err := target.PutSupersession(ctx, restored.ID, old.ID, link.Node.Revision, nil)
	require.NoError(t, err)
	require.False(t, unchanged.Changed)
	v, err = target.NodeValidity(ctx, old.ID, "2025-01-01")
	require.NoError(t, err)
	require.Equal(t, "superseded", string(v.State))
	require.False(t, v.Undated)
	require.Len(t, v.Successors, 1)
	require.Equal(t, "replacement", v.Successors[0].Note)
	_, _, err = target.Trash(ctx, restored.ID, restored.Revision)
	require.NoError(t, err)
	report, err := target.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Deleted)
	var links, windows int
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_supersessions`).Scan(&links))
	require.NoError(t, target.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_effective_windows`).Scan(&windows))
	require.Zero(t, links)
	require.Zero(t, windows)
	require.NoError(t, target.ValidateMetadata(ctx))
}

func TestSupersessionMetadataRejectsInvalidDeclarations(t *testing.T) {
	t.Parallel()
	source := newTestStore(t)
	ctx := t.Context()
	n, err := source.CreateFile(ctx, source.RootID(), "source.txt", fakeHash("a9"), 1, "text/plain")
	require.NoError(t, err)
	other, err := source.CreateFile(ctx, source.RootID(), "previous.txt", fakeHash("aa"), 1, "text/plain")
	require.NoError(t, err)
	next, err := source.CreateFile(ctx, source.RootID(), "next.txt", fakeHash("ab"), 1, "text/plain")
	require.NoError(t, err)
	_, err = source.PutSupersession(ctx, other.ID, n.ID, n.Revision, new(""))
	require.NoError(t, err)
	_, err = source.PutSupersession(ctx, next.ID, other.ID, other.Revision, new(""))
	require.NoError(t, err)
	var snapshot bytes.Buffer
	require.NoError(t, source.ExportMetadata(ctx, &snapshot))
	for _, tt := range []struct {
		name, record string
		wantErr      error
		wantMessage  string
	}{
		{"directory endpoint", fmt.Sprintf(`{"type":"node_supersession","successor_node_id":%d,"predecessor_node_id":%d,"note":"","recorded_at":%q}`, n.ID, source.RootID(), nowRFC3339()), ErrInvalidSupersession, ""},
		{"long note", fmt.Sprintf(`{"type":"node_supersession","successor_node_id":%d,"predecessor_node_id":%d,"note":%q,"recorded_at":%q}`, next.ID, n.ID, strings.Repeat("x", 1025), nowRFC3339()), ErrInvalidSupersession, "invalid supersession note"},
		{"directory window", fmt.Sprintf(`{"type":"node_effective_window","node_id":%d,"effective_from":"2025-01-01","effective_to":null}`, source.RootID()), ErrInvalidSupersession, ""},
		{"calendar date", fmt.Sprintf(`{"type":"node_effective_window","node_id":%d,"effective_from":"2026-02-30","effective_to":null}`, n.ID), nil, "invalid calendar date"},
		{"empty window", fmt.Sprintf(`{"type":"node_effective_window","node_id":%d,"effective_from":null,"effective_to":null}`, n.ID), ErrInvalidSupersession, ""},
		{"cycle", fmt.Sprintf(`{"type":"node_supersession","successor_node_id":%d,"predecessor_node_id":%d,"note":"","recorded_at":%q}`, n.ID, next.ID, nowRFC3339()), ErrInvalidSupersession, "supersession cycle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := newTestStore(t)
			err := target.ImportMetadata(ctx, strings.NewReader(snapshot.String()+tt.record+"\n"))
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			}
			if tt.wantMessage != "" {
				require.ErrorContains(t, err, tt.wantMessage)
			}
			_, err = target.NodeByPath(ctx, "/source.txt")
			require.ErrorIs(t, err, ErrNotFound, "failed import must not publish partial authority")
			require.NoError(t, target.ImportMetadata(ctx, bytes.NewReader(snapshot.Bytes())))
		})
	}
}

func TestAuditEnrollmentRejectsValidityDeclarations(t *testing.T) {
	t.Parallel()
	for _, declaration := range []string{"link", "window"} {
		t.Run(declaration, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			a, err := s.CreateFile(ctx, s.RootID(), "old.txt", fakeHash("a1"), 1, "text/plain")
			require.NoError(t, err)
			b, err := s.CreateFile(ctx, s.RootID(), "new.txt", fakeHash("b2"), 1, "text/plain")
			require.NoError(t, err)
			plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
			require.NoError(t, err)
			if declaration == "link" {
				_, err = s.PutSupersession(ctx, b.ID, a.ID, a.Revision, new("replacement"))
				require.NoError(t, err)
			}
			if declaration == "window" {
				_, err = s.PutEffectiveWindow(ctx, b.ID, b.Revision, "2025-01-01", "2025-12-31")
				require.NoError(t, err)
			}
			_, err = s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			require.ErrorContains(t, err, "document validity declarations")
			_, err = s.EnableInitialAudit(ctx, plan)
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			active, err := auditAuthorityActiveTx(ctx, s.db)
			require.NoError(t, err)
			require.False(t, active)
		})
	}
}

func TestAuditedImportRejectsValidityDeclarations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	n, err := s.CreateFile(ctx, s.RootID(), "old.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	other, err := s.CreateFile(ctx, s.RootID(), "new.txt", fakeHash("b2"), 1, "text/plain")
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	var baseline bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &baseline))
	// Hand-edit an audited stream to include unsupported declarations while
	// retaining every audit record.
	link := fmt.Sprintf("{\"type\":\"node_supersession\",\"successor_node_id\":%d,\"predecessor_node_id\":%d,\"note\":\"replacement\",\"recorded_at\":%q}\n", other.ID, n.ID, nowRFC3339())
	window := fmt.Sprintf("{\"type\":\"node_effective_window\",\"node_id\":%d,\"effective_from\":\"2025-01-01\",\"effective_to\":null}\n", other.ID)
	for _, tc := range []struct{ name, declarations string }{
		{"window_only", window},
		{"link_only", link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := newTestStore(t)
			err := target.ImportMetadata(ctx, strings.NewReader(baseline.String()+tc.declarations))
			require.ErrorIs(t, err, ErrAuditMutationUnsupported)
			require.ErrorContains(t, err, "document validity declarations")
			active, err := auditAuthorityActiveTx(ctx, target.db)
			require.NoError(t, err)
			require.False(t, active, "rejected import must roll back audit authority")
		})
	}
}
