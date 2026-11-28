package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

// QueryPayload preserves raw JSON until the strict shared QueryV1 codec reads
// it, including duplicate-member and numeric-lexeme validation.
type QueryPayload SavedQueryPayload

func (p QueryPayload) MarshalJSON() ([]byte, error) { return SavedQueryPayload(p).MarshalJSON() }

func (p *QueryPayload) UnmarshalJSON(raw []byte) error {
	var value SavedQueryPayload
	if err := value.UnmarshalJSON(raw); err != nil {
		return err
	}
	*p = QueryPayload(value)
	return nil
}

func (QueryPayload) Schema(r huma.Registry) *huma.Schema {
	return r.Schema(reflect.TypeFor[savedQueryV1Schema](), true, "")
}

// QueryDependency identifies the immutable identity and observed definition
// revision used by a preview. It does not claim a result membership snapshot.
type QueryDependency struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// QueryPreview contains canonical intent, not executable SQL or result rows.
type QueryPreview struct {
	Query            QueryPayload      `json:"query"`
	QueryFingerprint string            `json:"query_fingerprint"`
	Dependencies     []QueryDependency `json:"dependencies"`
}

// QueryHighlightPreview is a separate, bounded visual receipt. QueryPreview
// remains the strict QueryV1 parse contract used by existing clients.
type QueryHighlightPreview struct {
	QueryFingerprint string            `json:"query_fingerprint" pattern:"^sha256:[0-9a-f]{64}$"`
	Dependencies     []QueryDependency `json:"dependencies" maxItems:"256"`
	Terms            []string          `json:"terms" maxItems:"64"`
}

// SearchHighlightPreview holds the bounded terms of a plain-words search.
type SearchHighlightPreview struct {
	Terms []string `json:"terms" maxItems:"64"`
}

func browserRoleSearchScope(ctx context.Context) context.Context {
	if browserSessionRequest(ctx) {
		return store.WithSafeRoleSearch(ctx)
	}
	return ctx
}

// compileQueryProblem reports a positioned expression error to the caller and
// hides any other compile failure behind the given message.
func compileQueryProblem(err error, internal string) error {
	if positioned, ok := errors.AsType[*query.ExpressionError](err); ok {
		problem := NewError(http.StatusUnprocessableEntity, "invalid_query", positioned.Message)
		if positioned.End > positioned.Offset {
			problem.Position = &ErrorPosition{Offset: positioned.Offset, End: positioned.End}
		}
		return problem
	}
	return NewError(http.StatusInternalServerError, "internal", internal)
}

func queryDependencies(compiled []query.Dependency) []QueryDependency {
	dependencies := make([]QueryDependency, 0, len(compiled))
	for _, dependency := range compiled {
		dependencies = append(dependencies, QueryDependency{
			Kind: string(dependency.Kind), ID: dependency.ID, Revision: dependency.Revision,
		})
	}
	return dependencies
}

func registerQueryCompileRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "parseQuery", Method: http.MethodPost, Path: "/api/v1/queries/parse",
		Summary:      "Validate a search expression and resolve its saved references",
		Description:  "Returns canonical query intent and observed dependency revisions, without executing a search.",
		MaxBodyBytes: query.MaxInputBytes,
	}, func(ctx context.Context, in *struct{ Body QueryPayload }) (*struct{ Body QueryPreview }, error) {
		ctx = browserRoleSearchScope(ctx)
		value, err := query.Parse(in.Body)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
		}
		compiled, err := d.Store.CompileQuery(ctx, value)
		if err != nil {
			return nil, compileQueryProblem(err, "Could not compile query")
		}
		canonical, fingerprint, err := query.CanonicalWithFingerprint(compiled.Query)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "Could not encode query")
		}
		preview := QueryPreview{Query: QueryPayload(canonical), QueryFingerprint: fingerprint,
			Dependencies: queryDependencies(compiled.Dependencies)}
		return &struct{ Body QueryPreview }{Body: preview}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "previewQueryHighlights", Method: http.MethodPost,
		Path: "/api/v1/queries/highlights", Summary: "Preview positive document-text highlight terms",
		Description:  "Returns only bounded positive text operands from the resolved compiler AST; structured, name-only, and negated operands are excluded.",
		MaxBodyBytes: query.MaxInputBytes,
	}, func(ctx context.Context, in *struct{ Body QueryPayload }) (*struct{ Body QueryHighlightPreview }, error) {
		ctx = browserRoleSearchScope(ctx)
		value, err := query.Parse(in.Body)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
		}
		compiled, err := d.Store.CompileQuery(ctx, value)
		if err != nil {
			return nil, compileQueryProblem(err, "Could not compile query highlights")
		}
		fingerprint, err := query.Fingerprint(compiled.Query)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "Could not fingerprint query highlights")
		}
		preview := QueryHighlightPreview{QueryFingerprint: fingerprint,
			Dependencies: queryDependencies(compiled.Dependencies), Terms: compiled.PositiveTextTerms()}
		return &struct{ Body QueryHighlightPreview }{Body: preview}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "searchHighlights", Method: http.MethodGet,
		Path: "/api/v1/search/highlights", Summary: "Preview the text terms of a plain-words search",
		Description: "Returns the bounded terms the plain-words search for q requires, from the " +
			"analyzer search uses. A quoted phrase is one term without its quotes.",
	}, func(_ context.Context, in *struct {
		Q string `query:"q" maxLength:"131072"`
	}) (*struct{ Body SearchHighlightPreview }, error) {
		terms := store.SearchRequiredTerms(in.Q)
		for i, term := range terms {
			terms[i] = strings.Trim(term, `"`)
		}
		return &struct{ Body SearchHighlightPreview }{
			Body: SearchHighlightPreview{Terms: query.BoundedHighlightTerms(terms)},
		}, nil
	})
}
