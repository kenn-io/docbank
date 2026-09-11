package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	tagID        = "11111111-1111-4111-8111-111111111111"
	otherTagID   = "22222222-2222-4222-8222-222222222222"
	collectionID = "33333333-3333-4333-8333-333333333333"
	savedID      = "44444444-4444-4444-8444-444444444444"
	otherSavedID = "55555555-5555-4555-8555-555555555555"
)

type resolverCall struct {
	kind ReferenceKind
	key  string
	byID bool
}

type resolverFunc func(context.Context, ReferenceKind, string, bool) (Reference, error)

func (fn resolverFunc) Resolve(ctx context.Context, kind ReferenceKind, key string, byID bool) (Reference, error) {
	return fn(ctx, kind, key, byID)
}

func TestResolveQueryExpandsFieldScopedReferencesWithoutLosingBooleanScope(t *testing.T) {
	var calls []resolverCall
	resolver := resolverFunc(func(_ context.Context, kind ReferenceKind, key string, byID bool) (Reference, error) {
		calls = append(calls, resolverCall{kind: kind, key: key, byID: byID})
		ids := map[string]string{"red": tagID, "blue team": otherTagID, collectionID: collectionID}
		return Reference{Dependency: Dependency{Kind: kind, ID: ids[key], Revision: 7}}, nil
	})

	text := `tag:(red OR NOT "blue team") AND collection:` + collectionID
	resolved, err := ResolveQuery(context.Background(), lexicalQuery(text), resolver)
	require.NoError(t, err)

	assert.Equal(t, []resolverCall{
		{kind: ReferenceTag, key: "red", byID: false},
		{kind: ReferenceTag, key: "blue team", byID: false},
		{kind: ReferenceCollection, key: collectionID, byID: true},
	}, calls)
	require.Equal(t, ExpressionAnd, resolved.Expression.Syntax.Kind)
	tagField := resolved.Expression.Children[0]
	require.Equal(t, ExpressionField, tagField.Syntax.Kind)
	require.Equal(t, ExpressionOr, tagField.Children[0].Syntax.Kind)
	assert.Equal(t, &Dependency{Kind: ReferenceTag, ID: tagID, Revision: 7}, tagField.Children[0].Children[0].Dependency)
	notBlue := tagField.Children[0].Children[1]
	require.Equal(t, ExpressionNot, notBlue.Syntax.Kind)
	assert.Equal(t, &Dependency{Kind: ReferenceTag, ID: otherTagID, Revision: 7}, notBlue.Children[0].Dependency)
	assert.Equal(t, "blue team", notBlue.Children[0].Syntax.Value)
	collectionLeaf := resolved.Expression.Children[1].Children[0]
	assert.Equal(t, &Dependency{Kind: ReferenceCollection, ID: collectionID, Revision: 7}, collectionLeaf.Dependency)
	assert.Equal(t, []Dependency{
		{Kind: ReferenceCollection, ID: collectionID, Revision: 7},
		{Kind: ReferenceTag, ID: tagID, Revision: 7},
		{Kind: ReferenceTag, ID: otherTagID, Revision: 7},
	}, resolved.Dependencies)
}

