package api_test

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestIngestLabelReceiptNamesCommittedRun(t *testing.T) {
	ts, s := newTestServer(t, nil)
	source := filepath.Join(t.TempDir(), "note.txt")
	require.NoError(t, os.WriteFile(source, []byte("synthetic"), 0o600))

	report := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{source}, "dest": "/inbox", "collection_label": "Review set",
	})
	require.Equal(t, 1, report.Added)
	require.NotEmpty(t, report.IngestID)

	collection, err := s.CollectionByID(t.Context(), report.IngestID)
	require.NoError(t, err)
	require.NotNil(t, collection.Label)
	assert.Equal(t, "Review set", *collection.Label)
	assert.Equal(t, int64(1), collection.FileCount)
	page, err := s.CollectionMembers(t.Context(), report.IngestID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "/inbox/note.txt", page.Items[0].Path)
}

func TestIngestReceiptRecordsRepeatedAndReplacementMembership(t *testing.T) {
	ts, s := newTestServer(t, nil)
	source := filepath.Join(t.TempDir(), "record.txt")
	require.NoError(t, os.WriteFile(source, []byte("first"), 0o600))

	first := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{source}, "dest": "/inbox", "collection_label": "First pass",
	})
	require.NotEmpty(t, first.IngestID)
	before, err := s.NodeByPath(t.Context(), "/inbox/record.txt")
	require.NoError(t, err)

	repeated := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{source}, "dest": "/inbox", "collection_label": "Second pass",
	})
	assert.Zero(t, repeated.Added)
	assert.Equal(t, 1, repeated.Skipped)
	require.NotEmpty(t, repeated.IngestID)
	assert.NotEqual(t, first.IngestID, repeated.IngestID)
	repeatedCollection, err := s.CollectionByID(t.Context(), repeated.IngestID)
	require.NoError(t, err)
	require.NotNil(t, repeatedCollection.Label)
	assert.Equal(t, "Second pass", *repeatedCollection.Label)
	repeatedMembers, err := s.CollectionMembers(t.Context(), repeated.IngestID, 10, 0)
	require.NoError(t, err)
	require.Len(t, repeatedMembers.Items, 1)
	assert.Equal(t, before.ID, repeatedMembers.Items[0].Node.ID)
	afterRepeated, err := s.NodeByID(t.Context(), before.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Revision+1, afterRepeated.Revision)

	require.NoError(t, os.WriteFile(source, []byte("replacement"), 0o600))
	replacement := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{source}, "dest": "/inbox", "replace": true,
		"collection_label": "Replacement pass",
	})
	assert.Equal(t, 1, replacement.Added)
	require.NotEmpty(t, replacement.IngestID)
	replacementCollection, err := s.CollectionByID(t.Context(), replacement.IngestID)
	require.NoError(t, err)
	require.NotNil(t, replacementCollection.Label)
	assert.Equal(t, "Replacement pass", *replacementCollection.Label)
	replacementMembers, err := s.CollectionMembers(t.Context(), replacement.IngestID, 10, 0)
	require.NoError(t, err)
	require.Len(t, replacementMembers.Items, 1)
	assert.Equal(t, before.ID, replacementMembers.Items[0].Node.ID)
}

func TestIngestRepeatedPathWithinRunKeepsOneObservation(t *testing.T) {
	ts, s := newTestServer(t, nil)
	source := filepath.Join(t.TempDir(), "repeated.txt")
	require.NoError(t, os.WriteFile(source, []byte("one observation"), 0o600))

	report := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{source, source}, "dest": "/inbox",
		"collection_label": "One run",
	})
	assert.Equal(t, 1, report.Added)
	assert.Equal(t, 1, report.Skipped)
	require.NotEmpty(t, report.IngestID)
	collection, err := s.CollectionByID(t.Context(), report.IngestID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), collection.FileCount)
	node, err := s.NodeByPath(t.Context(), "/inbox/repeated.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(1), node.Revision)
}

func TestIngestLabelCollisionRollsBackRunAndDocument(t *testing.T) {
	ts, s := newTestServer(t, nil)
	dir := t.TempDir()
	firstSource := filepath.Join(dir, "first.txt")
	conflictSource := filepath.Join(dir, "conflict.txt")
	require.NoError(t, os.WriteFile(firstSource, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(conflictSource, []byte("conflict"), 0o600))

	first := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{firstSource}, "dest": "/inbox", "collection_label": "Claimed",
	})
	require.NotEmpty(t, first.IngestID)
	beforeIngests, beforeLabels := ingestAuthorityCounts(t, s)

	conflict := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{conflictSource}, "dest": "/inbox", "collection_label": "Claimed",
	})
	assert.Empty(t, conflict.IngestID)
	assert.Zero(t, conflict.Added)
	require.Len(t, conflict.Failed, 1)
	assert.Contains(t, conflict.Failed[0].Error, "already exists")
	_, err := s.NodeByPath(t.Context(), "/inbox/conflict.txt")
	require.ErrorIs(t, err, store.ErrNotFound)
	afterIngests, afterLabels := ingestAuthorityCounts(t, s)
	assert.Equal(t, beforeIngests, afterIngests)
	assert.Equal(t, beforeLabels, afterLabels)
}

