package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTagLifecycleAndNodeRevisions(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	one, err := s.Mkdir(ctx, s.RootID(), "one")
	require.NoError(t, err)
	two, err := s.Mkdir(ctx, s.RootID(), "two")
	require.NoError(t, err)

	tag, err := s.CreateTag(ctx, "cafe\u0301")
	require.NoError(t, err)
	require.NoError(t, validateUUIDv4(tag.ID))
	assert.Equal(t, "café", tag.Name)
	assert.Zero(t, tag.AssignmentCount)

	byName, err := s.TagByName(ctx, "café")
	require.NoError(t, err)
	assert.Equal(t, tag, byName)
	_, err = s.CreateTag(ctx, "café")
	require.ErrorIs(t, err, ErrExists)

	one, assigned, changed, err := s.AssignTag(ctx, tag.ID, one.ID, one.Revision)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, int64(2), one.Revision)
	assert.Equal(t, 1, assigned.AssignmentCount)

	unchanged, assigned, changed, err := s.AssignTag(ctx, tag.ID, one.ID, one.Revision)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, one.Revision, unchanged.Revision)
	assert.Equal(t, 1, assigned.AssignmentCount)

	_, _, changed, err = s.AssignTag(ctx, tag.ID, two.ID, two.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.False(t, changed)
	two, _, changed, err = s.AssignTag(ctx, tag.ID, two.ID, two.Revision)
	require.NoError(t, err)
	assert.True(t, changed)

	renamed, err := s.RenameTag(ctx, tag.ID, "archive")
	require.NoError(t, err)
	assert.Equal(t, 2, renamed.AssignmentCount)
	oneAfterRename, err := s.NodeByID(ctx, one.ID)
	require.NoError(t, err)
	twoAfterRename, err := s.NodeByID(ctx, two.ID)
	require.NoError(t, err)
	assert.Equal(t, one.Revision+1, oneAfterRename.Revision)
	assert.Equal(t, two.Revision+1, twoAfterRename.Revision)

	one, assigned, changed, err = s.UnassignTag(ctx, tag.ID, one.ID, oneAfterRename.Revision)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 1, assigned.AssignmentCount)
	assert.Equal(t, oneAfterRename.Revision+1, one.Revision)

	deleted, err := s.DeleteTag(ctx, tag.ID)
	require.NoError(t, err)
	assert.Equal(t, "archive", deleted.Name)
	assert.Equal(t, 1, deleted.AssignmentCount)
	_, err = s.TagByID(ctx, tag.ID)
	require.ErrorIs(t, err, ErrNotFound)
	twoAfterDelete, err := s.NodeByID(ctx, two.ID)
	require.NoError(t, err)
	assert.Equal(t, twoAfterRename.Revision+1, twoAfterDelete.Revision)

	var assignments int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM node_tags`).Scan(&assignments))
	assert.Zero(t, assignments)
}

func TestTagQueriesAreBoundedAndIncludeTrashedNodes(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.CreateTag(ctx, "beta")
	require.NoError(t, err)
	second, err := s.CreateTag(ctx, "alpha")
	require.NoError(t, err)
	node, err := s.Mkdir(ctx, s.RootID(), "tagged")
	require.NoError(t, err)
	node, _, _, err = s.AssignTag(ctx, first.ID, node.ID, node.Revision)
	require.NoError(t, err)
	node, _, _, err = s.AssignTag(ctx, second.ID, node.ID, node.Revision)
	require.NoError(t, err)
	node, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)

	tags, total, err := s.Tags(ctx, 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, tags, 1)
	assert.Equal(t, "alpha", tags[0].Name)

	nodeTags, total, err := s.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total)
	require.Len(t, nodeTags, 2)
	assert.Equal(t, []string{"alpha", "beta"}, []string{nodeTags[0].Name, nodeTags[1].Name})

	nodes, total, err := s.TaggedNodes(ctx, first.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, nodes, 1)
	assert.Equal(t, node.ID, nodes[0].Node.ID)
	require.NotNil(t, nodes[0].Node.TrashedAt)
	assert.Empty(t, nodes[0].Path)

	_, _, err = s.NodeTags(ctx, 99999, 10, 0)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = s.TaggedNodes(ctx, "00000000-0000-4000-8000-000000000000", 10, 0)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = s.Tags(ctx, 0, 0)
	require.ErrorContains(t, err, "between 1 and")
}

func TestTaggedNodesReturnsRootPath(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	tag, err := s.CreateTag(ctx, "root")
	require.NoError(t, err)
	root, err := s.NodeByID(ctx, s.RootID())
	require.NoError(t, err)
	updatedRoot, assigned, changed, err := s.AssignTag(ctx, tag.ID, root.ID, root.Revision)
	require.NoError(t, err)
	assert.Equal(t, root.Revision+1, updatedRoot.Revision)
	assert.Equal(t, 1, assigned.AssignmentCount)
	assert.True(t, changed)

	nodes, total, err := s.TaggedNodes(ctx, tag.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, nodes, 1)
	assert.Equal(t, "/", nodes[0].Path)
}

func TestTagNameValidation(t *testing.T) {
	for name, input := range map[string]string{
		"empty":        "",
		"invalid utf8": string([]byte{0xff}),
		"nul":          "bad\x00tag",
		"newline":      "bad\ntag",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeTagName(input)
			require.ErrorIs(t, err, ErrInvalidTag)
		})
	}
}
