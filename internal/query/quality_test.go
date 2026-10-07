package query

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestQualityBoundsValidation(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"filters":{"focus_min":0.7}}`, `{"filters":{"focus_min":1}}`, `{"filters":{"focus_min":"-0"}}`, `{"filters":{"focus_min":"NaN"}}`, `{"filters":{"focus_min":"1.00000000000000001"}}`, `{"filters":{"focus_min":"0.9","focus_max":"0.8"}}`, `{"filters":{"focus_min":"0.70000000000000001","focus_max":"0.7"}}`, `{"filters":{"focus_median":"0.5"}}`} {
		_, err := Parse([]byte(raw))
		require.Error(t, err, raw)
	}
	q, err := Parse([]byte(`{"filters":{"focus_min":"00.700","focus_max":"1.0","unevaluated":null}}`))
	require.NoError(t, err)
	require.Equal(t, "0.7", *q.Filters.FocusMin)
	require.Equal(t, "1", *q.Filters.FocusMax)
}
