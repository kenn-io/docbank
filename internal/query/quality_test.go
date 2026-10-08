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
