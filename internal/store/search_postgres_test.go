package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgtest"
	"go.kenn.io/docbank/internal/store/pgschema"
)

func checkLexicalSearchIndexesPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	setup, owner := pgtest.NewDatabase(t)
	s := newPostgresSchemaStore(t, setup, pgschema.Config{Schema: "documents", SchemaOwner: owner})
	ctx := t.Context()
	// A populated corpus gives the planner a reason to choose the selective GIN
	// lookup. Use canonical DDL and normal runtime reads, without forcing a plan.
	for i := range 2048 {
		name, text := fmt.Sprintf("ordinary-reference-%d.txt", i), "ordinary reference text"
		if i == 2047 {
			name, text = "mércury-invoice.pdf", "mercury invoice"
		}
		node, err := s.CreateFile(ctx, s.RootID(), name, testSHA256([]byte(name)), 1, "text/plain")
		require.NoError(t, err)
		require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: text}))
	}
	_, err := setup.ExecContext(ctx, `ANALYZE documents.nodes; ANALYZE documents.extracted_text`)
	require.NoError(t, err)
	query, err := postgresNameSearch("mercury", SearchOptions{}, 10, false)
	require.NoError(t, err)
	rows, err := s.db.QueryContext(ctx, "EXPLAIN "+query.SQL, query.Args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan = append(plan, line)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Contains(t, strings.Join(plan, "\n"), "nodes_search")
	hits, overflow, err := s.SearchPage(ctx, "mercury", 10)
	require.NoError(t, err)
	require.False(t, overflow)
	require.Len(t, hits, 1)
	require.Equal(t, "/mércury-invoice.pdf", hits[0].Path)
	// Raw extraction also uses the installed expression index; no source text is
	// reconstructed from a token vector or changed by normalization.
	query, err = postgresContentSearch(`"mercury"`, "", SearchOptions{})
	require.NoError(t, err)
	rows, err = s.db.QueryContext(ctx, "EXPLAIN "+query.SQL, query.Args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	plan = nil
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan = append(plan, line)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Contains(t, strings.Join(plan, "\n"), "extracted_text_search")
}
