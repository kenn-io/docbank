package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

// These tests fail if lookups use display text as identity, discard revisions,
// bypass collection eligibility, or convert database failures into no matches.
func TestQueryResolverStableReferences(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	tag, err := s.CreateTag(ctx, "Café review")
	require.NoError(t, err)
	run := createCollectionRun(t, s, "synthetic.txt", "a1")
	label, err := s.SetCollectionLabel(ctx, run.ID(), 1, new("Café documents"))
	require.NoError(t, err)
	saved, err := s.CreateSavedQuery(ctx, "Café query", "", SavedQueryKindQuery,
		[]byte(`{"text":"alpha OR beta","syntax":"advanced","filters":{"size_min":7}}`))
	require.NoError(t, err)
	r := queryResolver{q: s.db}
	for _, test := range []struct {
		kind     query.ReferenceKind
		name, id string
		revision int64
	}{
		{query.ReferenceTag, "Cafe\u0301 review", tag.ID, tag.Revision},
		{query.ReferenceCollection, "Cafe\u0301 documents", run.ID(), label.Revision},
		{query.ReferenceSaved, "Cafe\u0301 query", saved.ID, saved.Revision},
	} {
		byName, err := r.Resolve(ctx, test.kind, test.name, false)
		require.NoError(t, err)
		require.Equal(t, query.Dependency{Kind: test.kind, ID: test.id, Revision: test.revision}, byName.Dependency)
		byID, err := r.Resolve(ctx, test.kind, test.id, true)
		require.NoError(t, err)
		require.Equal(t, byName, byID)
		if test.kind == query.ReferenceSaved {
			require.NotNil(t, byID.Query)
			require.Equal(t, "alpha OR beta", byID.Query.Text)
			require.Equal(t, int64(7), byID.Query.Filters.SizeMin)
		}
		_, err = r.Resolve(ctx, test.kind, "missing", false)
		require.ErrorIs(t, err, query.ErrUnknownReference)
		_, err = r.Resolve(ctx, test.kind, test.name, true)
		require.ErrorIs(t, err, query.ErrUnknownReference)
	}
}

func TestQueryResolverCollectionEligibilityAndSavedKind(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	r := queryResolver{q: s.db}
	empty, err := s.BeginIngest(ctx, "cli", "Synthetic empty run")
	require.NoError(t, err)
	_, err = r.Resolve(ctx, query.ReferenceCollection, empty.ID(), true)
	require.ErrorIs(t, err, query.ErrUnknownReference)
	active := createCollectionRun(t, s, "active.txt", "b2")
	got, err := r.Resolve(ctx, query.ReferenceCollection, active.ID(), true)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Dependency.Revision)
	highlight, err := s.CreateSavedQuery(ctx, "Literal marks", "", SavedQueryKindHighlightSet, []byte(savedHighlightPayload))
	require.NoError(t, err)
	_, err = r.Resolve(ctx, query.ReferenceSaved, highlight.ID, true)
	require.ErrorIs(t, err, query.ErrUnknownReference)
}

func TestQueryResolverUsesSuppliedReadSnapshotAndPreservesBackendErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	saved, err := s.CreateSavedQuery(ctx, "Original", "", SavedQueryKindQuery, []byte(`{"text":"alpha"}`))
	require.NoError(t, err)
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	})
	// Store mutations default to IMMEDIATE; match the generation reader's
	// explicit DEFERRED transaction so a writer can advance concurrently.
	_, err = conn.ExecContext(ctx, "BEGIN DEFERRED")
	require.NoError(t, err)
	r := queryResolver{q: conn}
	first, err := r.Resolve(ctx, query.ReferenceSaved, "Original", false)
	require.NoError(t, err)
	_, err = s.UpdateSavedQuery(ctx, saved.ID, saved.Revision, SavedQueryPatch{Name: new("Renamed")})
	require.NoError(t, err)
	second, err := r.Resolve(ctx, query.ReferenceSaved, "Original", false)
	require.NoError(t, err)
	require.Equal(t, first, second)
	_, err = (queryResolver{q: s.db}).Resolve(ctx, query.ReferenceSaved, "Original", false)
	require.ErrorIs(t, err, query.ErrUnknownReference)
	_, err = conn.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	_, err = r.Resolve(ctx, query.ReferenceSaved, saved.ID, true)
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.NotErrorIs(t, err, query.ErrUnknownReference)
}

func TestQueryResolverExpansionFailsClosedAgainstRealStore(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	r := queryResolver{q: s.db}
	deleted, err := s.CreateSavedQuery(ctx, "deleted", "", SavedQueryKindQuery, []byte(`{"text":"alpha"}`))
	require.NoError(t, err)
	_, err = s.DeleteSavedQuery(ctx, deleted.ID, deleted.Revision)
	require.NoError(t, err)
	for _, text := range []string{`NOT tag:missing`, `NOT collection:missing`, `saved:deleted`} {
		input, err := query.Parse([]byte(`{"syntax":"advanced"}`))
		require.NoError(t, err)
		input.Text = text
		_, err = query.ResolveQuery(ctx, input, r)
		var positioned *query.ExpressionError
		require.ErrorAs(t, err, &positioned)
		require.GreaterOrEqual(t, positioned.Offset, 0)
		require.LessOrEqual(t, positioned.End, len(text))
	}
	saved, err := s.CreateSavedQuery(ctx, "Evidence", "", SavedQueryKindQuery,
		[]byte(`{"syntax":"advanced","text":"alpha OR beta","filters":{"size_min":7}}`))
	require.NoError(t, err)
	input, err := query.Parse([]byte(`{"syntax":"advanced","text":"gamma AND saved:Evidence","filters":{"size_max":20}}`))
	require.NoError(t, err)
	resolved, err := query.ResolveQuery(ctx, input, r)
	require.NoError(t, err)
	require.Equal(t, input, resolved.Query)
	require.Equal(t, []query.Dependency{{Kind: query.ReferenceSaved, ID: saved.ID, Revision: 1}}, resolved.Dependencies)
	reference := resolved.Expression.Children[1].Children[0]
	require.NotNil(t, reference.Saved)
	require.Equal(t, query.ExpressionOr, reference.Saved.Expression.Syntax.Kind)
	require.Equal(t, int64(7), reference.Saved.Query.Filters.SizeMin)
	require.Zero(t, resolved.Query.Filters.SizeMin)

	// Corrupt persisted authority must not be mistaken for an unknown operand.
	_, err = s.db.ExecContext(ctx, `UPDATE saved_queries SET fingerprint=? WHERE id=?`,
		"sha256:0000000000000000000000000000000000000000000000000000000000000000", saved.ID)
	require.NoError(t, err)
	_, err = query.ResolveQuery(ctx, input, r)
	require.Error(t, err)
	var positioned *query.ExpressionError
	require.NotErrorAs(t, err, &positioned)
	require.NotErrorIs(t, err, query.ErrUnknownReference)
}
