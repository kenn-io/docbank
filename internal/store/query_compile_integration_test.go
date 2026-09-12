package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/query"
)

func compileFixtureQuery(t *testing.T, s *Store, text string, filters query.Filters) CompiledQuery {
	t.Helper()
	value, err := query.Parse([]byte(`{"syntax":"advanced"}`))
	require.NoError(t, err)
	value.Text, value.Filters = text, filters
	compiled, err := compileQuery(t.Context(), value, queryResolver{q: s.db})
	require.NoError(t, err)
	return compiled
}

func compiledFixtureIDs(t *testing.T, q metadataQuerier, compiled CompiledQuery, generation string) []int64 {
	t.Helper()
	predicate, args, err := compiled.Bind(generation)
	require.NoError(t, err)
	rows, err := q.QueryContext(t.Context(), `SELECT n.id FROM `+nodeFrom+` WHERE `+predicate+` ORDER BY n.id`, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	ids := []int64{}
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// Hoisting saved facets or flattening a mixed-field OR changes these members.
func TestCompiledQuerySQLiteReferenceAndFacetScopes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	alpha, err := s.CreateFile(ctx, s.RootID(), "alpha.txt", fakeHash("a1"), 10, "text/plain")
	require.NoError(t, err)
	beta, err := s.CreateFile(ctx, s.RootID(), "beta.pdf", fakeHash("b2"), 20, "application/pdf")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "gamma.txt", fakeHash("c3"), 5, "text/plain")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "urgent")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, beta.ID, beta.Revision)
	require.NoError(t, err)
	_, err = s.CreateSavedQuery(ctx, "Choice", "", SavedQueryKindQuery,
		[]byte(`{"syntax":"advanced","text":"name:(alpha OR beta)","filters":{"size_max":15}}`))
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `saved:Choice OR tag:urgent`, query.Filters{MIMETypes: []string{"application/pdf"}})
	require.Equal(t, []int64{beta.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, `saved:Choice OR tag:urgent`, query.Filters{})
	require.Equal(t, []int64{alpha.ID, beta.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, `saved:Choice`, query.Filters{SizeMin: 11})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, `saved:Choice`, query.Filters{})
	require.Equal(t, []int64{alpha.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, `name:alpha OR NOT tag:urgent`, query.Filters{})
	require.Len(t, compiledFixtureIDs(t, s.db, compiled, ""), 2)
}

func TestCompiledQuerySQLiteCollectionUnionDeduplicatesMembership(t *testing.T) {
	s := newTestStore(t)
	first := createCollectionRun(t, s, "first.txt", "a1")
	second := createCollectionRun(t, s, "second.txt", "b2")
	node, err := s.NodeByPath(t.Context(), "/first.txt")
	require.NoError(t, err)
	addCollectionMembership(t, s, second, node.ID, "/synthetic/other/first.txt", nil)
	addCollectionMembership(t, s, first, node.ID, "/synthetic/duplicate/first.txt", nil)
	_, err = s.SetCollectionLabel(t.Context(), first.ID(), 1, new("First"))
	require.NoError(t, err)
	_, err = s.SetCollectionLabel(t.Context(), second.ID(), 1, new("Second"))
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `collection:(First OR Second)`, query.Filters{})
	require.Len(t, compiledFixtureIDs(t, s.db, compiled, ""), 2)
	compiled = compileFixtureQuery(t, s, `collection:First`, query.Filters{ExcludeCollectionIDs: []string{second.ID()}})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, `collection:First`, query.Filters{})
	require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
}

// Match paths at a separator boundary and compare fractional times exactly.
func TestCompiledQuerySQLitePathMediaAndTime(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	dir, err := s.Mkdir(ctx, s.RootID(), "case_1")
	require.NoError(t, err)
	otherDir, err := s.Mkdir(ctx, s.RootID(), "caseX1")
	require.NoError(t, err)
	inside, err := s.CreateFile(ctx, dir.ID, "REPORT.PDF", fakeHash("d4"), 10, "application/octet-stream")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, otherDir.ID, "REPORT.PDF", fakeHash("e5"), 10, "application/pdf")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, "2026-01-02T03:04:05.000000001Z", inside.ID)
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `path:/case_1 AND media_family:document AND extension:pdf`, query.Filters{
		ModifiedAfter: "2026-01-02T04:04:05+01:00", ModifiedBefore: "2026-01-02T04:04:05.000000002+01:00",
	})
	require.Equal(t, []int64{inside.ID}, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, `path:/case_1`, query.Filters{ModifiedBefore: "2026-01-02T03:04:05.000000001Z"})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""))
	compiled = compileFixtureQuery(t, s, `path:/`, query.Filters{ExcludePaths: []string{"/case_1"}})
	require.Len(t, compiledFixtureIDs(t, s.db, compiled, ""), 1)
}