func TestResolveQuerySavedReferenceRetainsNestedASTAndFacetScope(t *testing.T) {
	nested := lexicalQuery(`tag:red OR beta`)
	nested.Filters.TagIDs = []string{otherTagID}
	nested.Filters.ExcludeCollectionIDs = []string{collectionID}
	root := lexicalQuery(`alpha AND saved:"Case review"`)
	root.Filters.TagIDs = []string{tagID}

	resolver := resolverFunc(func(_ context.Context, kind ReferenceKind, key string, byID bool) (Reference, error) {
		switch {
		case kind == ReferenceSaved && key == "Case review" && !byID:
			return Reference{Dependency: Dependency{Kind: kind, ID: savedID, Revision: 9}, Query: &nested}, nil
		case kind == ReferenceTag && key == "red" && !byID:
			return Reference{Dependency: Dependency{Kind: kind, ID: tagID, Revision: 2}}, nil
		case kind == ReferenceTag && key == tagID && byID:
			return Reference{Dependency: Dependency{Kind: kind, ID: tagID, Revision: 2}}, nil
		case kind == ReferenceTag && key == otherTagID && byID:
			return Reference{Dependency: Dependency{Kind: kind, ID: otherTagID, Revision: 3}}, nil
		case kind == ReferenceCollection && key == collectionID && byID:
			return Reference{Dependency: Dependency{Kind: kind, ID: collectionID, Revision: 4}}, nil
		default:
			return Reference{}, fmt.Errorf("unexpected resolution: %s %q %v", kind, key, byID)
		}
	})

	resolved, err := ResolveQuery(context.Background(), root, resolver)
	require.NoError(t, err)
	assert.Equal(t, root.Text, resolved.Query.Text)
	assert.Equal(t, []string{tagID}, resolved.Query.Filters.TagIDs)

	savedLeaf := resolved.Expression.Children[1].Children[0]
	require.NotNil(t, savedLeaf.Saved)
	assert.Equal(t, &Dependency{Kind: ReferenceSaved, ID: savedID, Revision: 9}, savedLeaf.Dependency)
	assert.Equal(t, nested.Text, savedLeaf.Saved.Query.Text)
	assert.Equal(t, []string{otherTagID}, savedLeaf.Saved.Query.Filters.TagIDs)
	assert.Equal(t, []string{collectionID}, savedLeaf.Saved.Query.Filters.ExcludeCollectionIDs)
	require.Equal(t, ExpressionOr, savedLeaf.Saved.Expression.Syntax.Kind)
	assert.Equal(t, &Dependency{Kind: ReferenceTag, ID: tagID, Revision: 2}, savedLeaf.Saved.Expression.Children[0].Children[0].Dependency)
	assert.Equal(t, []Dependency{
		{Kind: ReferenceCollection, ID: collectionID, Revision: 4},
		{Kind: ReferenceTag, ID: tagID, Revision: 2},
		{Kind: ReferenceTag, ID: otherTagID, Revision: 3},
	}, savedLeaf.Saved.Dependencies)
	assert.Equal(t, []Dependency{
		{Kind: ReferenceCollection, ID: collectionID, Revision: 4},
		{Kind: ReferenceSaved, ID: savedID, Revision: 9},
		{Kind: ReferenceTag, ID: tagID, Revision: 2},
		{Kind: ReferenceTag, ID: otherTagID, Revision: 3},
	}, resolved.Dependencies)
}

func TestResolveQueryNormalizesCopyWithoutMutatingInput(t *testing.T) {
	sizeMax := int64(20)
	input := lexicalQuery("alpha")
	input.Filters.TagIDs = []string{otherTagID, tagID, tagID}
	input.Filters.SizeMax = &sizeMax
	originalIDs := append([]string(nil), input.Filters.TagIDs...)

	resolved, err := ResolveQuery(context.Background(), input, resolverFunc(func(_ context.Context, kind ReferenceKind, key string, byID bool) (Reference, error) {
		return Reference{Dependency: Dependency{Kind: kind, ID: key, Revision: 1}}, nil
	}))
	require.NoError(t, err)
	assert.Equal(t, originalIDs, input.Filters.TagIDs)
	assert.Equal(t, []string{tagID, otherTagID}, resolved.Query.Filters.TagIDs)
	*resolved.Query.Filters.SizeMax = 10
	assert.Equal(t, int64(20), *input.Filters.SizeMax)
}

func TestResolveQueryUnknownReferencesUseOperandOrWholeFacetSpan(t *testing.T) {
	resolver := resolverFunc(func(context.Context, ReferenceKind, string, bool) (Reference, error) {
		return Reference{}, fmt.Errorf("lookup: %w", ErrUnknownReference)
	})
	for _, testCase := range []struct {
		name  string
		query Query
		start int
		end   int
	}{
		{name: "negative tag", query: lexicalQuery(`tag:(NOT missing)`), start: 9, end: 16},
		{name: "deleted saved", query: lexicalQuery(`saved:gone`), start: 6, end: 10},
		{name: "negative collection facet", query: queryWithExcludedCollection("root", collectionID), start: 0, end: 4},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ResolveQuery(context.Background(), testCase.query, resolver)
			expressionErr := requireExpressionError(t, testCase.query.Text, err)
			assert.Equal(t, testCase.start, expressionErr.Offset)
			assert.Equal(t, testCase.end, expressionErr.End)
		})
	}
}

func TestResolveQueryPropagatesBackendErrorIdentity(t *testing.T) {
	backendErr := errors.New("backend unavailable")
	_, err := ResolveQuery(context.Background(), lexicalQuery(`tag:red`), resolverFunc(func(context.Context, ReferenceKind, string, bool) (Reference, error) {
		return Reference{}, backendErr
	}))
	require.ErrorIs(t, err, backendErr)
	var expressionErr *ExpressionError
	assert.NotErrorAs(t, err, &expressionErr)
}

