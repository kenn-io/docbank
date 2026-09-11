package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/query"
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

func registerQueryCompileRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "parseQuery", Method: http.MethodPost, Path: "/api/v1/queries/parse",
		Summary:      "Validate a search expression and resolve its saved references",
		Description:  "Returns canonical query intent and observed dependency revisions, without executing a search.",
		MaxBodyBytes: query.MaxInputBytes,
	}, func(ctx context.Context, in *struct{ Body QueryPayload }) (*struct{ Body QueryPreview }, error) {
		value, err := query.Parse(in.Body)
		if err != nil {
			problem := NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
			problem.Position = &ErrorPosition{}
			return nil, problem
		}
		compiled, err := d.Store.CompileQuery(ctx, value)
		if err != nil {
			if positioned, ok := errors.AsType[*query.ExpressionError](err); ok {
				problem := NewError(http.StatusUnprocessableEntity, "invalid_query", positioned.Message)
				problem.Position = &ErrorPosition{Offset: positioned.Offset, End: positioned.End}
				return nil, problem
			}
			return nil, NewError(http.StatusInternalServerError, "internal", "Could not compile query")
		}
		canonical, err := query.Canonical(compiled.Query)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "Could not encode query")
		}
		fingerprint, err := query.Fingerprint(compiled.Query)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "Could not fingerprint query")
		}
		preview := QueryPreview{Query: QueryPayload(canonical), QueryFingerprint: fingerprint, Dependencies: []QueryDependency{}}
		for _, dependency := range compiled.Dependencies {
			preview.Dependencies = append(preview.Dependencies, QueryDependency{
				Kind: string(dependency.Kind), ID: dependency.ID, Revision: dependency.Revision,
			})
		}
		return &struct{ Body QueryPreview }{Body: preview}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "previewQueryHighlights", Method: http.MethodPost,
		Path: "/api/v1/queries/highlights", Summary: "Preview positive document-text highlight terms",
		Description:  "Returns only bounded positive text operands from the resolved compiler AST; structured, name-only, and negated operands are excluded.",
		MaxBodyBytes: query.MaxInputBytes,
	}, func(ctx context.Context, in *struct{ Body QueryPayload }) (*struct{ Body QueryHighlightPreview }, error) {
		value, err := query.Parse(in.Body)
		if err != nil {
			problem := NewError(http.StatusUnprocessableEntity, "invalid_query", err.Error())
			problem.Position = &ErrorPosition{}
			return nil, problem
		}
		compiled, err := d.Store.CompileQuery(ctx, value)
		if err != nil {
			if positioned, ok := errors.AsType[*query.ExpressionError](err); ok {
				problem := NewError(http.StatusUnprocessableEntity, "invalid_query", positioned.Message)
				problem.Position = &ErrorPosition{Offset: positioned.Offset, End: positioned.End}
				return nil, problem
			}
			return nil, NewError(http.StatusInternalServerError, "internal", "Could not compile query highlights")
		}
		fingerprint, err := query.Fingerprint(compiled.Query)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal", "Could not fingerprint query highlights")
		}
		preview := QueryHighlightPreview{
			QueryFingerprint: fingerprint,
			Dependencies:     make([]QueryDependency, 0, len(compiled.Dependencies)),
			Terms:            compiled.PositiveTextTerms(),
		}
		for _, dependency := range compiled.Dependencies {
			preview.Dependencies = append(preview.Dependencies, QueryDependency{
				Kind: string(dependency.Kind), ID: dependency.ID, Revision: dependency.Revision,
			})
		}
		return &struct{ Body QueryHighlightPreview }{Body: preview}, nil
	})
}