func TestIngestReceiptsRequireCommittedDocumentAuthority(t *testing.T) {
	ts, s := newTestServer(t, nil)
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.txt")
	missing := filepath.Join(dir, "missing.txt")
	require.NoError(t, os.WriteFile(valid, []byte("valid"), 0o600))

	partial := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{missing, valid}, "dest": "/inbox", "collection_label": "Partial set",
	})
	assert.Equal(t, 1, partial.Added)
	require.Len(t, partial.Failed, 1)
	require.NotEmpty(t, partial.IngestID)
	partialCollection, err := s.CollectionByID(t.Context(), partial.IngestID)
	require.NoError(t, err)
	require.NotNil(t, partialCollection.Label)
	assert.Equal(t, "Partial set", *partialCollection.Label)
	assert.Equal(t, int64(1), partialCollection.FileCount)

	beforeIngests, beforeLabels := ingestAuthorityCounts(t, s)
	excluded := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{valid}, "dest": "/inbox", "include": []string{"*.md"},
		"collection_label": "Empty set",
	})
	assert.Empty(t, excluded.IngestID)
	assert.Equal(t, 1, excluded.Excluded)
	assert.Empty(t, excluded.Failed)
	afterIngests, afterLabels := ingestAuthorityCounts(t, s)
	assert.Equal(t, beforeIngests, afterIngests)
	assert.Equal(t, beforeLabels, afterLabels)

	missingOnly := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
		"paths": []string{missing}, "dest": "/inbox", "collection_label": "Rejected first",
	})
	assert.Empty(t, missingOnly.IngestID)
	require.Len(t, missingOnly.Failed, 1)
	afterIngests, afterLabels = ingestAuthorityCounts(t, s)
	assert.Equal(t, beforeIngests, afterIngests)
	assert.Equal(t, beforeLabels, afterLabels)
}

func TestIngestStreamTerminalReceiptNamesPartialCommittedRun(t *testing.T) {
	ts, s := newTestServer(t, nil)
	dir := t.TempDir()
	valid := filepath.Join(dir, "stream.txt")
	missing := filepath.Join(dir, "missing.txt")
	require.NoError(t, os.WriteFile(valid, []byte("stream"), 0o600))

	report := requestIngestCollection(t, ts, "/api/v1/ingest/stream", map[string]any{
		"paths": []string{missing, valid}, "dest": "/inbox", "collection_label": "Stream set",
	})
	assert.Equal(t, 1, report.Added)
	require.Len(t, report.Failed, 1)
	require.NotEmpty(t, report.IngestID)
	collection, err := s.CollectionByID(t.Context(), report.IngestID)
	require.NoError(t, err)
	require.NotNil(t, collection.Label)
	assert.Equal(t, "Stream set", *collection.Label)
	assert.Equal(t, int64(1), collection.FileCount)
}

