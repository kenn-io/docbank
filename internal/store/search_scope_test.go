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
			var queries []string
			func() {
				readDB := s.db
				release := make(chan struct{})
				close(release)
				s.db = sql.OpenDB(photoSearchBeginConnector{db: readDB, start: func() {}, release: release, query: func(query string) {
					if strings.Contains(query, "WITH RECURSIVE directories") {
						queries = append(queries, query)
					}
				}})
				defer func() {
					require.NoError(t, s.db.Close())
					s.db = readDB
				}()
				opts := SearchOptions{UnderNodeID: scope.ID}
				if explained {
					_, _, err = s.SearchExplainedLexicalCandidates(ctx, "mercury", 50, opts, false)
				} else {
					_, _, err = s.SearchPageWithOptions(ctx, "mercury", 50, opts)
				}
				require.NoError(t, err)
			}()
			require.Len(t, queries, 2)
			for i, query := range queries {
				args := []any{ftsQuery("mercury")}
				if i == 0 {
					names, err := nameSearchCandidates(ftsQuery("mercury"))
					require.NoError(t, err)
					args = names.Args
				}
				if i == 1 && published {
					args = append(args, generationID)
				}
				args = append(args, scope.ID)
				if i == 0 || !explained {
					args = append(args, 51)
				}
				detail := explainQueryPlan(t, s, query, args...)
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
		for _, state := range []string{"live", "trash", "restore", "outside", "inside", "trashed-root", "restored-root"} {
			switch state {
			case "trash":
				_, _, err = s.Trash(ctx, nested.ID, UnconditionalRev)
			case "restore":
				_, _, err = s.Restore(ctx, nested.ID, UnconditionalRev)
			case "outside":
				_, _, err = s.Move(ctx, nested.ID, s.RootID(), "nested", UnconditionalRev)
			case "inside":
				_, _, err = s.Move(ctx, nested.ID, scope.ID, "nested", UnconditionalRev)
			case "trashed-root":
				_, _, err = s.Trash(ctx, scope.ID, UnconditionalRev)
			case "restored-root":
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
			if state == "trashed-root" {
				require.Empty(t, want)
				require.ErrorIs(t, err, ErrNotFound)
				_, _, err = s.SearchExplainedLexicalCandidates(ctx, "mercury", 50, opts, false)
				require.ErrorIs(t, err, ErrNotFound)
				continue
			}
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

func TestScopedCopiedBlobUsesSmallFolderPlan(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	scope, err := s.Mkdir(ctx, s.RootID(), "scope")
	require.NoError(t, err)
	file, err := s.CreateFile(ctx, scope.ID, "inside.txt", fakeHash("b2"), 4, "text/plain")
	require.NoError(t, err)
	for i := range 8 {
		_, err = s.CreateFile(ctx, s.RootID(), "copy-"+strconv.Itoa(i)+".txt", file.BlobHash, 4, "text/plain")
		require.NoError(t, err)
	}
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{BlobHash: file.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: ExtractionOK, Text: "mercury"}))
	detail := scopedContentQueryPlan(t, s, scope.ID, "", false)
	// A one-file scope must probe its current version, rather than expand every copy of the matching blob.
	assert.NotContains(t, detail, "content_versions_blob")
}

func scopedContentQueryPlan(t *testing.T, s *Store, scopeID int64, generationID string, explained bool) string {
	t.Helper()
	var query string
	func() {
		readDB := s.db
		release := make(chan struct{})
		close(release)
		s.db = sql.OpenDB(photoSearchBeginConnector{db: readDB, start: func() {}, release: release, query: func(sql string) {
			if strings.Contains(sql, "content_fts MATCH") || strings.Contains(sql, "rendition_lexical_fts MATCH") {
				query = sql
			}
		}})
		defer func() {
			require.NoError(t, s.db.Close())
			s.db = readDB
		}()
		opts := SearchOptions{UnderNodeID: scopeID}
		if explained {
			hits, more, err := s.SearchExplainedLexicalCandidates(t.Context(), "mercury", 50, opts, false)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, "inside.txt", hits[0].Node.Name)
			assert.False(t, more)
		} else {
			hits, more, err := s.SearchPageWithOptions(t.Context(), "mercury", 50, opts)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, "inside.txt", hits[0].Node.Name)
			assert.False(t, more)
		}
	}()
	require.NotEmpty(t, query)
	args := []any{ftsQuery("mercury")}
	if generationID != "" {
		args = append(args, generationID)
	}
	if scopeID != 0 {
		args = append(args, scopeID)
	}
	if !explained {
		args = append(args, 51)
	}
	return explainQueryPlan(t, s, query, args...)
}
