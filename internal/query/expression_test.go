package query

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseExpressionSimplePreservesLiteralTermsAndByteSpans(t *testing.T) {
	expr, err := ParseExpression("  name:alpha\tOR βeta!  ", "simple")
	require.NoError(t, err)
	require.Equal(t, ExpressionAnd, expr.Kind)
	require.Len(t, expr.Children, 2)
	assert.Equal(t, ExpressionAnd, expr.Children[0].Kind)
	assert.Equal(t, &Expression{
		Kind: ExpressionTerm, Start: 2, End: 12, Value: "name:alpha", Prefix: true,
	}, expr.Children[0].Children[0])
	assert.Equal(t, &Expression{
		Kind: ExpressionTerm, Start: 13, End: 15, Value: "OR", Prefix: true,
	}, expr.Children[0].Children[1])
	assert.Equal(t, &Expression{
		Kind: ExpressionTerm, Start: 16, End: 22, Value: "βeta!", Prefix: true,
	}, expr.Children[1])
	assert.Equal(t, 2, expr.Start)
	assert.Equal(t, 22, expr.End)

	expr, err = ParseExpression(`"a:b" AND* \word`, "simple")
	require.NoError(t, err)
	assert.Equal(t, `"a:b"`, expr.Children[0].Children[0].Value)
	assert.Equal(t, `AND*`, expr.Children[0].Children[1].Value)
	assert.Equal(t, `\word`, expr.Children[1].Value)
	assert.True(t, expr.Children[0].Children[0].Prefix)
	assert.True(t, expr.Children[0].Children[1].Prefix)
	assert.True(t, expr.Children[1].Prefix)
}

func TestParseExpressionEmptyInputMatchesEverything(t *testing.T) {
	for _, syntax := range []string{"simple", "advanced"} {
		for _, text := range []string{"", " \t\n"} {
			expr, err := ParseExpression(text, syntax)
			require.NoError(t, err)
			assert.Equal(t, &Expression{Kind: ExpressionAll, Start: 0, End: len(text)}, expr)
		}
	}
}

func TestParseExpressionAdvancedPreservesFieldAndBooleanScope(t *testing.T) {
	text := `name:(alpha OR "beta gamma") AND NOT tag:closed`
	expr, err := ParseExpression(text, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionAnd, expr.Kind)
	require.Len(t, expr.Children, 2)

	name := expr.Children[0]
	require.Equal(t, ExpressionField, name.Kind)
	assert.Equal(t, "name", name.Field)
	assert.Equal(t, 0, name.Start)
	assert.Equal(t, 28, name.End)
	require.Len(t, name.Children, 1)
	assert.Equal(t, ExpressionOr, name.Children[0].Kind)
	assert.Equal(t, 5, name.Children[0].Start)
	assert.Equal(t, 28, name.Children[0].End)

	not := expr.Children[1]
	require.Equal(t, ExpressionNot, not.Kind)
	require.Len(t, not.Children, 1)
	assert.Equal(t, ExpressionField, not.Children[0].Kind)
	assert.Equal(t, "tag", not.Children[0].Field)
	assert.Equal(t, 33, not.Start)
	assert.Equal(t, len(text), not.End)
}

func TestParseExpressionAdvancedUsesDocumentedPrecedence(t *testing.T) {
	expr, err := ParseExpression(`a OR b AND c`, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionOr, expr.Kind)
	assert.Equal(t, ExpressionTerm, expr.Children[0].Kind)
	assert.Equal(t, ExpressionAnd, expr.Children[1].Kind)

	expr, err = ParseExpression(`a NOT b`, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionAnd, expr.Kind)
	assert.Equal(t, ExpressionTerm, expr.Children[0].Kind)
	assert.Equal(t, ExpressionNot, expr.Children[1].Kind)

	expr, err = ParseExpression(`and ORbit NEARby`, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionAnd, expr.Kind)
	assert.Equal(t, "NEARby", expr.Children[1].Value)
}

