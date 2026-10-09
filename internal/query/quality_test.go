package query

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeQualityOperand(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{"0": "0", "0.70": "0.7", "1.0": "1", "00.5": "0.5"} {
		got, err := NormalizeQualityOperand(value)
		require.NoError(t, err, value)
		require.Equal(t, want, got)
	}
	for _, value := range []string{"", "abc", "1.5", "2", "-0", "-0.1", ".5", "0.5e1"} {
		_, err := NormalizeQualityOperand(value)
		require.EqualError(t, err, "quality score must be a decimal string within 0..1", value)
	}
}

func TestQualityUnevaluatedFalse(t *testing.T) {
	t.Parallel()
	for _, text := range []string{`unevaluated:false`, `unevaluated:"false"`, `unevaluated:(true OR false)`} {
		_, err := ParseExpression(text, "advanced")
		expressionErr := requireExpressionError(t, text, err)
		require.Contains(t, expressionErr.Message, "use focus_min:0")
		require.Contains(t, text[expressionErr.Offset:expressionErr.End], "false")
	}
	value, err := Parse([]byte(`{"filters":{"unevaluated":false}}`))
	require.NoError(t, err)
	omitted, err := Parse([]byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, omitted, value)
}
