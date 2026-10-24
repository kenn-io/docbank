package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

const (
	compilerTagID        = "11111111-1111-4111-8111-111111111111"
	compilerCollectionID = "22222222-2222-4222-8222-222222222222"
	compilerSavedID      = "33333333-3333-4333-8333-333333333333"
	compilerOtherTagID   = "44444444-4444-4444-8444-444444444444"
	compilerOtherCollID  = "55555555-5555-4555-8555-555555555555"
)

type compilerResolverFunc func(context.Context, query.ReferenceKind, string, bool) (query.Reference, error)

func (fn compilerResolverFunc) Resolve(
	ctx context.Context, kind query.ReferenceKind, key string, byID bool,
) (query.Reference, error) {
	return fn(ctx, kind, key, byID)
}

func compilerQuery(t *testing.T, text string) query.Query {
	t.Helper()
	value, err := query.Parse([]byte(`{"syntax":"advanced"}`))
	require.NoError(t, err)
	value.Text = text
	return value
}

func TestCompileQueryPreservesNormalizedQueryAndDependencies(t *testing.T) {
	t.Parallel()
	value := compilerQuery(t, `tag:urgent AND "alpha beta"*`)
	compiled, err := compileQuery(t.Context(), value, compilerResolverFunc(func(
		_ context.Context, kind query.ReferenceKind, key string, byID bool,
	) (query.Reference, error) {
		require.Equal(t, query.ReferenceTag, kind)
		require.Equal(t, "urgent", key)
		require.False(t, byID)
		return query.Reference{Dependency: query.Dependency{
			Kind: kind, ID: compilerTagID, Revision: 7,
		}}, nil
	}))
	require.NoError(t, err)

	assert.Equal(t, value, compiled.Query)
	assert.Equal(t, []query.Dependency{{
		Kind: query.ReferenceTag, ID: compilerTagID, Revision: 7,
	}}, compiled.Dependencies)
	predicate, args, err := compiled.Bind("")
	require.NoError(t, err)
	assert.NotEmpty(t, predicate)
	assert.Contains(t, args, compilerTagID)
	assert.Contains(t, args, `"alpha beta"*`)
}

func TestCompiledQueryBindsSimpleAndAdvancedLexicalPredicates(t *testing.T) {
	t.Parallel()
	t.Run("simple is one same-source FTS expression", func(t *testing.T) {
		value, err := query.Parse([]byte(`{"text":"alpha  beta"}`))
		require.NoError(t, err)
		compiled, err := compileQuery(t.Context(), value, nil)
		require.NoError(t, err)
		_, args, err := compiled.Bind("")
		require.NoError(t, err)
		assert.Equal(t, 3, countArgument(args, `"alpha"* "beta"*`))
		assert.Zero(t, countArgument(args, `"alpha"*`))
	})

	t.Run("advanced preserves source scope and quotes FTS values", func(t *testing.T) {
		value := compilerQuery(t, `name:"a\"b"* OR (alpha NEAR/4 beta)`)
		compiled, err := compileQuery(t.Context(), value, nil)
		require.NoError(t, err)
		sql, args, err := compiled.Bind("generation-a")
		require.NoError(t, err)
		assert.NotContains(t, sql, `a"b`)
		assert.Contains(t, args, `"a""b"*`)
		assert.Contains(t, args, `NEAR("alpha" "beta",4)`)
		assert.Contains(t, args, "generation-a")
		assert.Contains(t, sql, "content_fts")
		assert.Contains(t, sql, "rendition_lexical_fts")
	})
}

func TestCompileQueryCollectsLexicalOperandsOnlyForPhotos(t *testing.T) {
	t.Parallel()
	for _, text := range []string{`alpha`, `name:alpha OR NOT beta`, `alpha NEAR/4 beta`} {
		t.Run(text, func(t *testing.T) {
			value := compilerQuery(t, text)
			documents, err := compileQuery(t.Context(), value, nil)
			require.NoError(t, err)
			require.Empty(t, documents.predicate.lexical)
			photos, err := (queryCompiler{photoDisplayMetadata: true}).compile(t.Context(), value, nil)
			require.NoError(t, err)
			require.NotEmpty(t, photos.predicate.lexical)
		})
	}
}

func TestCompiledQueryBindClonesGenerationArguments(t *testing.T) {
	t.Parallel()
	compiled, err := compileQuery(t.Context(), compilerQuery(t, "alpha"), nil)
	require.NoError(t, err)
	_, first, err := compiled.Bind("generation-a")
	require.NoError(t, err)
	_, second, err := compiled.Bind("generation-b")
	require.NoError(t, err)
	assert.Contains(t, first, "generation-a")
	assert.NotContains(t, first, "generation-b")
	assert.Contains(t, second, "generation-b")
	assert.NotContains(t, second, "generation-a")
}

