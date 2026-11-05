package store

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopedSearchPlansStartFromMatches(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "scope")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, scope.ID, "mercury.txt", fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	for i := range 8 {
		_, err = s.CreateFile(ctx, s.RootID(), "copy-"+strconv.Itoa(i)+".txt", file.BlobHash, 4, "text/plain")
		require.NoError(t, err)
	}
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: file.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "mercury"}))
	for _, published := range []bool{false, true} {
		if published {
			_, err = s.MigrateLegacyPlainText(ctx)
			require.NoError(t, err)
		}
		var generationID string
		require.NoError(t, s.withLexicalGenerationRead(ctx, func(_ metadataQuerier, generation LexicalGeneration) error {
			generationID = generation.ID
			return nil
		}))
		for _, explained := range []bool{false, true} {
			plans := scopedSearchQueryPlans(t, s, scope.ID, generationID, explained)
			for i, detail := range plans {
				assert.NotRegexp(t, `(?m)^SCAN n(?: |$)`, detail, "published=%v explained=%v query=%d", published, explained, i)
				assert.NotContains(t, detail, "nodes_parent")
				assert.Contains(t, detail, "nodes_live_children")
				if i == 1 && published {
					assert.Contains(t, detail, "rendition_attachments_build")
				}
			}
		}
	}
}

func TestScopedSearchResultsFollowTopology(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "scope")
	require.NoError(t, err)
	nested, err := s.Mkdir(ctx, scope.ID, "nested")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, scope.ID, "mercury.txt", fakeHash("a1"), 4, "text/plain")
	require.NoError(t, err)
	for i, parent := range []int64{nested.ID, nested.ID, s.RootID()} {
		_, err = s.CreateFile(ctx, parent, "copy-"+strconv.Itoa(i)+".txt", fakeHash("b2"), 4, "text/plain")
		require.NoError(t, err)
	}
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: fakeHash("b2"), Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "mercury"}))
	for _, published := range []bool{false, true} {
		if published {
			_, err = s.MigrateLegacyPlainText(ctx)
			require.NoError(t, err)
		}
		for _, state := range []string{"live", "trash", "restore", "restored-root"} {
			switch state {
			case "trash":
				_, _, err = s.Trash(ctx, nested.ID, UnconditionalRev)
			case "restore":
				_, _, err = s.Restore(ctx, nested.ID, UnconditionalRev)
			case "restored-root":
				_, _, err = s.Trash(ctx, scope.ID, UnconditionalRev)
				require.NoError(t, err)
				_, _, err = s.Restore(ctx, scope.ID, UnconditionalRev)
			}
			require.NoError(t, err)
			all, _, err := s.SearchPage(ctx, "mercury", 50)
			require.NoError(t, err)
			var want []SearchHit
			for _, hit := range all {
				if strings.HasPrefix(hit.Path, "/scope/") {
					want = append(want, hit)
				}
			}
			opts := SearchOptions{UnderNodeID: scope.ID}
			hits, more, err := s.SearchPageWithOptions(ctx, "mercury", 50, opts)
			require.NoError(t, err)
			assert.False(t, more)
			assert.Equal(t, want, hits, "published=%v state=%s", published, state)
			explained, more, err := s.SearchExplainedLexicalCandidates(ctx, "mercury", 50, opts, false)
			require.NoError(t, err)
			assert.False(t, more)
			require.Len(t, explained, len(want))
			for i, hit := range explained {
				assert.Equal(t, want[i].Node.ID, hit.Node.ID)
				assert.Equal(t, want[i].Match, hit.Match)
			}
		}
	}
}

func TestRenditionAttachmentBuildIndexAddedOnReopen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.db.ExecContext(t.Context(), `DROP INDEX rendition_attachments_build`)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	reopened, err := Open(t.Context(), s.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	detail := explainQueryPlan(t, reopened, `SELECT attachment_id FROM rendition_attachments WHERE build_id=?`, catalogBuildID)
	assert.Contains(t, detail, "rendition_attachments_build")
}

func TestPublishedSearchExcludesUnprocessedCopies(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	file, err := s.CreateFile(ctx, s.RootID(), "inside.txt", fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: file.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "mercury"}))
	_, err = s.MigrateLegacyPlainText(ctx)
	require.NoError(t, err)
	// Copies imported after publication have no rendition attachment.
	for i := range 8 {
		_, err = s.CreateFile(ctx, s.RootID(), "copy-"+strconv.Itoa(i)+".txt", file.BlobHash, 4, "text/plain")
		require.NoError(t, err)
	}
	hits, more, err := s.SearchPage(ctx, "mercury", 50)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, hits, 1)
	assert.Equal(t, file.ID, hits[0].Node.ID)
	explained, more, err := s.SearchExplainedLexicalCandidates(ctx, "mercury", 50, SearchOptions{}, false)
	require.NoError(t, err)
	assert.False(t, more)
	require.Len(t, explained, 1)
	assert.Equal(t, file.ID, explained[0].Node.ID)
}

func scopedSearchQueryPlans(t *testing.T, s *Store, scopeID int64, generationID string, explained bool) []string {
	t.Helper()
	var queries []string
	func() {
		readDB := s.db
		release := make(chan struct{})
		close(release)
		s.db = sql.OpenDB(photoSearchBeginConnector{db: readDB, start: func() {}, release: release, query: func(query string) {
			if strings.Contains(query, "WITH name_matches AS") || strings.Contains(query, "content_fts MATCH") || strings.Contains(query, "rendition_lexical_fts MATCH") {
				queries = append(queries, query)
			}
		}})
		defer func() {
			require.NoError(t, s.db.Close())
			s.db = readDB
		}()
		opts := SearchOptions{UnderNodeID: scopeID}
		if explained {
			_, _, err := s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 50, opts, false)
			require.NoError(t, err)
		} else {
			_, _, err := s.SearchPageWithOptions(t.Context(), "mercury", 50, opts)
			require.NoError(t, err)
		}
	}()
	require.Len(t, queries, 2)
	plans := make([]string, len(queries))
	for i, query := range queries {
		args := []any{ftsQuery("mercury")}
		if i == 0 {
			names, err := nameSearchCandidates(ftsQuery("mercury"))
			require.NoError(t, err)
			args = names.Args
		} else if generationID != "" {
			args = append(args, generationID)
		}
		if scopeID != 0 {
			args = append(args, scopeID)
		}
		if i == 0 || !explained {
			args = append(args, 51)
		}
		plans[i] = explainQueryPlan(t, s, query, args...)
	}
	return plans
}