func TestParseExpressionAdvancedPhraseNearAndEscapes(t *testing.T) {
	expr, err := ParseExpression(`"alpha beta"* NEAR/5 gamma`, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionNear, expr.Kind)
	assert.Equal(t, 5, expr.Distance)
	assert.Equal(t, 0, expr.Start)
	assert.Equal(t, 26, expr.End)
	assert.Equal(t, &Expression{
		Kind: ExpressionPhrase, Start: 0, End: 13, Value: "alpha beta", Prefix: true,
	}, expr.Children[0])
	assert.Equal(t, &Expression{
		Kind: ExpressionTerm, Start: 21, End: 26, Value: "gamma",
	}, expr.Children[1])

	expr, err = ParseExpression(`foo\:bar \AND`, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionAnd, expr.Kind)
	assert.Equal(t, "foo:bar", expr.Children[0].Value)
	assert.Equal(t, "AND", expr.Children[1].Value)

	expr, err = ParseExpression(`alpha NEAR beta`, "advanced")
	require.NoError(t, err)
	assert.Equal(t, 10, expr.Distance)

	expr, err = ParseExpression(`alpha NEAR/0 beta`, "advanced")
	require.NoError(t, err)
	assert.Equal(t, 0, expr.Distance)
}

func TestParseExpressionAdvancedDecodesOnlyEscapedSyntax(t *testing.T) {
	tests := []struct {
		text   string
		kind   ExpressionKind
		value  string
		prefix bool
	}{
		{text: `alpha*`, kind: ExpressionTerm, value: "alpha", prefix: true},
		{text: `foo\*`, kind: ExpressionTerm, value: "foo*"},
		{text: `\NEAR\/5`, kind: ExpressionTerm, value: "NEAR/5"},
		{text: `"a  b\ c"`, kind: ExpressionPhrase, value: "a  b c"},
		{text: `"alpha"*`, kind: ExpressionPhrase, value: "alpha", prefix: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.text, func(t *testing.T) {
			expr, err := ParseExpression(testCase.text, "advanced")
			require.NoError(t, err)
			assert.Equal(t, testCase.kind, expr.Kind)
			assert.Equal(t, testCase.value, expr.Value)
			assert.Equal(t, testCase.prefix, expr.Prefix)
			assert.Equal(t, 0, expr.Start)
			assert.Equal(t, len(testCase.text), expr.End)
		})
	}
}

func TestParseExpressionAdvancedRecognizesExactFieldNames(t *testing.T) {
	fields := []string{
		"name", "path", "tag", "collection", "saved", "mime", "extension",
		"media_family", "modified_after", "modified_before", "size_min", "size_max",
		"text_coverage", "has_duplicates",
	}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			text := field + ":value"
			expr, err := ParseExpression(text, "advanced")
			require.NoError(t, err)
			assert.Equal(t, ExpressionField, expr.Kind)
			assert.Equal(t, field, expr.Field)
			assert.Equal(t, 0, expr.Start)
			assert.Equal(t, len(text), expr.End)
		})
	}

	for _, text := range []string{`Name:value`, `unknown:value`, `unknown\:value`} {
		expr, err := ParseExpression(text, "advanced")
		if text == `unknown\:value` {
			require.NoError(t, err)
			assert.Equal(t, "unknown:value", expr.Value)
			continue
		}
		_ = requireExpressionError(t, text, err)
	}
}

func TestParseExpressionAdvancedRetainsSeparateFieldScopes(t *testing.T) {
	expr, err := ParseExpression(`name:alpha OR tag:closed`, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionOr, expr.Kind)
	assert.Equal(t, "name", expr.Children[0].Field)
	assert.Equal(t, "tag", expr.Children[1].Field)

	expr, err = ParseExpression(`saved:"Case review"`, "advanced")
	require.NoError(t, err)
	require.Equal(t, ExpressionField, expr.Kind)
	assert.Equal(t, "saved", expr.Field)
	require.Len(t, expr.Children, 1)
	assert.Equal(t, ExpressionPhrase, expr.Children[0].Kind)
	assert.Equal(t, "Case review", expr.Children[0].Value)
}

func TestParseExpressionRejectsMalformedAdvancedSyntax(t *testing.T) {
	tests := map[string]string{
		"unknown field":         `unknown:value`,
		"dangling escape":       `alpha\`,
		"unclosed phrase":       `"alpha`,
		"embedded star":         `al*pha`,
		"repeated star":         `alpha**`,
		"phrase embedded star":  `"alpha"*beta`,
		"bare star":             `*`,
		"empty group":           `()`,
		"unmatched open group":  `(alpha`,
		"unmatched close group": `alpha)`,
		"missing field operand": `name:`,
		"nested field operand":  `name:tag:closed`,
		"leading operator":      `OR alpha`,
		"trailing operator":     `alpha AND`,
		"missing near operand":  `alpha NEAR`,
		"negative near":         `alpha NEAR/-1 beta`,
		"signed near":           `alpha NEAR/+1 beta`,
		"invalid near":          `alpha NEAR/x beta`,
		"large near":            `alpha NEAR/1001 beta`,
	}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseExpression(text, "advanced")
			_ = requireExpressionError(t, text, err)
		})
	}
}

