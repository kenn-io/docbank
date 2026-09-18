package query

import "unicode/utf8"

// PositiveTextTerms returns the bounded literal text leaves that contributed
// positively to a resolved lexical expression. Structured fields, name-only
// operands, negation, and saved-reference labels are not document text.
func PositiveTextTerms(resolved ResolvedQuery) []string {
	terms := make([]string, 0, maxHighlightTerms)
	seen := make(map[string]struct{}, maxHighlightTerms)
	collectPositiveTextTerms(resolved.Expression, "", false, &terms, seen)
	return terms
}

func collectPositiveTextTerms(
	expression *ResolvedExpression,
	field string,
	negated bool,
	terms *[]string,
	seen map[string]struct{},
) {
	if expression == nil || expression.Syntax == nil || len(*terms) >= maxHighlightTerms {
		return
	}
	syntax := expression.Syntax
	switch syntax.Kind {
	case ExpressionField:
		if len(expression.Children) == 1 {
			collectPositiveTextTerms(expression.Children[0], syntax.Field, negated, terms, seen)
		}
	case ExpressionNot:
		if len(expression.Children) == 1 {
			collectPositiveTextTerms(expression.Children[0], field, !negated, terms, seen)
		}
	case ExpressionTerm, ExpressionPhrase:
		if negated {
			return
		}
		if field == string(ReferenceSaved) && expression.Saved != nil {
			collectPositiveTextTerms(expression.Saved.Expression, "", false, terms, seen)
			return
		}
		if field != "" || syntax.Value == "" || utf8.RuneCountInString(syntax.Value) > maxHighlightTermRunes {
			return
		}
		if _, duplicate := seen[syntax.Value]; duplicate {
			return
		}
		seen[syntax.Value] = struct{}{}
		*terms = append(*terms, syntax.Value)
	default:
		for _, child := range expression.Children {
			collectPositiveTextTerms(child, field, negated, terms, seen)
		}
	}
}
