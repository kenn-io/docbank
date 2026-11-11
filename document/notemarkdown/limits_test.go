package notemarkdown

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLimitsConfiguration(t *testing.T) {
	defaults, err := normalizeLimits(Limits{})
	require.NoError(t, err)
	require.Equal(t, Limits{4 << 20, 4096, 10000, 64, 256, 1024, 32}, defaults)
	for i := 0; i < reflect.TypeFor[Limits]().NumField(); i++ {
		for _, value := range []int{-1, reflect.ValueOf(defaults).Field(i).Interface().(int) + 1} {
			l := Limits{}
			reflect.ValueOf(&l).Elem().Field(i).SetInt(int64(value))
			n, err := Parse(t.Context(), []byte("# body"), l)
			require.ErrorIs(t, err, ErrInvalidInput)
			require.Equal(t, Note{}, n)
		}
	}
}

func TestPublicLimitsInclusiveAndZeroNote(t *testing.T) {
	keys := func(n int) string {
		var s strings.Builder
		for i := range n {
			fmt.Fprintf(&s, "k%d: v\n", i)
		}
		return s.String()
	}
	for _, tc := range []struct {
		name, at, over, resource string
		limits                   Limits
	}{
		{"bytes", "abcd", "abcde", "input_bytes", Limits{MaxBytes: 4}},
		{"headings", "# A\n# B\n", "# A\n# B\n# C\n", "headings", Limits{MaxHeadings: 2}},
		{"links", "[[a]] [[b]]", "[[a]] [[b]] [[c]]", "links", Limits{MaxLinks: 2}},
		{"keys", "---\n" + keys(2) + "---", "---\n" + keys(3) + "---", "frontmatter_keys", Limits{MaxFrontmatterKeys: 2}},
		{"values", "---\na: [a,b]\n---", "---\na: [a,b,c]\n---", "frontmatter_values", Limits{MaxValuesPerKey: 2}},
		{"value bytes", "---\na: abcd\n---", "---\na: abcde\n---", "frontmatter_value_bytes", Limits{MaxValueBytes: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(t.Context(), []byte(tc.at), tc.limits)
			require.NoError(t, err)
			n, err := Parse(t.Context(), []byte(tc.over), tc.limits)
			requireLimit(t, err, tc.resource)
			require.Equal(t, Note{}, n)
		})
	}
	for _, tc := range []struct{ src, resource string }{
		{strings.Repeat("a", maxInput+1), "input_bytes"},
		{strings.Repeat("# H\n", maxHeadings+1), "headings"},
		{strings.Repeat("[[x]] ", maxLinks+1), "links"},
		{"---\n" + keys(maxKeys+1) + "---", "frontmatter_keys"},
		{"---\na: [" + strings.Repeat("v,", maxMetadata) + "v]\n---", "frontmatter_values"},
		{"---\na: " + strings.Repeat("v", maxValue+1) + "\n---", "frontmatter_value_bytes"},
	} {
		n, err := Parse(t.Context(), []byte(tc.src), Limits{})
		requireLimit(t, err, tc.resource)
		require.Equal(t, Note{}, n)
	}
}

func TestInternalLimits(t *testing.T) {
	value := math.MaxInt - 1
	requireLimit(t, addLimit("overflow", &value, math.MaxInt, math.MaxInt), "overflow")
	for _, tc := range []struct{ src, resource string }{
		{"---\na: " + strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33) + "\n---", "yaml_depth"},
		{"---\n" + strings.Repeat("# comment\n", 7000) + "---", "frontmatter_bytes"},
		{strings.Repeat("\n", maxLines+1), "line_breaks"},
		{"[x](" + strings.Repeat("a", maxTarget+1) + ")", "target_bytes"},
	} {
		n, err := Parse(t.Context(), []byte(tc.src), Limits{})
		requireLimit(t, err, tc.resource)
		require.Equal(t, Note{}, n)
	}
}