func TestCompiledQuerySQLiteExtensionRequiresBasename(t *testing.T) {
	s := newTestStore(t)
	want := []int64{}
	for _, name := range []string{".pdf", ".PDF", "README", "report.", "report.pdf", ".report.pdf", "REPORT.PDF"} {
		node, err := s.CreateFile(t.Context(), s.RootID(), name, fakeHash("a1"), 10, "application/octet-stream")
		require.NoError(t, err)
		if name == "report.pdf" || name == ".report.pdf" || name == "REPORT.PDF" {
			want = append(want, node.ID)
		}
	}
	for _, testCase := range []struct {
		name    string
		text    string
		filters query.Filters
	}{
		{name: "expression", text: "extension:pdf"},
		{name: "filter", filters: query.Filters{Extensions: []string{"pdf"}}},
		{name: "media family", text: "media_family:document"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			compiled := compileFixtureQuery(t, s, testCase.text, testCase.filters)
			require.Equal(t, want, compiledFixtureIDs(t, s.db, compiled, ""))
		})
	}
}

func TestCompiledQuerySQLitePhrasesNearAndSimpleSourceSemantics(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	adjacent, err := s.CreateFile(ctx, s.RootID(), "alpha beta.txt", fakeHash("f6"), 10, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "alpha distant distant beta.txt", fakeHash("a7"), 10, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, adjacent, "gamma")
	for _, text := range []string{`name:"alpha beta"`, `name:(alpha NEAR/0 beta)`, `name:alpha AND gamma`} {
		compiled := compileFixtureQuery(t, s, text, query.Filters{})
		require.Equal(t, []int64{adjacent.ID}, compiledFixtureIDs(t, s.db, compiled, ""), text)
	}
	value, err := query.Parse([]byte(`{"text":"alpha gamma"}`))
	require.NoError(t, err)
	compiled, err := compileQuery(ctx, value, queryResolver{q: s.db})
	require.NoError(t, err)
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""), "simple mode retains same-source implicit AND")
	compiled = compileFixtureQuery(t, s, `name:"' OR 1=1 --"`, query.Filters{})
	require.Empty(t, compiledFixtureIDs(t, s.db, compiled, ""), "entered text must remain an FTS value, never SQL")
}

func seedCompiledLegacyText(t *testing.T, s *Store, node Node, text string) {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(), `INSERT INTO content_fts(blob_hash,extractor,text) VALUES(?,?,?)`, node.BlobHash, "synthetic-test", text)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `INSERT OR IGNORE INTO text_searchable_versions(version_id) VALUES(?)`, node.CurrentVersionID)
	require.NoError(t, err)
}

// A selected rendition generation makes the legacy cache non-serving, and
// historical attachment text must not match a replacement current version.
func TestCompiledQuerySQLiteCurrentRenditionGeneration(t *testing.T) {
	s, versions := newRenditionCatalogFixture(t)
	ctx := t.Context()
	profile := catalogProcessingProfile(t, false)
	build := lexicalSearchBuild(s, profile, catalogBuildID, "mercury phrase")
	require.NoError(t, s.StageRenditionBuild(ctx, build))
	generation, err := s.StageLexicalGeneration(ctx, fakeHash("a9"))
	require.NoError(t, err)
	attachment := RenditionAttachmentRecord{ID: catalogAttachmentFirst, VaultID: s.VaultID(),
		ContentVersionID: versions[0], BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, s.PublishRenditionAndLexicalHeads(ctx, attachment, RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime}, generation.ID))
	legacy, err := s.CreateFile(ctx, s.RootID(), "legacy.txt", fakeHash("b8"), 10, "text/plain")
	require.NoError(t, err)
	seedCompiledLegacyText(t, s, legacy, "mercury phrase")
	node, err := s.NodeByPath(ctx, "/synthetic-source-a.pdf")
	require.NoError(t, err)
	compiled := compileFixtureQuery(t, s, `"mercury phrase"`, query.Filters{})
	require.NoError(t, s.withLexicalGenerationRead(ctx, func(q metadataQuerier, selected LexicalGeneration) error {
		require.Equal(t, []int64{node.ID}, compiledFixtureIDs(t, q, compiled, selected.ID))
		return nil
	}))
	_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("c8"), 11, "application/pdf")
	require.NoError(t, err)
	require.NoError(t, s.withLexicalGenerationRead(ctx, func(q metadataQuerier, selected LexicalGeneration) error {
		require.Empty(t, compiledFixtureIDs(t, q, compiled, selected.ID))
		return nil
	}))
}