func TestResolveQueryRejectsCyclesByStableSavedIDAcrossAliases(t *testing.T) {
	first := lexicalQuery(`saved:second-alias`)
	second := lexicalQuery(`saved:first-alias`)
	resolver := resolverFunc(func(_ context.Context, kind ReferenceKind, key string, _ bool) (Reference, error) {
		switch key {
		case "first-alias":
			return Reference{Dependency: Dependency{Kind: kind, ID: savedID, Revision: 1}, Query: &first}, nil
		case "second-alias":
			return Reference{Dependency: Dependency{Kind: kind, ID: otherSavedID, Revision: 1}, Query: &second}, nil
		default:
			return Reference{}, ErrUnknownReference
		}
	})

	text := `saved:first-alias`
	_, err := ResolveQuery(context.Background(), lexicalQuery(text), resolver)
	expressionErr := requireExpressionError(t, text, err)
	assert.Equal(t, 6, expressionErr.Offset)
	assert.Equal(t, len(text), expressionErr.End)
	assert.Contains(t, expressionErr.Message, "cycle")
}

func TestResolveQueryAllowsRepeatedConsistentReferencesAndRejectsRevisionConflicts(t *testing.T) {
	t.Run("consistent", func(t *testing.T) {
		resolved, err := ResolveQuery(context.Background(), lexicalQuery(`tag:red OR tag:red`), resolverFunc(func(_ context.Context, kind ReferenceKind, _ string, _ bool) (Reference, error) {
			return Reference{Dependency: Dependency{Kind: kind, ID: tagID, Revision: 3}}, nil
		}))
		require.NoError(t, err)
		assert.Equal(t, []Dependency{{Kind: ReferenceTag, ID: tagID, Revision: 3}}, resolved.Dependencies)
	})

	t.Run("conflict is backend error", func(t *testing.T) {
		calls := 0
		_, err := ResolveQuery(context.Background(), lexicalQuery(`tag:red OR tag:red`), resolverFunc(func(_ context.Context, kind ReferenceKind, _ string, _ bool) (Reference, error) {
			calls++
			return Reference{Dependency: Dependency{Kind: kind, ID: tagID, Revision: int64(calls)}}, nil
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "conflicting")
		var expressionErr *ExpressionError
		assert.NotErrorAs(t, err, &expressionErr)
	})
}

func TestResolveQueryRejectsMalformedResolverResultsAsBackendErrors(t *testing.T) {
	savedQuery := lexicalQuery("alpha")
	tests := map[string]Reference{
		"wrong kind":     {Dependency: Dependency{Kind: ReferenceCollection, ID: tagID, Revision: 1}},
		"invalid id":     {Dependency: Dependency{Kind: ReferenceTag, ID: "not-an-id", Revision: 1}},
		"wrong exact id": {Dependency: Dependency{Kind: ReferenceTag, ID: otherTagID, Revision: 1}},
		"zero revision":  {Dependency: Dependency{Kind: ReferenceTag, ID: tagID, Revision: 0}},
	}
	for name, result := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveQuery(context.Background(), lexicalQuery(`tag:`+tagID), resolverFunc(func(context.Context, ReferenceKind, string, bool) (Reference, error) {
				return result, nil
			}))
			require.Error(t, err)
			var expressionErr *ExpressionError
			assert.NotErrorAs(t, err, &expressionErr)
		})
	}

	for name, saved := range map[string]*Query{"nil query": nil, "valid control": &savedQuery} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveQuery(context.Background(), lexicalQuery(`saved:one`), resolverFunc(func(context.Context, ReferenceKind, string, bool) (Reference, error) {
				return Reference{Dependency: Dependency{Kind: ReferenceSaved, ID: savedID, Revision: 1}, Query: saved}, nil
			}))
			if name == "valid control" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestResolveQueryRejectsUnsupportedReferenceFieldOperandsAndNestedFields(t *testing.T) {
	for _, text := range []string{
		`tag:red*`,
		`tag:(red NEAR blue)`,
		`tag:(collection:blue)`,
		`name:(tag:red)`,
	} {
		t.Run(text, func(t *testing.T) {
			_, err := ResolveQuery(context.Background(), lexicalQuery(text), nil)
			_ = requireExpressionError(t, text, err)
		})
	}
}

func TestResolveQueryRejectsNonLexicalNestedQueriesAtOuterOperandSpan(t *testing.T) {
	nested := lexicalQuery("alpha")
	nested.Mode = "semantic"
	text := `saved:semantic`
	_, err := ResolveQuery(context.Background(), lexicalQuery(text), resolverFunc(func(context.Context, ReferenceKind, string, bool) (Reference, error) {
		return Reference{Dependency: Dependency{Kind: ReferenceSaved, ID: savedID, Revision: 1}, Query: &nested}, nil
	}))
	expressionErr := requireExpressionError(t, text, err)
	assert.Equal(t, 6, expressionErr.Offset)
	assert.Equal(t, len(text), expressionErr.End)
}