func TestCompileQuerySupportsEveryBoundExpressionField(t *testing.T) {
	t.Parallel()
	resolver := compilerResolver(nil)
	tests := []struct {
		name string
		text string
		want any
	}{
		{name: "name", text: `name:"alpha beta"*`, want: `"alpha beta"*`},
		{name: "path", text: `path:/case_1`, want: "/case_1"},
		{name: "tag", text: `tag:urgent`, want: compilerTagID},
		{name: "collection", text: `collection:batch`, want: compilerCollectionID},
		{name: "mime", text: `mime:application/pdf`, want: "application/pdf"},
		{name: "extension", text: `extension:tar_gz`, want: `_%.tar\_gz`},
		{name: "media family", text: `media_family:document`, want: "document"},
		{name: "modified after", text: `modified_after:"2026-01-02T03:04:05.1+02:00"`, want: "2026-01-02T01:04:05.100000000Z"},
		{name: "modified before", text: `modified_before:"2026-01-02T03:04:05.000000002Z"`, want: "2026-01-02T03:04:05.000000002Z"},
		{name: "size min", text: `size_min:0`, want: int64(0)},
		{name: "size max", text: `size_max:9007199254740991`, want: int64(9007199254740991)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			compiled, err := compileQuery(t.Context(), compilerQuery(t, testCase.text), resolver)
			require.NoError(t, err)
			sql, args, err := compiled.Bind("")
			require.NoError(t, err)
			assert.Contains(t, args, testCase.want)
			if text, ok := testCase.want.(string); ok && text != `_%.tar\_gz` {
				assert.NotContains(t, sql, text)
			}
		})
	}
}

func TestCompileQuerySupportsCanonicalCoverageAndDuplicatePredicates(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		`text_coverage:(complete OR partial OR failed OR unprocessed OR none OR unavailable)`,
		`has_duplicates:(true OR false)`,
	} {
		compiled, err := compileQuery(t.Context(), compilerQuery(t, text), nil)
		require.NoError(t, err)
		_, _, err = compiled.Bind("")
		require.ErrorContains(t, err, "matched population bindings")
	}

	for _, text := range []string{
		`text_coverage:pending`, `has_duplicates:yes`, `has_duplicates:TRUE`,
		`text_coverage:complete*`, `has_duplicates:true*`,
		`text_coverage:(partial OR "complete"*)`, `has_duplicates:(true OR "false"*)`,
	} {
		_, err := compileQuery(t.Context(), compilerQuery(t, text), nil)
		var expressionErr *query.ExpressionError
		require.ErrorAs(t, err, &expressionErr)
		require.Positive(t, expressionErr.End-expressionErr.Offset)
	}
}

func TestCompiledQueryRelationRequirementsSurviveNegation(t *testing.T) {
	t.Parallel()
	for _, text := range []string{`NOT text_coverage:complete`, `NOT has_duplicates:true`} {
		compiled, err := compileQuery(t.Context(), compilerQuery(t, text), nil)
		require.NoError(t, err)
		_, _, err = compiled.Bind("")
		require.ErrorContains(t, err, "matched population bindings")
	}
}

func TestCompiledQueryBindRejectsTopLevelDuplicateCollapse(t *testing.T) {
	t.Parallel()
	value := compilerQuery(t, `name:alpha`)
	value.Filters.CollapseDuplicates = true
	compiled, err := compileQuery(t.Context(), value, nil)
	require.NoError(t, err)
	_, _, err = compiled.Bind("")
	require.ErrorContains(t, err, "matched population bindings")
}

func TestCompileQueryRetainsInheritedFieldScopeAcrossBooleanChildren(t *testing.T) {
	t.Parallel()
	compiled, err := compileQuery(t.Context(), compilerQuery(t,
		`mime:(application/pdf OR image/png) AND NOT path:/archive`), nil)
	require.NoError(t, err)
	_, args, err := compiled.Bind("")
	require.NoError(t, err)
	assert.Contains(t, args, "application/pdf")
	assert.Contains(t, args, "image/png")
	assert.Contains(t, args, "/archive")
}

