package gitworktree

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePorcelain(t *testing.T) {
	assert := assert.New(t)
	output := "" +
		"worktree /repo\nHEAD abc123\nbranch refs/heads/main\n\n" +
		"worktree /repo/.wt/feature\nHEAD def456\nbranch refs/heads/feature/x\n\n" +
		"worktree /bare\nbare\n\n" +
		"worktree /repo/.wt/detached\nHEAD 9f9f9f9f9f9f9f\ndetached\n" +
		"prunable gitdir file points to non-existent location\n"

	entries := ParsePorcelain(output)
	require.Len(t, entries, 4)

	assert.Equal(PorcelainEntry{Path: "/repo", Head: "abc123", Branch: "main"}, entries[0])
	assert.Equal(PorcelainEntry{Path: "/repo/.wt/feature", Head: "def456", Branch: "feature/x"}, entries[1])
	assert.Equal(PorcelainEntry{Path: "/bare", Bare: true}, entries[2])
	assert.Equal(PorcelainEntry{
		Path: "/repo/.wt/detached", Head: "9f9f9f9f9f9f9f", Detached: true,
		Prunable: true, PrunableReason: "gitdir file points to non-existent location",
	}, entries[3])
}

func TestParsePorcelainIgnoresUnknownFieldsAndIncompleteBlocks(t *testing.T) {
	assert := assert.New(t)
	entries := ParsePorcelain("HEAD ignored\n\nworktree /repo\nlocked reason\ncustom value\n")

	require.Len(t, entries, 1)
	assert.Equal("/repo", entries[0].Path)
	assert.True(entries[0].Locked)
	assert.Equal("reason", entries[0].LockedReason)
}

func TestParsePorcelainHandlesCRLFAndQuotedPaths(t *testing.T) {
	assert := assert.New(t)
	entries := ParsePorcelain(
		"worktree /repo\r\nHEAD abc123\r\nbranch refs/heads/main\r\n\r\n" +
			"worktree \"/repo/quoted\\tpath\"\r\nHEAD def456\r\ndetached\r\n",
	)

	require.Len(t, entries, 2)
	assert.Equal("/repo", entries[0].Path)
	assert.Equal("/repo/quoted\tpath", entries[1].Path)
	assert.True(entries[1].Detached)
}