func TestResolveQueryHonorsContextAndNilResolver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ResolveQuery(ctx, lexicalQuery(`tag:red`), resolverFunc(func(context.Context, ReferenceKind, string, bool) (Reference, error) {
		t.Fatal("resolver called after cancellation")
		return Reference{}, nil
	}))
	require.ErrorIs(t, err, context.Canceled)

	resolved, err := ResolveQuery(context.Background(), lexicalQuery("alpha"), nil)
	require.NoError(t, err)
	assert.Empty(t, resolved.Dependencies)

	_, err = ResolveQuery(context.Background(), lexicalQuery(`tag:red`), nil)
	require.Error(t, err)
	var expressionErr *ExpressionError
	assert.NotErrorAs(t, err, &expressionErr)
}

func TestResolveQueryEnforcesSavedDepthNodeByteAndDependencyBounds(t *testing.T) {
	t.Run("saved depth", func(t *testing.T) {
		resolver := resolverFunc(func(_ context.Context, kind ReferenceKind, key string, _ bool) (Reference, error) {
			var index int
			_, err := fmt.Sscanf(key, "level-%d", &index)
			require.NoError(t, err)
			next := lexicalQuery(fmt.Sprintf("saved:level-%d", index+1))
			id := fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
			return Reference{Dependency: Dependency{Kind: kind, ID: id, Revision: 1}, Query: &next}, nil
		})
		text := "saved:level-0"
		_, err := ResolveQuery(context.Background(), lexicalQuery(text), resolver)
		_ = requireExpressionError(t, text, err)
	})

	t.Run("expanded nodes", func(t *testing.T) {
		large := lexicalQuery(strings.TrimSpace(strings.Repeat("x ", 256)))
		resolver := resolverFunc(func(_ context.Context, kind ReferenceKind, _ string, _ bool) (Reference, error) {
			return Reference{Dependency: Dependency{Kind: kind, ID: savedID, Revision: 1}, Query: &large}, nil
		})
		text := strings.TrimSuffix(strings.Repeat("saved:large OR ", 8), " OR ")
		_, err := ResolveQuery(context.Background(), lexicalQuery(text), resolver)
		_ = requireExpressionError(t, text, err)
	})

	t.Run("expanded bytes", func(t *testing.T) {
		large := lexicalQuery(strings.Repeat("x", 8192))
		resolver := resolverFunc(func(_ context.Context, kind ReferenceKind, _ string, _ bool) (Reference, error) {
			return Reference{Dependency: Dependency{Kind: kind, ID: savedID, Revision: 1}, Query: &large}, nil
		})
		text := strings.TrimSuffix(strings.Repeat("saved:large OR ", 32), " OR ")
		_, err := ResolveQuery(context.Background(), lexicalQuery(text), resolver)
		_ = requireExpressionError(t, text, err)
	})

	t.Run("distinct dependencies", func(t *testing.T) {
		query := lexicalQuery(`tag:overflow`)
		for index := range 64 {
			query.Filters.TagIDs = append(query.Filters.TagIDs, boundedUUID(index, 1))
			query.Filters.ExcludeTagIDs = append(query.Filters.ExcludeTagIDs, boundedUUID(index+64, 2))
			query.Filters.CollectionIDs = append(query.Filters.CollectionIDs, boundedUUID(index+128, 3))
			query.Filters.ExcludeCollectionIDs = append(query.Filters.ExcludeCollectionIDs, boundedUUID(index+192, 4))
		}
		resolver := resolverFunc(func(_ context.Context, kind ReferenceKind, key string, byID bool) (Reference, error) {
			if !byID {
				return Reference{Dependency: Dependency{Kind: kind, ID: tagID, Revision: 1}}, nil
			}
			return Reference{Dependency: Dependency{Kind: kind, ID: key, Revision: 1}}, nil
		})
		_, err := ResolveQuery(context.Background(), query, resolver)
		_ = requireExpressionError(t, query.Text, err)
	})
}

func lexicalQuery(text string) Query {
	return Query{V: 1, Text: text, Syntax: "advanced", Mode: "lexical", Sort: Sort{Field: "name", Direction: "asc"}}
}

func queryWithExcludedCollection(text, id string) Query {
	query := lexicalQuery(text)
	query.Filters.ExcludeCollectionIDs = []string{id}
	return query
}

func boundedUUID(index, group int) string {
	return fmt.Sprintf("%08x-%04x-4000-8000-%012x", index+1, group, index+1)
}
