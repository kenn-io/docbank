package postgres

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/search/sqlquery"
)

// Distance selects a pgvector operator. The score column is higher-is-better.
// Cosine score is one minus cosine distance. L2 score is the negated L2
// distance. Inner-product score is the negated pgvector inner-product
// distance, which is the inner product.
type Distance string

const (
	DistanceCosine Distance = "cosine"
	DistanceL2     Distance = "l2"
	DistanceIP     Distance = "ip"
)

// VectorMapping identifies the source row and vector column.
type VectorMapping struct {
	SourceTable  string
	SourceKey    string
	VectorColumn string
}

// VectorRequest is one bounded pgvector candidate query. Filters in
// SourcePredicate are applied before CandidateLimit and follow the
// placeholder rules on LexicalRequest. CandidateLimit bounds returned rows;
// it does not report whether an ANN index has further neighbors.
type VectorRequest struct {
	Mapping  VectorMapping
	Distance Distance
	// Query is a textual vector such as "[1,0]" or a driver value PostgreSQL
	// accepts as vector.
	Query           any
	RevisionColumn  string
	SourcePredicate sqlquery.Predicate
	ExtraSourceCols []sqlquery.Column
	CandidateLimit  int
}

// BuildVector returns a candidate SELECT. Query is bound as $1 and cast to
// vector. Callers pass a textual vector such as "[1,0]" or a driver value
// that PostgreSQL accepts as vector. Output columns are doc_key, revision
// when requested, score, then the extra columns.
func BuildVector(req VectorRequest) (sqlquery.Query, error) {
	if err := checkIdentifier("source table", req.Mapping.SourceTable); err != nil {
		return sqlquery.Query{}, err
	}
	if err := checkIdentifier("source key", req.Mapping.SourceKey); err != nil {
		return sqlquery.Query{}, err
	}
	if err := checkIdentifier("vector column", req.Mapping.VectorColumn); err != nil {
		return sqlquery.Query{}, err
	}
	if req.Query == nil {
		return sqlquery.Query{}, errors.New("postgres: query vector is required")
	}
	if text, ok := req.Query.(string); ok && strings.TrimSpace(text) == "" {
		return sqlquery.Query{}, errors.New("postgres: query vector is required")
	}
	if req.CandidateLimit <= 0 {
		return sqlquery.Query{}, errors.New("postgres: candidate limit must be positive")
	}
	op, score, err := distanceSQL(req.Distance)
	if err != nil {
		return sqlquery.Query{}, err
	}
	if req.RevisionColumn != "" {
		if err := checkIdentifier("revision column", req.RevisionColumn); err != nil {
			return sqlquery.Query{}, err
		}
	}
	aliases := map[string]bool{"doc_key": true, "score": true, "revision": true, "distance": true}
	for _, col := range req.ExtraSourceCols {
		if err := checkIdentifier("extra source column", col.Name); err != nil || !validIdentifier(col.As) || aliases[strings.ToLower(col.As)] {
			return sqlquery.Query{}, fmt.Errorf("postgres: invalid or conflicting extra source column %q AS %q", col.Name, col.As)
		}
		aliases[strings.ToLower(col.As)] = true
	}
	predicate := strings.TrimSpace(req.SourcePredicate.SQL)
	if predicate == "" && len(req.SourcePredicate.Args) != 0 {
		return sqlquery.Query{}, errors.New("postgres: source predicate args require SQL")
	}

	col := "d." + quote(req.Mapping.VectorColumn)
	distance := col + " " + op + " $1::vector"
	projection := []string{"d." + quote(req.Mapping.SourceKey) + " AS doc_key"}
	if req.RevisionColumn != "" {
		projection = append(projection, "d."+quote(req.RevisionColumn)+" AS revision")
	}
	projection = append(projection, score(distance)+" AS score")
	for _, extra := range req.ExtraSourceCols {
		projection = append(projection, "d."+quote(extra.Name)+" AS "+quote(extra.As))
	}
	args := []any{req.Query}
	n := 1
	var predSQL string
	if predicate != "" {
		var last int
		predSQL, last = rebase(predicate, n)
		if err := checkArgs("source predicate", last-n, len(req.SourcePredicate.Args)); err != nil {
			return sqlquery.Query{}, err
		}
		n = last
		args = append(args, req.SourcePredicate.Args...)
	}
	n++
	args = append(args, req.CandidateLimit)

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s FROM %s AS d WHERE TRUE", strings.Join(projection, ", "), quote(req.Mapping.SourceTable))
	if predSQL != "" {
		b.WriteString(" AND (")
		b.WriteString(predSQL)
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " ORDER BY %s, doc_key ASC LIMIT $%d", distance, n)
	return sqlquery.Query{SQL: b.String(), Args: args}, nil
}

func distanceSQL(distance Distance) (string, func(string) string, error) {
	switch distance {
	case "", DistanceCosine:
		return "<=>", func(expr string) string { return "1 - (" + expr + ")" }, nil
	case DistanceL2:
		return "<->", func(expr string) string { return "-(" + expr + ")" }, nil
	case DistanceIP:
		return "<#>", func(expr string) string { return "-(" + expr + ")" }, nil
	default:
		return "", nil, fmt.Errorf("postgres: unknown distance %q", distance)
	}
}