func TestCompiledQueryBindsFacetUnionsAndExclusions(t *testing.T) {
	t.Parallel()
	maxSize := int64(23)
	value := compilerQuery(t, "")
	value.Filters = query.Filters{
		Paths: []string{"/a", "/b"}, ExcludePaths: []string{"/c"},
		CollectionIDs: []string{compilerCollectionID}, ExcludeCollectionIDs: []string{compilerOtherCollID},
		TagIDs: []string{compilerTagID}, ExcludeTagIDs: []string{compilerOtherTagID}, NoTags: true,
		MediaFamilies: []string{"document", "image"}, MIMETypes: []string{"application/pdf", "image/png"},
		Extensions: []string{"pdf", "tar_gz"}, ModifiedAfter: "2026-01-02T03:04:05Z",
		ModifiedBefore: "2026-01-03T03:04:05Z", SizeMin: 1, SizeMax: &maxSize,
	}
	// no_tags cannot coexist with an inclusion list, so exercise it separately.
	value.Filters.NoTags = false
	compiled, err := compileQuery(t.Context(), value, compilerResolver(nil))
	require.NoError(t, err)
	_, args, err := compiled.Bind("")
	require.NoError(t, err)
	for _, expected := range []any{
		"/a", "/b", "/c", compilerCollectionID, compilerOtherCollID,
		compilerTagID, compilerOtherTagID, "document", "image", "application/pdf", "image/png",
		`_%.pdf`, `_%.tar\_gz`, "2026-01-02T03:04:05.000000000Z",
		"2026-01-03T03:04:05.000000000Z", int64(1), maxSize,
	} {
		assert.Contains(t, args, expected)
	}

	value.Filters.TagIDs = nil
	value.Filters.NoTags = true
	compiled, err = compileQuery(t.Context(), value, compilerResolver(nil))
	require.NoError(t, err)
	sql, _, err := compiled.Bind("")
	require.NoError(t, err)
	assert.Contains(t, sql, "NOT EXISTS (SELECT 1 FROM node_tags")
}

func TestCompileQueryRejectsUnsupportedFeaturesAndMalformedFieldOperands(t *testing.T) {
	t.Parallel()
	rootUnsupported := []query.Query{
		func() query.Query { q := compilerQuery(t, ""); q.Sort.Field = "relevance"; return q }(),
		func() query.Query {
			q := compilerQuery(t, "invalid canonical input")
			q.Filters.Paths = []string{"relative"}
			return q
		}(),
	}
	for _, value := range rootUnsupported {
		_, err := compileQuery(t.Context(), value, nil)
		assertWholeExpressionError(t, value.Text, err)
	}

	for _, text := range []string{
		`path:/alpha*`, `mime:not-a-mime`,
		`extension:bad%`, `media_family:bogus`, `modified_after:not-a-time`, `size_min:-1`,
		`size_max:9007199254740992`, `mime:""`, `alpha NEAR (beta OR gamma)`,
		`modified_after:"2026-01-02T03:04:05+24:00"`,
		`modified_before:"2026-01-02T03:04:05.0000000001Z"`,
		`modified_after:"2026-01-02T03:04:05,1Z"`,
		`size_min:9007199254740992`, `size_max:+1`, `size_min:1.5`,
		`unevaluated:false`, `unevaluated:"false"`, `unevaluated:(true OR false)`,
	} {
		t.Run(text, func(t *testing.T) {
			_, err := compileQuery(t.Context(), compilerQuery(t, text), nil)
			var expressionErr *query.ExpressionError
			require.ErrorAs(t, err, &expressionErr)
			assert.GreaterOrEqual(t, expressionErr.Offset, 0)
			assert.LessOrEqual(t, expressionErr.End, len(text))
		})
	}
}

func TestCompileQueryRemapsNestedSavedExpressionErrorsButPreservesBackendErrors(t *testing.T) {
	t.Parallel()
	nested := compilerQuery(t, `text_coverage:complete`)
	text := `saved:inside`
	compiled, err := compileQuery(t.Context(), compilerQuery(t, text), compilerResolver(&nested))
	require.NoError(t, err)
	_, _, err = compiled.Bind("")
	var expressionErr *query.ExpressionError
	require.ErrorAs(t, err, &expressionErr)
	assert.Equal(t, 0, expressionErr.Offset)
	assert.Equal(t, len(text), expressionErr.End)

	nested = compilerQuery(t, "alpha")
	nested.Sort.Field = "relevance"
	_, err = compileQuery(t.Context(), compilerQuery(t, text), compilerResolver(&nested))
	require.ErrorAs(t, err, &expressionErr)
	assert.Equal(t, 6, expressionErr.Offset)
	assert.Equal(t, len(text), expressionErr.End)

	backendErr := errors.New("resolver unavailable")
	_, err = compileQuery(t.Context(), compilerQuery(t, `tag:urgent`), compilerResolverFunc(func(
		context.Context, query.ReferenceKind, string, bool,
	) (query.Reference, error) {
		return query.Reference{}, backendErr
	}))
	require.ErrorIs(t, err, backendErr)
	assert.NotErrorAs(t, err, &expressionErr)
}

