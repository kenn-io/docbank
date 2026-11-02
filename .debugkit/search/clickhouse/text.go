package clickhouse

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/search/sqlquery"
)

// Tokenizer names reported by system.tokenizers on ClickHouse 26.2.
// asciiCJK is not one of them. The query does not embed the tokenizer;
// the column's text index supplies it.
const (
	TokenizerSplitByNonAlpha = "splitByNonAlpha"
	TokenizerSplitByString   = "splitByString"
	TokenizerNgrams          = "ngrams"
	TokenizerSparseGrams     = "sparseGrams"
	TokenizerArray           = "array"
)

// TextMatch selects the token predicate.
type TextMatch string

const (
	// MatchAll requires every token.
	MatchAll TextMatch = "all"
	// MatchAny requires one token.
	MatchAny TextMatch = "any"
	// MatchToken requires one exact token. Use it when the index tokenizer
	// emits the whole term, such as a CJK run under splitByNonAlpha.
	MatchToken TextMatch = "token"
	// MatchSubstring requires the query text to occur in order. A CJK run
	// stored as one splitByNonAlpha token still matches a shorter sequence
	// inside it. hasToken does not.
	MatchSubstring TextMatch = "substring"
)

// TextRequest is one bounded text candidate query. SourcePredicate is
// passed to ClickHouse unchanged, so each ? binds the next argument.
// Set Text or Tokens. Tokens are sent as an array and are not tokenized
// again. An empty token list is rejected. MatchToken accepts Text only.
type TextRequest struct {
	SourceTable     string
	SourceKey       string
	TextColumn      string
	Match           TextMatch
	Text            string
	Tokens          []string
	RevisionColumn  string
	SourcePredicate sqlquery.Predicate
	ExtraSourceCols []sqlquery.Column
	CandidateLimit  int
}

// TokenizersQuery reads system.tokenizers. Compare the names with the
// Tokenizer constants before creating an index. A missing name means that
// server cannot use that tokenizer.
func TokenizersQuery() sqlquery.Query {
	return sqlquery.Query{SQL: "SELECT name FROM system.tokenizers ORDER BY name"}
}

// BuildText returns a candidate SELECT. Columns are doc_key, revision when
// requested, then extra columns. Rows are ordered by doc_key. There is no
// native text rank column.
func BuildText(req TextRequest) (sqlquery.Query, error) {
	if err := checkIdentifier("source table", req.SourceTable); err != nil {
		return sqlquery.Query{}, err
	}
	if err := checkIdentifier("source key", req.SourceKey); err != nil {
		return sqlquery.Query{}, err
	}
	if err := checkIdentifier("text column", req.TextColumn); err != nil {
		return sqlquery.Query{}, err
	}
	predicateFn, err := textPredicate(req.Match, quote(req.TextColumn))
	if err != nil {
		return sqlquery.Query{}, err
	}
	if req.CandidateLimit <= 0 {
		return sqlquery.Query{}, errors.New("clickhouse: candidate limit must be positive")
	}
	useTokens := req.Tokens != nil
	if useTokens && strings.TrimSpace(req.Text) != "" {
		return sqlquery.Query{}, errors.New("clickhouse: set text or tokens, not both")
	}
	if useTokens && (req.Match == MatchToken || req.Match == MatchSubstring) {
		return sqlquery.Query{}, errors.New("clickhouse: this match requires text")
	}
	if useTokens && len(req.Tokens) == 0 {
		return sqlquery.Query{}, errors.New("clickhouse: token list is empty")
	}
	if !useTokens && strings.TrimSpace(req.Text) == "" {
		return sqlquery.Query{}, errors.New("clickhouse: query text is empty")
	}
	if req.RevisionColumn != "" {
		if err := checkIdentifier("revision column", req.RevisionColumn); err != nil {
			return sqlquery.Query{}, err
		}
	}
	aliases := map[string]bool{"doc_key": true, "revision": true}
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

	projection := []string{"d." + quote(req.SourceKey) + " AS doc_key"}
	if req.RevisionColumn != "" {
		projection = append(projection, "d."+quote(req.RevisionColumn)+" AS revision")
	}
	for _, col := range req.ExtraSourceCols {
		projection = append(projection, "d."+quote(col.Name)+" AS "+quote(col.As))
	}
	args := []any{}
	if useTokens {
		args = append(args, req.Tokens)
	} else {
		args = append(args, req.Text)
	}
	args = append(args, req.SourcePredicate.Args...)
	args = append(args, req.CandidateLimit)

	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s FROM %s AS d WHERE %s",
		strings.Join(projection, ", "), quote(req.SourceTable), predicateFn)
	if predicate != "" {
		b.WriteString(" AND (")
		b.WriteString(predicate)
		b.WriteString(")")
	}
	b.WriteString(" ORDER BY doc_key ASC LIMIT ?")
	return sqlquery.Query{SQL: b.String(), Args: args}, nil
}

func textPredicate(match TextMatch, column string) (string, error) {
	switch match {
	case "", MatchAll:
		return "hasAllTokens(d." + column + ", ?)", nil
	case MatchAny:
		return "hasAnyTokens(d." + column + ", ?)", nil
	case MatchToken:
		return "hasToken(d." + column + ", ?)", nil
	case MatchSubstring:
		return "positionUTF8(d." + column + ", ?) > 0", nil
	default:
		return "", fmt.Errorf("clickhouse: unknown text match %q", match)
	}
}