func TestParseExpressionReportsUTF8ByteOffsets(t *testing.T) {
	text := `😀 alpha AND`
	_, err := ParseExpression(text, "advanced")
	expressionErr := requireExpressionError(t, text, err)
	assert.Equal(t, 11, expressionErr.Offset)
	assert.Equal(t, 14, expressionErr.End)
}

func TestParseExpressionEnforcesInputDepthAndNodeBounds(t *testing.T) {
	_, err := ParseExpression(string([]byte{0xff}), "advanced")
	_ = requireExpressionError(t, string([]byte{0xff}), err)

	_, err = ParseExpression(strings.Repeat("😀", maxTextRunes+1), "advanced")
	_ = requireExpressionError(t, strings.Repeat("😀", maxTextRunes+1), err)

	_, err = ParseExpression(strings.Repeat("(", 33)+"x"+strings.Repeat(")", 33), "advanced")
	_ = requireExpressionError(t, strings.Repeat("(", 33)+"x"+strings.Repeat(")", 33), err)

	_, err = ParseExpression(strings.Repeat("x ", 257), "advanced")
	_ = requireExpressionError(t, strings.Repeat("x ", 257), err)

	_, err = ParseExpression("x", "future")
	_ = requireExpressionError(t, "x", err)
}

func TestParseExpressionAcceptsExactInputDepthAndNodeBounds(t *testing.T) {
	text := strings.Repeat("x", maxTextRunes)
	expr, err := ParseExpression(text, "advanced")
	require.NoError(t, err)
	assert.Equal(t, len(text), expr.End)

	text = strings.Repeat("(", 32) + "x" + strings.Repeat(")", 32)
	expr, err = ParseExpression(text, "advanced")
	require.NoError(t, err)
	assert.Equal(t, 0, expr.Start)
	assert.Equal(t, len(text), expr.End)

	text = strings.Repeat("x ", 256)
	expr, err = ParseExpression(text, "advanced")
	require.NoError(t, err)
	assert.Equal(t, ExpressionAnd, expr.Kind)
}

func FuzzParseExpression(f *testing.F) {
	for _, seed := range []string{"", `name:(alpha OR "beta gamma")`, `😀 alpha AND`, `foo\:bar`, `alpha**`} {
		f.Add(seed, "advanced")
	}
	f.Add("name:alpha OR beta", "simple")
	f.Fuzz(func(t *testing.T, text, syntax string) {
		expr, err := ParseExpression(text, syntax)
		if err != nil {
			var expressionErr *ExpressionError
			require.ErrorAs(t, err, &expressionErr)
			assert.GreaterOrEqual(t, expressionErr.Offset, 0)
			assert.GreaterOrEqual(t, expressionErr.End, expressionErr.Offset)
			assert.LessOrEqual(t, expressionErr.End, len(text))
			return
		}
		assertExpressionSpans(t, expr, len(text))
	})
}

func requireExpressionError(t *testing.T, text string, err error) *ExpressionError {
	t.Helper()
	require.Error(t, err)
	var expressionErr *ExpressionError
	require.ErrorAs(t, err, &expressionErr, "error type: %T", err)
	assert.GreaterOrEqual(t, expressionErr.Offset, 0)
	assert.GreaterOrEqual(t, expressionErr.End, expressionErr.Offset)
	assert.LessOrEqual(t, expressionErr.End, len(text))
	return expressionErr
}

func assertExpressionSpans(t *testing.T, expr *Expression, textLen int) {
	t.Helper()
	require.NotNil(t, expr)
	assert.GreaterOrEqual(t, expr.Start, 0)
	assert.GreaterOrEqual(t, expr.End, expr.Start)
	assert.LessOrEqual(t, expr.End, textLen)
	for _, child := range expr.Children {
		assertExpressionSpans(t, child, textLen)
	}
}
