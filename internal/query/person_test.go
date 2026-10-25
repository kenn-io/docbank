package query

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPersonNameStartsWithWord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, prefix string
		matches      bool
	}{
		{"mary_jane", "jane", true},
		{"jo o’brien", "brien", true},
		{"李·明", "明", true},
		{"ájane", "jane", false},
		{"2jane", "jane", false},
		{"john smith", "mith", false},
		{"john smith", "", true},
	} {
		require.Equal(t, tc.matches, PersonNameStartsWithWord(tc.name, tc.prefix), "%q / %q", tc.name, tc.prefix)
	}
}
