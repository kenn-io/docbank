package ingest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchExclusionPatterns(t *testing.T) {
	for _, tc := range []struct {
		name, rel         string
		literal, patterns []string
		want              bool
	}{
		{"basename at depth", "a/b/x.swp", nil, []string{"*.swp"}, true},
		{"relative path", "a/x.swp", nil, []string{"a/*.swp"}, true},
		{"relative depth", "a/b/x.swp", nil, []string{"a/*.swp"}, false},
		{"literal star", "keep.tmp", []string{"*.tmp"}, nil, false},
		{"literal question", "keep.tmp", []string{"keep?tmp"}, nil, false},
		{"literal bracket", "[", []string{"["}, nil, true},
		{"literal subtree", "cache/file.txt", []string{"cache/"}, nil, true},
		{"combined literal", "cache/file.txt", []string{"cache/"}, []string{"*.swp"}, true},
		{"combined pattern", "a/x.swp", []string{"cache/"}, []string{"*.swp"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := compileExclusions(tc.literal, tc.patterns)
			require.NoError(t, err)
			rel := filepath.FromSlash(tc.rel)
			root := t.TempDir()
			assert.Equal(t, tc.want, e.match(root, filepath.Join(root, rel)))
			assert.Equal(t, tc.want, e.matchRelative(rel))
		})
	}
	for _, pattern := range []string{"[", `a\*.swp`} {
		_, err := compileExclusions(nil, []string{pattern})
		require.Error(t, err)
	}
}

func FuzzWatchExclusions(f *testing.F) {
	for _, seed := range [][3]string{{"a/b/x.swp", "cache/", "*.swp"}, {"cache/x.txt", "cache/", "*.swp"}, {"a/note.md", "note.md", "a/*.swp"}} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, rel, literal, pattern string) {
		if !validFuzzRelative(rel) || len(literal) > 80 || len(pattern) > 80 {
			t.Skip()
		}
		literals := strings.Split(literal, ",")
		patterns := strings.Split(pattern, ",")
		old, err := compileExclusions(literals, nil)
		if err != nil {
			t.Skip()
		}
		rules, err := compileSourceRules("exclude pattern", patterns)
		if err != nil {
			t.Skip()
		}
		combined, err := compileExclusions(literals, patterns)
		require.NoError(t, err)
		root := t.TempDir()
		native := filepath.FromSlash(rel)
		want := old.matchRelative(native) || (sourceSelection{exclude: rules}).excluded(root, filepath.Join(root, native))
		assert.Equal(t, want, combined.matchRelative(native))
		assert.Equal(t, want, combined.match(root, filepath.Join(root, native)))
	})
}
