package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngestFileIdempotency(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "/src/docs")
	require.NoError(t, err)

	// First import.
	n1, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 10, "application/pdf", "/src/docs/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report.pdf", n1.Name)

	// Same content, same name: skipped.
	_, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 10, "application/pdf", "/src/docs/report.pdf", "")
	require.NoError(t, err)
	assert.False(t, added)

	// Different content, same name: suffixed.
	n2, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("b2"), 11, "application/pdf", "/other/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (2).pdf", n2.Name)

	// Re-run of the suffixed one: skipped even though names differ.
	_, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("b2"), 11, "application/pdf", "/other/report.pdf", "")
	require.NoError(t, err)
	assert.False(t, added)

	// Third distinct content takes the next free ordinal.
	n3, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("c3"), 12, "application/pdf", "/more/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (3).pdf", n3.Name)
}

func TestIngestFileMatchesAcrossSuffixGap(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)

	// Occupy report.pdf and report (3).pdf; (2) is free (e.g. it was trashed).
	_, _, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "p1", "")
	require.NoError(t, err)
	f2, err := s.CreateFile(ctx, s.RootID(), "report (3).pdf", fakeHash("c3"), 1, "application/pdf")
	require.NoError(t, err)
	_ = f2

	// Content matching the gapped candidate must still be skipped: the
	// resolver checks ALL live candidates, not just up to the first gap.
	_, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("c3"), 1, "application/pdf", "p2", "")
	require.NoError(t, err)
	assert.False(t, added)

	// New content fills the gap.
	n, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("d4"), 1, "application/pdf", "p3", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (2).pdf", n.Name)
}

func TestIngestFileRecordsProvenance(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)
	n, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"a.txt", fakeHash("a1"), 1, "text/plain", "/orig/a.txt", "2026-01-02T03:04:05Z")
	require.NoError(t, err)
	require.True(t, added)

	var origPath, origMtime string
	require.NoError(t, s.db.QueryRow(
		`SELECT original_path, original_mtime FROM provenance WHERE node_id = ?`,
		n.ID).Scan(&origPath, &origMtime))
	assert.Equal(t, "/orig/a.txt", origPath)
	assert.Equal(t, "2026-01-02T03:04:05Z", origMtime)
}

func TestIngestFileDistinctSourceNamedLikeSuffix(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	ing, err := s.BeginIngest(ctx, "cli", "test")
	require.NoError(t, err)

	// A real source file named like an auto-suffixed copy imports first...
	n1, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report (2).pdf", fakeHash("a1"), 1, "application/pdf", "/src/report (2).pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report (2).pdf", n1.Name)

	// ...and an identical-content report.pdf is a distinct source file,
	// not a re-import: it must land under its own name.
	n2, added, err := s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "/src/report.pdf", "")
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "report.pdf", n2.Name)

	// Re-running both converges: each skips against its own prior import.
	_, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report (2).pdf", fakeHash("a1"), 1, "application/pdf", "/src/report (2).pdf", "")
	require.NoError(t, err)
	assert.False(t, added)
	_, added, err = s.IngestFile(ctx, ing, s.RootID(),
		"report.pdf", fakeHash("a1"), 1, "application/pdf", "/src/report.pdf", "")
	require.NoError(t, err)
	assert.False(t, added)
}
