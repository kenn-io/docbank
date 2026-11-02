package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// testApp is the App collaborator the in-package engine tests capture and
// restore through. It models a minimal, application-neutral schema: a plain
// "notes" table plus a "blobs" table that references content-addressed files,
// so the round-trip and verify tests exercise a realistic App without binding
// the engine to any particular application.
//
// This double exists only because these tests live in package backup (they
// reach unexported engine internals); a production App implementation lives in
// the application that embeds this engine.
type testApp struct{ version string }

var _ App = (*testApp)(nil)

// newTestApp returns the App the engine tests pass to Create/Restore/Verify.
func newTestApp() App { return &testApp{version: "test"} }

// testPackExt is the pack file extension these tests use, both through
// testApp and at call sites that build a PackAppender or read a blob
// directly without an App in hand.
const testPackExt = ".kpack"

func (a *testApp) FrozenView(s *FrozenSession) FrozenView { return &testFrozenView{tx: s.Tx()} }

func (a *testApp) DBFileName() string        { return "app.db" }
func (a *testApp) ContentDirName() string    { return "content" }
func (a *testApp) PackFileExtension() string { return testPackExt }
func (a *testApp) Version() string           { return a.version }

func (a *testApp) ExcludedPaths() []string {
	return []string{"scratch.db", "cache/", "logs/", "imports/", "tmp/", "locks"}
}

// testContentBearing / testPreviewBearing select blob rows whose bytes live in
// the local content tree; only genuine URL schemes are excluded.
const testContentBearing = `content_hash IS NOT NULL AND content_hash != ''
	AND storage_path IS NOT NULL AND storage_path != ''
	AND storage_path NOT LIKE 'http://%'
	AND storage_path NOT LIKE 'https://%'`

const testPreviewBearing = `preview_hash IS NOT NULL AND preview_hash != ''
	AND preview_path IS NOT NULL AND preview_path != ''
	AND preview_path NOT LIKE 'http://%'
	AND preview_path NOT LIKE 'https://%'`

const testBlobFilesQuery = `SELECT COUNT(*) FROM (
	SELECT content_hash AS h FROM blobs WHERE ` + testContentBearing + `
	UNION
	SELECT preview_hash AS h FROM blobs WHERE ` + testPreviewBearing + `
)`

// testStats is testApp's opaque, application-defined stats payload.
type testStats struct {
	Notes     int64     `json:"notes"`
	BlobRows  int64     `json:"blob_rows"`
	BlobFiles int64     `json:"blob_files"`
	DateRange [2]string `json:"date_range"`
}

type testRowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func computeTestStats(ctx context.Context, q testRowQuerier) (testStats, error) {
	var st testStats
	counts := []struct {
		dst   *int64
		query string
	}{
		{&st.Notes, "SELECT COUNT(*) FROM notes"},
		{&st.BlobRows, "SELECT COUNT(*) FROM blobs"},
		{&st.BlobFiles, testBlobFilesQuery},
	}
	for _, c := range counts {
		if err := q.QueryRowContext(ctx, c.query).Scan(c.dst); err != nil {
			return st, fmt.Errorf("testapp: stats query %q: %w", c.query, err)
		}
	}
	err := q.QueryRowContext(ctx,
		"SELECT COALESCE(MIN(created_at),''), COALESCE(MAX(created_at),'') FROM notes",
	).Scan(&st.DateRange[0], &st.DateRange[1])
	if err != nil {
		return st, fmt.Errorf("testapp: date range query: %w", err)
	}
	return st, nil
}