func TestIngestRejectsInvalidLabelWithoutAuthority(t *testing.T) {
	ts, s := newTestServer(t, nil)
	source := filepath.Join(t.TempDir(), "invalid.txt")
	require.NoError(t, os.WriteFile(source, []byte("invalid label"), 0o600))
	beforeIngests, beforeLabels := ingestAuthorityCounts(t, s)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest", nil, map[string]any{
		"paths": []string{source}, "dest": "/new/inbox", "collection_label": "   ",
	})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, body)
	assert.Contains(t, body, `"code":"invalid_collection_label"`)
	assert.Contains(t, body, "all whitespace")
	afterIngests, afterLabels := ingestAuthorityCounts(t, s)
	assert.Equal(t, beforeIngests, afterIngests)
	assert.Equal(t, beforeLabels, afterLabels)
	_, err := s.NodeByPath(t.Context(), "/new")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestIngestStreamRejectsInvalidLabelWithoutAuthority(t *testing.T) {
	ts, s := newTestServer(t, nil)
	source := filepath.Join(t.TempDir(), "invalid.txt")
	require.NoError(t, os.WriteFile(source, []byte("invalid label"), 0o600))
	beforeIngests, beforeLabels := ingestAuthorityCounts(t, s)

	resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest/stream", nil, map[string]any{
		"paths": []string{source}, "dest": "/new/inbox", "collection_label": "   ",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	require.NotEmpty(t, lines)
	var terminal api.IngestEvent
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &terminal))
	assert.Equal(t, "error", terminal.Type)
	require.NotNil(t, terminal.Error)
	assert.Equal(t, "invalid_collection_label", terminal.Error.Code)
	assert.Contains(t, terminal.Error.Detail, "all whitespace")
	afterIngests, afterLabels := ingestAuthorityCounts(t, s)
	assert.Equal(t, beforeIngests, afterIngests)
	assert.Equal(t, beforeLabels, afterLabels)
	_, err := s.NodeByPath(t.Context(), "/new")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestIngestInitialLabelAuditRejectionHasNoReceiptOrAuthority(t *testing.T) {
	for _, route := range []string{"/api/v1/ingest", "/api/v1/ingest/stream"} {
		t.Run(route, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "api", nil)
			require.NoError(t, err)
			_, err = s.EnableInitialAudit(t.Context(), plan)
			require.NoError(t, err)
			source := filepath.Join(t.TempDir(), "audited-tree")
			require.NoError(t, os.MkdirAll(filepath.Join(source, "sub"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(source, "sub", "audited.txt"), []byte("audited"), 0o600))
			beforeIngests, beforeLabels := ingestAuthorityCounts(t, s)

			report := requestIngestCollection(t, ts, route, map[string]any{
				"paths": []string{source}, "dest": "/new/inbox", "collection_label": "Audited set",
			})
			assert.Empty(t, report.IngestID)
			assert.Zero(t, report.Added)
			require.Len(t, report.Failed, 1)
			assert.Contains(t, report.Failed[0].Error, store.ErrAuditMutationUnsupported.Error())
			_, err = s.NodeByPath(t.Context(), "/new")
			require.ErrorIs(t, err, store.ErrNotFound)
			afterIngests, afterLabels := ingestAuthorityCounts(t, s)
			assert.Equal(t, beforeIngests, afterIngests)
			assert.Equal(t, beforeLabels, afterLabels)
		})
	}
}

func TestIngestLabelCollisionLeavesMissingDestinationAndSourceTreeAbsent(t *testing.T) {
	for _, route := range []string{"/api/v1/ingest", "/api/v1/ingest/stream"} {
		t.Run(route, func(t *testing.T) {
			ts, s := newTestServer(t, nil)
			winnerSource := filepath.Join(t.TempDir(), "winner.txt")
			require.NoError(t, os.WriteFile(winnerSource, []byte("winner"), 0o600))
			winner := requestIngestCollection(t, ts, "/api/v1/ingest", map[string]any{
				"paths": []string{winnerSource}, "dest": "/existing", "collection_label": "Claimed",
			})
			require.NotEmpty(t, winner.IngestID)

			source := filepath.Join(t.TempDir(), "conflict-tree")
			require.NoError(t, os.MkdirAll(filepath.Join(source, "sub"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(source, "sub", "note.txt"), []byte("conflict"), 0o600))
			beforeRoot, err := s.NodeByID(t.Context(), s.RootID())
			require.NoError(t, err)
			beforeIngests, beforeLabels := ingestAuthorityCounts(t, s)

			report := requestIngestCollection(t, ts, route, map[string]any{
				"paths": []string{source}, "dest": "/new/inbox", "collection_label": "Claimed",
			})
			assert.Empty(t, report.IngestID)
			assert.Zero(t, report.Added)
			require.Len(t, report.Failed, 1)
			assert.Contains(t, report.Failed[0].Error, "already exists")
			_, err = s.NodeByPath(t.Context(), "/new")
			require.ErrorIs(t, err, store.ErrNotFound)
			afterRoot, err := s.NodeByID(t.Context(), s.RootID())
			require.NoError(t, err)
			assert.Equal(t, beforeRoot, afterRoot)
			afterIngests, afterLabels := ingestAuthorityCounts(t, s)
			assert.Equal(t, beforeIngests, afterIngests)
			assert.Equal(t, beforeLabels, afterLabels)
		})
	}
}

func requestIngestCollection(
	t *testing.T, ts *httptest.Server, route string, request map[string]any,
) api.IngestReport {
	t.Helper()
	resp, body := do(t, ts, http.MethodPost, route, nil, request)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	if route == "/api/v1/ingest/stream" {
		lines := strings.Split(strings.TrimSpace(body), "\n")
		require.NotEmpty(t, lines)
		var terminal struct {
			Type   string            `json:"type"`
			Report *api.IngestReport `json:"report"`
		}
		require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &terminal))
		require.Equal(t, "result", terminal.Type)
		require.NotNil(t, terminal.Report)
		return *terminal.Report
	}
	var report api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	return report
}

func ingestAuthorityCounts(t *testing.T, s *testStore) (ingests, labels int) {
	t.Helper()
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	for line := range strings.SplitSeq(strings.TrimSpace(exported.String()), "\n") {
		var record struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &record), line)
		switch record.Type {
		case "ingest":
			ingests++
		case "collection_label":
			labels++
		}
	}
	return ingests, labels
}