func TestCompiledQueryNeverInterpolatesBoundValuesAndEnforcesLimits(t *testing.T) {
	t.Parallel()
	attack := `Robert'); DROP TABLE nodes; --`
	compiled, err := compileQuery(t.Context(), compilerQuery(t, `name:"`+attack+`"`), nil)
	require.NoError(t, err)
	sql, args, err := compiled.Bind("generation-'attack")
	require.NoError(t, err)
	assert.NotContains(t, sql, attack)
	assert.NotContains(t, sql, "generation-'attack")
	assert.Contains(t, args, `"`+attack+`"`)

	tooLarge := CompiledQuery{Query: compilerQuery(t, "bounded"), predicate: compiledQueryFragment{sql: strings.Repeat("x", maxCompiledQuerySQL)}}
	_, _, err = tooLarge.Bind("")
	assertWholeExpressionError(t, tooLarge.Query.Text, err)
	tooMany := CompiledQuery{Query: compilerQuery(t, "bounded"), predicate: compiledQueryFragment{sql: "1", args: make([]any, maxCompiledQueryArgs+1)}}
	_, _, err = tooMany.Bind("")
	assertWholeExpressionError(t, tooMany.Query.Text, err)
}

func compilerResolver(saved *query.Query) compilerResolverFunc {
	return func(_ context.Context, kind query.ReferenceKind, key string, byID bool) (query.Reference, error) {
		ids := map[query.ReferenceKind]map[string]string{
			query.ReferenceTag:        {"urgent": compilerTagID, compilerTagID: compilerTagID, compilerOtherTagID: compilerOtherTagID},
			query.ReferenceCollection: {"batch": compilerCollectionID, compilerCollectionID: compilerCollectionID, compilerOtherCollID: compilerOtherCollID},
			query.ReferenceSaved:      {"inside": compilerSavedID},
		}
		id := ids[kind][key]
		if id == "" {
			return query.Reference{}, query.ErrUnknownReference
		}
		ref := query.Reference{Dependency: query.Dependency{Kind: kind, ID: id, Revision: 1}}
		if kind == query.ReferenceSaved {
			ref.Query = saved
		}
		return ref, nil
	}
}

func countArgument(args []any, value any) int {
	count := 0
	for _, arg := range args {
		if arg == value {
			count++
		}
	}
	return count
}

func assertWholeExpressionError(t *testing.T, text string, err error) {
	t.Helper()
	var expressionErr *query.ExpressionError
	require.ErrorAs(t, err, &expressionErr)
	assert.Equal(t, 0, expressionErr.Offset)
	assert.Equal(t, len(text), expressionErr.End)
}

func personMatchNodeIDs(t *testing.T, s *Store, value query.Query) []int64 {
	t.Helper()
	compiled, err := compileQuery(t.Context(), value, queryResolver{q: s.db})
	require.NoError(t, err)
	predicate, args, err := compiled.Bind("")
	require.NoError(t, err)
	rows, err := s.db.QueryContext(t.Context(), `SELECT n.id FROM nodes n JOIN content_versions cv ON cv.node_id=n.id WHERE `+predicate+` ORDER BY n.id`, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestQueryPersonResolutionFiltersAndHiddenPhotoBrowsing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	p, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	node, _ := faceForDecisionTest(t, s, p.PersonID)
	doc, version := seedPeopleVersion(t, s)
	_, err = s.AssertDocumentPerson(ctx, PersonDocumentAssertion{ContentVersionID: version, PersonID: p.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, compilerQuery(t, `person:"ADA LOVELACE"`)))
	typed := snapshotTestQuery(t, `{}`)
	typed.Filters.PersonIDs = []string{p.PersonID}
	compiled, err := compileQuery(ctx, typed, queryResolver{q: s.db})
	require.NoError(t, err)
	require.Equal(t, []query.Dependency{{Kind: query.ReferencePerson, ID: p.PersonID, Revision: p.Revision}}, compiled.Dependencies)
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, typed))
	page := browsePhotoPage(t, s, `{"syntax":"advanced","text":"person:\"Ada Lovelace\""}`)
	require.Len(t, page.Items, 1)
	duplicate, err := s.CreatePerson(ctx, "Ada Lovelace", "operator")
	require.NoError(t, err)
	_, err = compileQuery(ctx, compilerQuery(t, `person:"Ada Lovelace"`), queryResolver{q: s.db})
	require.ErrorContains(t, err, "use a person ID")
	_, err = s.MergePersons(ctx, p.PersonID, duplicate.PersonID, uuid.NewString(), 1, 1)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+duplicate.PersonID)))
	asset, err := s.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	page = browsePhotoPage(t, s, `{"filters":{"person_ids":["`+p.PersonID+`"]}}`)
	require.Empty(t, page.Items)
	require.ElementsMatch(t, []int64{node.ID, doc}, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+p.PersonID)))
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	page, err = s.ListPhotoAssets(WithPhotoHiddenToken(ctx, token), PhotoBrowseRequest{Query: typed, Hidden: true}, nil)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
}

