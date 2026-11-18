package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/pgstoretest"
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

func BenchmarkPostgresContentSearch(b *testing.B) {
	setup, owner := pgtest.NewDatabase(b)
	cfg := pgschema.Config{Schema: "documents", SchemaOwner: owner}
	connection := pgstoretest.RuntimeConfig(b, setup, cfg)
	s, err := OpenPostgres(b.Context(), connection, cfg, b.TempDir())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, s.Close()) })
	text := strings.Repeat("ordinary background prose ", 2048) + "needle retained evidence"
	for i := range 1000 {
		name := fmt.Sprintf("document-%04d.txt", i)
		node, err := s.CreateFile(b.Context(), s.RootID(), name, testSHA256([]byte(name)), int64(len(text)), "text/plain")
		require.NoError(b, err)
		require.NoError(b, s.RecordExtraction(b.Context(), ExtractionResult{BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: text}))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		hits, truncated, err := s.SearchExplainedLexicalCandidates(b.Context(), "needle", 10, SearchOptions{}, true)
		require.NoError(b, err)
		require.Len(b, hits, 10)
		require.True(b, truncated)
	}
}

func checkLexicalExcerptsSnapshotPostgres(t *testing.T) {
	t.Helper()
	t.Parallel()
	s := newPostgresQueryStore(t)
	ctx := t.Context()
	var alpha Node
	for _, name := range []string{"beta.txt", "alpha.txt"} {
		node, err := s.CreateFile(ctx, s.RootID(), name, testSHA256([]byte(name)), 3, "text/plain")
		require.NoError(t, err)
		text := "needle beta evidence"
		if name == "alpha.txt" {
			alpha = node
			require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "alternate", ExtractorVersion: 1, Status: ExtractionOK, Text: "unrelated prose"}))
			text = "needle old evidence"
		}
		require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: node.BlobHash, Extractor: "synthetic", ExtractorVersion: 1, Status: ExtractionOK, Text: text}))
	}
	conn, err := s.db.Conn(ctx)
	require.NoError(t, err)
	var cfg *pgx.ConnConfig
	err = conn.Raw(func(raw any) error {
		driver, ok := raw.(*stdlib.Conn)
		require.True(t, ok)
		cfg = driver.Conn().Config().Copy()
		return nil
	})
	require.NoError(t, conn.Close())
	require.NoError(t, err)
	var once sync.Once
	changed := false
	var writeErr error
	cfg.Tracer = renditionQueryStart(func(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) {
		if strings.Contains(data.SQL, "ts_headline(") {
			once.Do(func() {
				writeErr = s.RecordExtraction(ctx, ExtractionResult{BlobHash: alpha.BlobHash, Extractor: "synthetic", ExtractorVersion: 2, Status: ExtractionOK, Text: "needle replacement evidence"})
				changed = true
			})
		}
	})
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	reader := &Store{db: db, rootID: s.rootID, vaultID: s.vaultID, postgresSchema: s.postgresSchema}
	during, truncated, err := reader.SearchExplainedLexicalCandidates(ctx, "needle", 2, SearchOptions{}, true)
	require.NoError(t, err)
	require.NoError(t, writeErr)
	require.True(t, changed)
	require.False(t, truncated)
	require.Len(t, during, 2)
	require.Equal(t, "alpha.txt", during[0].Node.Name)
	require.Contains(t, during[0].Excerpt, "old evidence")
	require.Equal(t, "beta.txt", during[1].Node.Name)
	require.Contains(t, during[1].Excerpt, "beta evidence")
	after, _, err := s.SearchExplainedLexicalCandidates(ctx, "needle", 2, SearchOptions{}, true)
	require.NoError(t, err)
	require.Len(t, after, 2)
	require.Contains(t, after[0].Excerpt, "replacement evidence")
}
