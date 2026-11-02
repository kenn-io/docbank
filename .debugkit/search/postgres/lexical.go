package postgres

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/search/sqlquery"
)

// TextQuery selects the tsquery constructor used for caller text.
type TextQuery string

const (
	// QueryPlain binds text with plainto_tsquery. Terms are combined with AND.
	QueryPlain TextQuery = "plain"
	// QueryPhrase binds text with phraseto_tsquery. Term order and adjacency
	// are preserved by PostgreSQL's parser.
	QueryPhrase TextQuery = "phrase"
	// QueryWeb binds text with websearch_to_tsquery.
	QueryWeb TextQuery = "web"
)

// Rank selects the native tsvector rank function.
type Rank string

const (
	RankDefault Rank = "ts_rank"
	RankCover   Rank = "ts_rank_cd"
)

// LexicalMapping identifies the source row and the tsvector expression.
// Vector is trusted SQL over alias d. It can be a stored column or a weighted
// expression such as setweight(to_tsvector('simple', d.title), 'A').
type LexicalMapping struct {
	SourceTable string
	SourceKey   string
	Vector      string
}

// LexicalRequest is one bounded full-text candidate query.
// Set Text or TSQuery, not both. TSQuery is trusted SQL of type tsquery and
// uses the same ? placeholders as SourcePredicate. In both, a ? inside a
// quote, dollar quote, or comment stays literal, and ?? outside those
// regions is one literal ?. Each remaining ? becomes the next $n. It exists for callers whose
// analyzer emits a tsquery the plain constructors cannot represent.
type LexicalRequest struct {
	Mapping         LexicalMapping
	Config          string
	Query           TextQuery
	Text            string
	TSQuery         string
	TSQueryArgs     []any
	Rank            Rank
	RevisionColumn  string
	SourcePredicate sqlquery.Predicate
	ExtraSourceCols []sqlquery.Column
	CandidateLimit  int
}

// BuildLexical returns a candidate SELECT. Output columns are doc_key,
// revision when requested, score, then the extra columns, the same order as
// BuildVector. Score is the native
// rank, higher first. Ties break by source key. Outer queries must set their
// own order.
func BuildLexical(req LexicalRequest) (sqlquery.Query, error) {
	if err := checkIdentifier("source table", req.Mapping.SourceTable); err != nil {
		return sqlquery.Query{}, err
	}
	if err := checkIdentifier("source key", req.Mapping.SourceKey); err != nil {
		return sqlquery.Query{}, err
	}
	vector := strings.TrimSpace(req.Mapping.Vector)
	if vector == "" {
		return sqlquery.Query{}, errors.New("postgres: tsvector expression is required")
	}
	if req.CandidateLimit <= 0 {
		return sqlquery.Query{}, errors.New("postgres: candidate limit must be positive")
	}
	config := req.Config
	if config == "" {
		config = "simple"
	}
	if err := checkTextSearchConfig(config); err != nil {
		return sqlquery.Query{}, err
	}
	rank, err := rankFunc(req.Rank)
	if err != nil {
		return sqlquery.Query{}, err
	}
	constructor, err := tsQueryFunc(req.Query, req.Text, req.TSQuery)
	if err != nil {
		return sqlquery.Query{}, err
	}
	if req.RevisionColumn != "" {
		if err := checkIdentifier("revision column", req.RevisionColumn); err != nil {
			return sqlquery.Query{}, err
		}
	}
	aliases := map[string]bool{"doc_key": true, "score": true, "revision": true}
	for _, col := range req.ExtraSourceCols {
		if err := checkIdentifier("extra source column", col.Name); err != nil {
			return sqlquery.Query{}, err
		}
		if err := checkIdentifier("extra source alias", col.As); err != nil {
			return sqlquery.Query{}, err
		}
		if aliases[strings.ToLower(col.As)] {
			return sqlquery.Query{}, fmt.Errorf("postgres: conflicting extra source column %q", col.As)
		}
		aliases[strings.ToLower(col.As)] = true
	}
	predicate := strings.TrimSpace(req.SourcePredicate.SQL)
	if predicate == "" && len(req.SourcePredicate.Args) != 0 {
		return sqlquery.Query{}, errors.New("postgres: source predicate args require SQL")
	}
	if strings.TrimSpace(req.TSQuery) == "" && len(req.TSQueryArgs) != 0 {
		return sqlquery.Query{}, errors.New("postgres: tsquery args require SQL")
	}

	projection := []string{"d." + quote(req.Mapping.SourceKey) + " AS doc_key"}
	if req.RevisionColumn != "" {
		projection = append(projection, "d."+quote(req.RevisionColumn)+" AS revision")
	}
	projection = append(projection, rank+"("+vector+", q.tsq) AS score")
	for _, col := range req.ExtraSourceCols {
		projection = append(projection, "d."+quote(col.Name)+" AS "+quote(col.As))
	}

	var querySQL string
	var n int
	var args []any
	if strings.TrimSpace(req.TSQuery) != "" {
		var last int
		querySQL, last = rebase(req.TSQuery, 0)
		if err := checkArgs("tsquery", last, len(req.TSQueryArgs)); err != nil {
			return sqlquery.Query{}, err
		}
		n = last
		args = append(args, req.TSQueryArgs...)
	} else {
		querySQL = constructor + "($1::regconfig, $2)"
		n = 2
		args = append(args, config, req.Text)
	}
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
	fmt.Fprintf(&b, "SELECT %s FROM %s AS d CROSS JOIN (SELECT %s AS tsq) AS q WHERE %s @@ q.tsq",
		strings.Join(projection, ", "), quote(req.Mapping.SourceTable), querySQL, vector)
	if predSQL != "" {
		b.WriteString(" AND (")
		b.WriteString(predSQL)
		b.WriteString(")")
	}
	fmt.Fprintf(&b, " ORDER BY score DESC, doc_key ASC LIMIT $%d", n)
	return sqlquery.Query{SQL: b.String(), Args: args}, nil
}

func checkTextSearchConfig(value string) error {
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return fmt.Errorf("postgres: invalid text search config %q", value)
	}
	for _, part := range parts {
		if !validIdentifier(part) {
			return fmt.Errorf("postgres: invalid text search config %q", value)
		}
	}
	return nil
}

func rankFunc(rank Rank) (string, error) {
	switch rank {
	case "", RankDefault:
		return "ts_rank", nil
	case RankCover:
		return "ts_rank_cd", nil
	default:
		return "", fmt.Errorf("postgres: unknown rank %q", rank)
	}
}

func tsQueryFunc(mode TextQuery, text, tsquery string) (string, error) {
	if strings.TrimSpace(tsquery) != "" {
		if strings.TrimSpace(text) != "" || mode != "" {
			return "", errors.New("postgres: set text or tsquery, not both")
		}
		return "", nil
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("postgres: query text is empty")
	}
	switch mode {
	case "", QueryPlain:
		return "plainto_tsquery", nil
	case QueryPhrase:
		return "phraseto_tsquery", nil
	case QueryWeb:
		return "websearch_to_tsquery", nil
	default:
		return "", fmt.Errorf("postgres: unknown text query %q", mode)
	}
}