func TestQueryPersonCurrentPublishedEdges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	version := seedDocumentPeopleEvent(t, s, "author.txt", "d1", nil)
	input, err := s.PrepareDocumentPeopleInputs(ctx, version.ID)
	require.NoError(t, err)
	publication := documentPeoplePublicationForInput(t, input)
	_, err = s.PublishDocumentPeople(ctx, publication)
	require.NoError(t, err)
	require.NotEmpty(t, publication.People.Edges)
	id := publication.People.Edges[0].PersonID
	require.NotEmpty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+id)))
	_, err = s.db.ExecContext(ctx, `UPDATE document_people_state SET binding_epoch=binding_epoch+1`)
	require.NoError(t, err)
	require.Empty(t, personMatchNodeIDs(t, s, compilerQuery(t, "person:"+id)))
}

func TestQueryPersonPhotoLiveSibling(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"trashed", "superseded"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset, nodes := photoTrashFixture(t, s)
			jpeg, _, err := s.ReplaceContent(ctx, nodes[1].ID, nodes[1].Revision, fakeHash("b2"), 20, "image/jpeg")
			require.NoError(t, err)
			nodes[1] = jpeg
			image := fileByRole(asset.Files, PhotoRoleImage)
			_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &image.ID)
			require.NoError(t, err)
			p, err := s.CreatePerson(ctx, "Ada", "operator")
			require.NoError(t, err)
			_, _, err = s.CreatePhotoFace(ctx, nodes[0].CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, p.PersonID)
			require.NoError(t, err)
			filter := `{"filters":{"person_ids":["` + p.PersonID + `"]}}`
			require.Len(t, browsePhotoPage(t, s, filter).Items, 1)
			if state == "trashed" {
				_, _, err = s.Trash(ctx, nodes[0].ID, nodes[0].Revision)
			} else {
				_, _, err = s.ReplaceContent(ctx, nodes[0].ID, nodes[0].Revision, fakeHash("c3"), 30, "application/octet-stream")
			}
			require.NoError(t, err)
			require.Empty(t, browsePhotoPage(t, s, filter).Items)
			_, _, err = s.CreatePhotoFace(ctx, nodes[1].CurrentVersionID, uuid.NewString(), document.FaceBox{Width: .2, Height: .2}, p.PersonID)
			require.NoError(t, err)
			require.Len(t, browsePhotoPage(t, s, filter).Items, 1)
		})
	}
}

func TestCompilePhotoPickShorthandAndLiteralDocumentSearch(t *testing.T) {
	t.Parallel()
	compile := func(text, syntax string, photo bool) CompiledQuery {
		value, err := query.Parse([]byte(`{"syntax":` + strconv.Quote(syntax) + `,"text":` + strconv.Quote(text) + `}`))
		require.NoError(t, err)
		compiled, err := (queryCompiler{photoDisplayMetadata: photo}).compile(t.Context(), value, nil)
		require.NoError(t, err, text)
		return compiled
	}
	photoPick := compile("pick", "advanced", true)
	photoFlag := compile("flag:pick", "advanced", true)
	require.Equal(t, photoFlag.predicate, photoPick.predicate)
	nested := compilerQuery(t, "pick")
	saved, err := (queryCompiler{photoDisplayMetadata: true}).compile(t.Context(), compilerQuery(t, "saved:inside"), compilerResolver(&nested))
	require.NoError(t, err)
	nestedFlag := compilerQuery(t, "flag:pick")
	savedFlag, err := (queryCompiler{photoDisplayMetadata: true}).compile(t.Context(), compilerQuery(t, "saved:inside"), compilerResolver(&nestedFlag))
	require.NoError(t, err)
	require.Equal(t, savedFlag.predicate, saved.predicate)
	documents := compile("pick", "advanced", false)
	require.Equal(t, compile(`"pick"`, "advanced", false).predicate, documents.predicate)
	simple := compile("pick", "simple", true)
	require.Equal(t, compile(`"pick"*`, "advanced", true).predicate, simple.predicate)
}
