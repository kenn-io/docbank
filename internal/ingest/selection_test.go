package ingest

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileSourceSelectionMatchesBasenamesAndRelativePaths(t *testing.T) {
	selection, err := compileSourceSelection(Options{Include: []string{"*.txt", "docs/*.md"}})
	require.NoError(t, err)
	root := filepath.FromSlash("/source")
	assert.True(t, selection.included(root, filepath.Join(root, "nested", "note.txt")))
	assert.True(t, selection.included(root, filepath.Join(root, "docs", "guide.md")))
	assert.False(t, selection.included(root, filepath.Join(root, "docs", "nested", "guide.md")))
	assert.False(t, selection.included(root, filepath.Join(root, "nested", "note.md")))
}

func TestSourceSelectionUsesPathMatchEscapingAndLimits(t *testing.T) {
	root := filepath.FromSlash("/source")
	escaped, err := compileSourceSelection(Options{Include: []string{"report[[]1].txt"}})
	require.NoError(t, err)
	assert.True(t, escaped.included(root, filepath.Join(root, "report[1].txt")))
	assert.False(t, escaped.included(root, filepath.Join(root, "report1.txt")))

	doubleStar, err := compileSourceSelection(Options{Include: []string{"**/*.txt"}})
	require.NoError(t, err)
	assert.False(t, doubleStar.included(root, filepath.Join(root, "file.txt")))
	assert.True(t, doubleStar.included(root, filepath.Join(root, "nested", "file.txt")))
	explicit, err := compileSourceSelection(Options{Include: []string{"docs/*.txt"}})
	require.NoError(t, err)
	assert.False(t, explicit.included(root, filepath.Join(root, "report.txt")),
		"path-form rules do not match an explicitly named file's empty relative path")
}

func TestSourceSelectionRequiresSlashSeparators(t *testing.T) {
	_, err := compileSourceSelection(Options{Include: []string{`project\cache`}})
	assert.Error(t, err)
}

func TestSourceSelectionRejectsUnsafeAndInvalidRules(t *testing.T) {
	for _, opts := range []Options{
		{Include: []string{""}},
		{Include: []string{"../outside"}},
		{Include: []string{"["}},
		{Exclude: []string{""}},
		{Exclude: []string{"inside/../outside"}},
		{Exclude: []string{"["}},
	} {
		assert.Error(t, ValidateOptions(opts))
	}
}

func TestSourceSelectionEmptyRulesPreserveDefaultBehavior(t *testing.T) {
	selection, err := compileSourceSelection(Options{Include: []string{}, Exclude: []string{}})
	require.NoError(t, err)
	root := filepath.FromSlash("/source")
	assert.True(t, selection.included(root, filepath.Join(root, "nested", "file.bin")))
	assert.False(t, selection.excluded(root, filepath.Join(root, "nested", "file.bin")))
}

func TestSourceSelectionExclusionWinsAndDoesNotPruneDirectoriesByInclude(t *testing.T) {
	selection, err := compileSourceSelection(Options{
		Include: []string{"*.txt"}, Exclude: []string{"secret.txt"},
	})
	require.NoError(t, err)
	root := filepath.FromSlash("/source")
	secret := filepath.Join(root, "nested", "secret.txt")
	keep := filepath.Join(root, "nested", "keep.txt")
	assert.True(t, selection.excluded(root, secret))
	assert.True(t, selection.included(root, secret))
	assert.False(t, selection.excluded(root, keep))
	assert.True(t, selection.included(root, keep))
	assert.False(t, selection.excluded(root, filepath.Join(root, "nested")))
}
