package clickhouse

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/kit/search/sqlquery"
)

// Distance selects the ClickHouse function used both for scoring and for the
// index match. The zero value is cosine, as in the other backends.
// Cosine score is one minus cosineDistance. L2 score is the negated
// L2Distance. Inner-product score is dotProduct.
type Distance string

const (
	DistanceCosine Distance = "cosine"
	DistanceL2     Distance = "l2"
	DistanceIP     Distance = "ip"
)

// VectorRequest is one bounded vector candidate query. SourcePredicate is
// passed to ClickHouse unchanged. Query is the reference vector.
// CandidateLimit is the SQL LIMIT. Probes, when positive, sets
// hnsw_candidate_list_size_for_search; otherwise the server default (256)
// applies. Approximate does not change the SQL shape; it records that the
// caller expects an HNSW index and requires the distance function to match
// that index.
type VectorRequest struct {
	SourceTable     string
	SourceKey       string
	VectorColumn    string
	Distance        Distance
	Query           []float32
	RevisionColumn  string
	SourcePredicate sqlquery.Predicate
	ExtraSourceCols []sqlquery.Column
	CandidateLimit  int
	Approximate     bool
	Probes          int
}

// BuildVector returns a candidate SELECT. Columns are doc_key, revision when
// requested, score, then extra columns. The query vector is bound once per
// distance expression.
func BuildVector(req VectorRequest) (sqlquery.Query, error) {
	if err := checkIdentifier("source table", req.SourceTable); err != nil {
		return sqlquery.Query{}, err
	}
	if err := checkIdentifier("source key", req.SourceKey); err != nil {
		return sqlquery.Query{}, err
	}
	if err := checkIdentifier("vector column", req.VectorColumn); err != nil {
		return sqlquery.Query{}, err
	}
	if len(req.Query) == 0 {
		return sqlquery.Query{}, errors.New("clickhouse: query vector is required")
	}
	if req.CandidateLimit <= 0 {
		return sqlquery.Query{}, errors.New("clickhouse: candidate limit must be positive")
	}
	if req.Probes < 0 {
		return sqlquery.Query{}, errors.New("clickhouse: probes must not be negative")
	}
	if !req.Approximate && req.Probes > 0 {
		return sqlquery.Query{}, errors.New("clickhouse: probes require approximate search")
	}
	fn, desc, score, err := vectorOrder(req.Distance)
	if err != nil {
		return sqlquery.Query{}, err
	}
	if req.RevisionColumn != "" {
		if err := checkIdentifier("revision column", req.RevisionColumn); err != nil {
			return sqlquery.Query{}, err
		}
	}
	aliases := map[string]bool{"doc_key": true, "revision": true, "score": true, "distance": true}
	for _, col := range req.ExtraSourceCols {
		if err := checkIdentifier("extra column", col.Name); err != nil || !validIdentifier(col.As) || aliases[strings.ToLower(col.As)] {
			return sqlquery.Query{}, fmt.Errorf("clickhouse: invalid or conflicting extra column %q AS %q", col.Name, col.As)
		}
		aliases[strings.ToLower(col.As)] = true
	}
	predicate := strings.TrimSpace(req.SourcePredicate.SQL)
	if predicate == "" && len(req.SourcePredicate.Args) != 0 {
		return sqlquery.Query{}, errors.New("clickhouse: source predicate args require SQL")
	}

	distance := fn + "(d." + quote(req.VectorColumn) + ", ?)"
	projection := []string{"d." + quote(req.SourceKey) + " AS doc_key"}
	if req.RevisionColumn != "" {
		projection = append(projection, "d."+quote(req.RevisionColumn)+" AS revision")
	}
	projection = append(projection, score(distance)+" AS score")
	for _, col := range req.ExtraSourceCols {
		projection = append(projection, "d."+quote(col.Name)+" AS "+quote(col.As))
	}
	// Placeholders are bound in textual order: score, predicate, ORDER BY, LIMIT.
	args := []any{req.Query}
	args = append(args, req.SourcePredicate.Args...)
	args = append(args, req.Query, req.CandidateLimit)

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s FROM %s AS d", strings.Join(projection, ", "), quote(req.SourceTable))
	if predicate != "" {
		b.WriteString(" WHERE (")
		b.WriteString(predicate)
		b.WriteString(")")
	}
	// A second sort key disables the HNSW index on ClickHouse 26.2.
	// Equal distances therefore have no key tie-break.
	fmt.Fprintf(&b, " ORDER BY %s %s LIMIT ?", distance, desc)
	if req.Probes > 0 {
		b.WriteString(" SETTINGS hnsw_candidate_list_size_for_search = ")
		b.WriteString(strconv.Itoa(req.Probes))
	}
	return sqlquery.Query{SQL: b.String(), Args: args}, nil
}

func vectorOrder(distance Distance) (string, string, func(string) string, error) {
	switch distance {
	case "", DistanceCosine:
		return "cosineDistance", "ASC", func(expr string) string { return "1 - (" + expr + ")" }, nil
	case DistanceL2:
		return "L2Distance", "ASC", func(expr string) string { return "-(" + expr + ")" }, nil
	case DistanceIP:
		return "dotProduct", "DESC", func(expr string) string { return expr }, nil
	default:
		return "", "", nil, fmt.Errorf("clickhouse: unknown distance %q", distance)
	}
}
