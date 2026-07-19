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
	assert.Equal(t, int64(1), tag.Revision)
	assert.Zero(t, tag.AssignmentCount)

	byName, err := s.TagByName(ctx, "café")
	require.NoError(t, err)
	assert.Equal(t, tag, byName)
	_, err = s.CreateTag(ctx, "café")
	require.ErrorIs(t, err, ErrExists)

	change, err := s.AssignTag(ctx, tag.ID, one.ID, one.Revision)
	require.NoError(t, err)
	one = change.Node
	assert.True(t, change.Changed)
	assert.Equal(t, int64(2), one.Revision)
	assert.Equal(t, "/one", change.Path)
	assert.Equal(t, int64(2), change.Tag.Revision)
	assert.Equal(t, 1, change.Tag.AssignmentCount)

	change, err = s.AssignTag(ctx, tag.ID, one.ID, one.Revision)
	require.NoError(t, err)
	assert.False(t, change.Changed)
	assert.Equal(t, one.Revision, change.Node.Revision)
	assert.Equal(t, "/one", change.Path)
	assert.Equal(t, 1, change.Tag.AssignmentCount)

	change, err = s.AssignTag(ctx, tag.ID, two.ID, two.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)
	assert.False(t, change.Changed)
	change, err = s.AssignTag(ctx, tag.ID, two.ID, two.Revision)
	require.NoError(t, err)
	two = change.Node
	assert.True(t, change.Changed)
	assert.Equal(t, int64(3), change.Tag.Revision)

	renamed, err := s.RenameTag(ctx, tag.ID, change.Tag.Revision, "archive")
	require.NoError(t, err)
	assert.Equal(t, 2, renamed.AssignmentCount)
	assert.Equal(t, int64(4), renamed.Revision)
	oneAfterRename, err := s.NodeByID(ctx, one.ID)
	require.NoError(t, err)
	twoAfterRename, err := s.NodeByID(ctx, two.ID)
	require.NoError(t, err)
	assert.Equal(t, one.Revision+1, oneAfterRename.Revision)
	assert.Equal(t, two.Revision+1, twoAfterRename.Revision)

	change, err = s.UnassignTag(ctx, tag.ID, one.ID, oneAfterRename.Revision)
	require.NoError(t, err)
	one = change.Node
	assert.True(t, change.Changed)
	assert.Equal(t, 1, change.Tag.AssignmentCount)
	assert.Equal(t, int64(5), change.Tag.Revision)
	assert.Equal(t, oneAfterRename.Revision+1, one.Revision)

	deleted, err := s.DeleteTag(ctx, tag.ID, change.Tag.Revision)
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

func TestTagRenameAndDeleteRejectStaleRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	tag, err := s.CreateTag(ctx, "records")
	require.NoError(t, err)
	node, err := s.Mkdir(ctx, s.RootID(), "records")
	require.NoError(t, err)
	change, err := s.AssignTag(ctx, tag.ID, node.ID, node.Revision)
	require.NoError(t, err)

	_, err = s.RenameTag(ctx, tag.ID, tag.Revision, "archive")
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.DeleteTag(ctx, tag.ID, tag.Revision)
	require.ErrorIs(t, err, ErrStaleRevision)

	current, err := s.TagByID(ctx, tag.ID)
	require.NoError(t, err)
	assert.Equal(t, "records", current.Name)
	assert.Equal(t, change.Tag.Revision, current.Revision)
	assert.Equal(t, 1, current.AssignmentCount)
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
	change, err := s.AssignTag(ctx, first.ID, node.ID, node.Revision)
	require.NoError(t, err)
	node = change.Node
	assert.Equal(t, "/tagged", change.Path)
	change, err = s.AssignTag(ctx, second.ID, node.ID, node.Revision)
	require.NoError(t, err)
	node = change.Node
	node, _, err = s.Trash(ctx, node.ID, node.Revision)
	require.NoError(t, err)
	change, err = s.AssignTag(ctx, first.ID, node.ID, node.Revision)
	require.NoError(t, err)
	assert.False(t, change.Changed)
	assert.Empty(t, change.Path)
	node = change.Node

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
	change, err := s.AssignTag(ctx, tag.ID, root.ID, root.Revision)
	require.NoError(t, err)
	assert.Equal(t, root.Revision+1, change.Node.Revision)
	assert.Equal(t, "/", change.Path)
	assert.Equal(t, 1, change.Tag.AssignmentCount)
	assert.True(t, change.Changed)

	nodes, total, err := s.TaggedNodes(ctx, tag.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, nodes, 1)
	assert.Equal(t, "/", nodes[0].Path)
}

func TestTagAssignmentPathUsesCurrentTopology(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	left, err := s.Mkdir(ctx, s.RootID(), "left")
	require.NoError(t, err)
	right, err := s.Mkdir(ctx, s.RootID(), "right")
	require.NoError(t, err)
	leaf, err := s.Mkdir(ctx, left.ID, "leaf")
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "topology")
	require.NoError(t, err)

	left, err = s.NodeByID(ctx, left.ID)
	require.NoError(t, err)
	_, _, err = s.Move(ctx, left.ID, right.ID, "moved", left.Revision)
	require.NoError(t, err)
	unchangedLeaf, err := s.NodeByID(ctx, leaf.ID)
	require.NoError(t, err)
	assert.Equal(t, leaf.Revision, unchangedLeaf.Revision,
		"moving an ancestor must not be mistaken for a descendant revision change")

	_, err = s.AssignTagPath(ctx, tag.ID, "/left/leaf")
	require.ErrorIs(t, err, ErrNotFound)
	change, err := s.AssignTagPath(ctx, tag.ID, "/right/moved/leaf")
	require.NoError(t, err)
	assert.Equal(t, leaf.ID, change.Node.ID)
	assert.Equal(t, "/right/moved/leaf", change.Path)
	assert.True(t, change.Changed)

	change, err = s.UnassignTagPath(ctx, tag.ID, "/right/moved/leaf")
	require.NoError(t, err)
	assert.Equal(t, leaf.ID, change.Node.ID)
	assert.Equal(t, "/right/moved/leaf", change.Path)
	assert.True(t, change.Changed)
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

func TestNodeTagsHasReverseLookupIndex(t *testing.T) {
	s := newTestStore(t)
	var definition string
	require.NoError(t, s.db.QueryRow(`
		SELECT sql FROM sqlite_master WHERE type='index' AND name='node_tags_tag'
	`).Scan(&definition))
	assert.Contains(t, definition, "node_tags(tag_id)")
}