func (a *testApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	st, err := computeTestStats(ctx, db)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

func (a *testApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT content_hash, storage_path FROM blobs WHERE "+testContentBearing+
			" UNION SELECT preview_hash, preview_path FROM blobs WHERE "+testPreviewBearing)
	if err != nil {
		return nil, fmt.Errorf("testapp: content path query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	paths := map[string][]string{}
	for rows.Next() {
		var hash, p string
		if err := rows.Scan(&hash, &p); err != nil {
			return nil, fmt.Errorf("testapp: scanning content path: %w", err)
		}
		rel := filepath.FromSlash(p)
		if !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("testapp: blob %s storage path %q escapes the content directory", hash, p)
		}
		paths[hash] = append(paths[hash], rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("testapp: content path rows: %w", err)
	}
	return paths, nil
}

func (a *testApp) CheckManifest(m *Manifest) []string {
	st, err := parseTestStats(m.Stats)
	if err != nil {
		return []string{fmt.Sprintf("manifest stats unreadable: %v", err)}
	}
	if st.BlobFiles != m.Attachments.Blobs {
		return []string{fmt.Sprintf(
			"stats.blob_files %d != attachments.blobs %d", st.BlobFiles, m.Attachments.Blobs)}
	}
	return nil
}

// testFrozenView answers Create's schema questions against the pinned tx.
type testFrozenView struct{ tx *sql.Tx }

func (v *testFrozenView) Stats(ctx context.Context) (json.RawMessage, error) {
	st, err := computeTestStats(ctx, v.tx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

func (v *testFrozenView) ContentInfo(ctx context.Context) (*ContentInfo, error) {
	rows, err := v.tx.QueryContext(ctx,
		"SELECT content_hash, COALESCE(MAX(size), -1), MIN(storage_path) FROM blobs WHERE "+testContentBearing+
			" GROUP BY content_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("testapp: content locator query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []ContentRef
	seen := map[string]bool{}
	for rows.Next() {
		var ref ContentRef
		if err := rows.Scan(&ref.Hash, &ref.Size, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("testapp: scanning content locator: %w", err)
		}
		refs = append(refs, ref)
		seen[ref.Hash] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("testapp: content locator rows: %w", err)
	}

	previewRows, err := v.tx.QueryContext(ctx,
		"SELECT preview_hash, MIN(preview_path) FROM blobs WHERE "+testPreviewBearing+
			" GROUP BY preview_hash ORDER BY MIN(id)")
	if err != nil {
		return nil, fmt.Errorf("testapp: preview locator query: %w", err)
	}
	defer func() { _ = previewRows.Close() }()
	for previewRows.Next() {
		var ref ContentRef
		if err := previewRows.Scan(&ref.Hash, &ref.StoragePath); err != nil {
			return nil, fmt.Errorf("testapp: scanning preview locator: %w", err)
		}
		if !seen[ref.Hash] {
			ref.Size = -1
			refs = append(refs, ref)
			seen[ref.Hash] = true
		}
	}
	if err := previewRows.Err(); err != nil {
		return nil, fmt.Errorf("testapp: preview locator rows: %w", err)
	}

	var rowCount int64
	if err := v.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM blobs").Scan(&rowCount); err != nil {
		return nil, fmt.Errorf("testapp: blob row count query: %w", err)
	}

	var nonCanonical bool
	err = v.tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM blobs WHERE `+testContentBearing+`
		  AND storage_path != substr(content_hash, 1, 2) || '/' || content_hash
		UNION ALL
		SELECT 1 FROM blobs WHERE `+testPreviewBearing+`
		  AND preview_path != substr(preview_hash, 1, 2) || '/' || preview_hash
	)`).Scan(&nonCanonical)
	if err != nil {
		return nil, fmt.Errorf("testapp: content path canonicality query: %w", err)
	}

	return &ContentInfo{Refs: refs, Rows: rowCount, NonCanonicalPaths: nonCanonical}, nil
}

func parseTestStats(raw json.RawMessage) (testStats, error) {
	var st testStats
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("testapp: parsing manifest stats: %w", err)
	}
	return st, nil
}

// mustParseStats decodes a manifest's stats payload for assertions.
func mustParseStats(t *testing.T, raw json.RawMessage) testStats {
	t.Helper()
	st, err := parseTestStats(raw)
	require.NoError(t, err)
	return st
}
