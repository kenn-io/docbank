package query

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQualityFilterPairs(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"focus", "blur", "brightness", "framing", "aesthetics", "color_red", "color_green", "color_blue"} {
		t.Run(field, func(t *testing.T) {
			value, err := Parse(fmt.Appendf(nil, `{"filters":{"%s_min":"00.50","%s_max":"1.0"}}`, field, field))
			require.NoError(t, err)
			for _, bound := range QualityBounds(value.Filters) {
				switch bound.Field {
				case field + "_min":
					require.Equal(t, "0.5", *bound.Value)
				case field + "_max":
					require.Equal(t, "1", *bound.Value)
				default:
					require.Nil(t, bound.Value)
				}
				require.True(t, IsQualityField(bound.Field))
			}
			_, err = Parse(fmt.Appendf(nil, `{"filters":{"%s_min":"0.8","%s_max":"0.7"}}`, field, field))
			require.ErrorContains(t, err, "quality minimum exceeds maximum")
		})
	}
}

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
	value, err := Parse([]byte(`{"filters":{"unevaluated":false}}`))
	require.NoError(t, err)
	omitted, err := Parse([]byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, omitted, value)
}
