package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTagRarityWeight(t *testing.T) {
	if TagRarityWeight(100, 1) <= TagRarityWeight(100, 100) {
		t.Fatal("broad tag outranks rare")
	}
	if TagRarityWeight(0, 0) != 0 || TagRarityWeight(10, 11) != 0 {
		t.Fatal("invalid scope stats")
	}
}

func TestTagNeighborhoodUsesOnlyDirectAssignmentsInsideExactScope(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seed := createTagGraphFile(t, s, "seed.txt", "31")
	rarePeer := createTagGraphFile(t, s, "rare-peer.txt", "32")
	broadPeer := createTagGraphFile(t, s, "broad-peer.txt", "33")
	outside := createTagGraphFile(t, s, "outside.txt", "34")
	untagged := createTagGraphFile(t, s, "untagged.txt", "35")

	universal, err := s.CreateTag(ctx, "universal")
	require.NoError(t, err)
	rare, err := s.CreateTag(ctx, "rare")
	require.NoError(t, err)
	private, err := s.CreateTag(ctx, "private-only")
	require.NoError(t, err)
	folderOnly, err := s.CreateTag(ctx, "folder-only")
	require.NoError(t, err)

	for _, node := range []*Node{&seed, &rarePeer, &broadPeer} {
		change, assignErr := s.AssignTag(ctx, universal.ID, node.ID, node.Revision)
		require.NoError(t, assignErr)
		*node = change.Node
	}
	for _, node := range []*Node{&seed, &rarePeer} {
		change, assignErr := s.AssignTag(ctx, rare.ID, node.ID, node.Revision)
		require.NoError(t, assignErr)
		*node = change.Node
	}
	_, err = s.AssignTag(ctx, private.ID, outside.ID, outside.Revision)
	require.NoError(t, err)
	folder, err := s.Mkdir(ctx, s.RootID(), "folder")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, folderOnly.ID, folder.ID, folder.Revision)
	require.NoError(t, err)

	result, err := s.TagNeighborhood(ctx, TagNeighborhoodRequest{
		Fence: TagGraphFence{VaultUID: s.VaultID(), ContentVersionIDs: []string{
			seed.CurrentVersionID, rarePeer.CurrentVersionID, broadPeer.CurrentVersionID,
			untagged.CurrentVersionID,
		}},
		Seed: TagGraphSeed{NodeID: seed.ID}, AssignmentKinds: []string{TagAssignmentDocument},
		Limit: 20, MaxHops: 10, MaxVisited: 1000,
	})
	require.NoError(t, err)
	assert.Equal(t, 4, result.DocumentCount)
	assert.Equal(t, 1, result.UntaggedDocumentCount)
	assert.False(t, result.Truncated)
	require.Len(t, result.Documents, 2)
	assert.Equal(t, rarePeer.ID, result.Documents[0].NodeID)
	assert.Equal(t, broadPeer.ID, result.Documents[1].NodeID)
	assert.Greater(t, result.Documents[0].Score, result.Documents[1].Score)
	assert.ElementsMatch(t, []string{rare.ID, universal.ID}, tagGraphIDs(result.Documents[0].SharedTags))
	assert.Equal(t, []string{universal.ID}, tagGraphIDs(result.Documents[1].SharedTags))
	require.Len(t, result.Documents[0].Path, 3)
	assert.Equal(t, TagGraphPathStep{Kind: TagGraphNodeDocument, NodeID: seed.ID}, result.Documents[0].Path[0])
	assert.Equal(t, TagGraphNodeTag, result.Documents[0].Path[1].Kind)
	assert.Contains(t, []string{rare.ID, universal.ID}, result.Documents[0].Path[1].TagID)
	assert.Equal(t, TagGraphPathStep{Kind: TagGraphNodeDocument, NodeID: rarePeer.ID}, result.Documents[0].Path[2])
	assert.NotContains(t, tagGraphIDs(result.Tags), private.ID)
	assert.NotContains(t, tagGraphIDs(result.Tags), folderOnly.ID,
		"folder assignments are not inherited by document descendants")
	for _, tag := range result.Tags {
		assert.NotEqual(t, "private-only", tag.Name)
		assert.Contains(t, []int{2, 3}, tag.ScopedDocumentCount)
		assert.Equal(t, TagAssignmentOriginLegacy, tag.AssignmentOrigin)
	}
}

func TestTagNeighborhoodHandlesEmptyUntaggedRenameDeleteAndTrash(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seed := createTagGraphFile(t, s, "seed.txt", "41")
	peer := createTagGraphFile(t, s, "peer.txt", "42")
	tag, err := s.CreateTag(ctx, "before")
	require.NoError(t, err)

	empty, err := s.TagNeighborhood(ctx, TagNeighborhoodRequest{
		Fence: TagGraphFence{VaultUID: s.VaultID()}, Seed: TagGraphSeed{TagID: tag.ID},
	})
	require.NoError(t, err)
	assert.Zero(t, empty.DocumentCount)
	assert.Empty(t, empty.Documents)

	untagged, err := s.TagNeighborhood(ctx, TagNeighborhoodRequest{
		Fence: TagGraphFence{VaultUID: s.VaultID(), ContentVersionIDs: []string{seed.CurrentVersionID}},
		Seed:  TagGraphSeed{NodeID: seed.ID},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, untagged.UntaggedDocumentCount)
	assert.Empty(t, untagged.Documents)
	assert.Empty(t, untagged.Tags)

	change, err := s.AssignTag(ctx, tag.ID, seed.ID, seed.Revision)
	require.NoError(t, err)
	seed = change.Node
	change, err = s.AssignTag(ctx, tag.ID, peer.ID, peer.Revision)
	require.NoError(t, err)
	peer = change.Node
	tag, err = s.RenameTag(ctx, tag.ID, change.Tag.Revision, "after")
	require.NoError(t, err)
	seed, err = s.NodeByID(ctx, seed.ID)
	require.NoError(t, err)
	peer, err = s.NodeByID(ctx, peer.ID)
	require.NoError(t, err)

	request := TagNeighborhoodRequest{
		Fence: TagGraphFence{VaultUID: s.VaultID(), ContentVersionIDs: []string{
			seed.CurrentVersionID, peer.CurrentVersionID,
		}}, Seed: TagGraphSeed{NodeID: seed.ID},
	}
	renamed, err := s.TagNeighborhood(ctx, request)
	require.NoError(t, err)
	require.Len(t, renamed.Documents, 1)
	require.Len(t, renamed.Documents[0].SharedTags, 1)
	assert.Equal(t, "after", renamed.Documents[0].SharedTags[0].Name)

	_, err = s.DeleteTag(ctx, tag.ID, tag.Revision)
	require.NoError(t, err)
	deleted, err := s.TagNeighborhood(ctx, request)
	require.NoError(t, err)
	assert.Empty(t, deleted.Documents)
	assert.Empty(t, deleted.Tags)

	peer, err = s.NodeByID(ctx, peer.ID)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, peer.ID, peer.Revision)
	require.NoError(t, err)
	_, err = s.TagNeighborhood(ctx, request)
	require.ErrorIs(t, err, ErrProcessingSourceFenceStaleVersion)
}

func TestTagNeighborhoodBoundsCyclesAndHighDegreePostings(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	shared, err := s.CreateTag(ctx, "shared")
	require.NoError(t, err)
	secondary, err := s.CreateTag(ctx, "secondary")
	require.NoError(t, err)
	versions := make([]string, 0, 151)
	var seed Node
	for i := range 151 {
		node := createTagGraphFile(t, s, fmt.Sprintf("document-%03d.txt", i), fmt.Sprintf("5%02x", i))
		change, assignErr := s.AssignTag(ctx, shared.ID, node.ID, node.Revision)
		require.NoError(t, assignErr)
		node = change.Node
		if i < 2 {
			change, assignErr = s.AssignTag(ctx, secondary.ID, node.ID, node.Revision)
			require.NoError(t, assignErr)
			node = change.Node
		}
		if i == 0 {
			seed = node
		}
		versions = append(versions, node.CurrentVersionID)
	}

	result, err := s.TagNeighborhood(ctx, TagNeighborhoodRequest{
		Fence: TagGraphFence{VaultUID: s.VaultID(), ContentVersionIDs: versions},
		Seed:  TagGraphSeed{NodeID: seed.ID}, Limit: 100, MaxHops: 10, MaxVisited: 25,
	})
	require.NoError(t, err)
	assert.True(t, result.Truncated)
	assert.LessOrEqual(t, result.VisitedNodes, 25)
	assert.LessOrEqual(t, len(result.Documents)+len(result.Tags), 100)
	seen := map[int64]bool{}
	for _, document := range result.Documents {
		assert.False(t, seen[document.NodeID], "cycles must not repeat a document")
		seen[document.NodeID] = true
		assert.LessOrEqual(t, len(document.Path), 11)
	}
}

func TestTagNeighborhoodReturnsBoundedIndirectPathsWithoutInventingSharedTags(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seed := createTagGraphFile(t, s, "seed.txt", "61")
	bridge := createTagGraphFile(t, s, "bridge.txt", "62")
	indirect := createTagGraphFile(t, s, "indirect.txt", "63")
	first, err := s.CreateTag(ctx, "first")
	require.NoError(t, err)
	second, err := s.CreateTag(ctx, "second")
	require.NoError(t, err)
	for _, assignment := range []struct {
		tag  string
		node *Node
	}{
		{first.ID, &seed}, {first.ID, &bridge}, {second.ID, &bridge}, {second.ID, &indirect},
	} {
		change, assignErr := s.AssignTag(ctx, assignment.tag, assignment.node.ID, assignment.node.Revision)
		require.NoError(t, assignErr)
		*assignment.node = change.Node
	}
	result, err := s.TagNeighborhood(ctx, TagNeighborhoodRequest{
		Fence: TagGraphFence{VaultUID: s.VaultID(), ContentVersionIDs: []string{
			seed.CurrentVersionID, bridge.CurrentVersionID, indirect.CurrentVersionID,
		}},
		Seed: TagGraphSeed{NodeID: seed.ID}, Limit: 20, MaxHops: 4,
	})
	require.NoError(t, err)
	require.Len(t, result.Documents, 2)
	assert.Equal(t, bridge.ID, result.Documents[0].NodeID)
	assert.Positive(t, result.Documents[0].Score)
	assert.Equal(t, indirect.ID, result.Documents[1].NodeID)
	assert.Zero(t, result.Documents[1].Score)
	assert.Empty(t, result.Documents[1].SharedTags)
	require.Len(t, result.Documents[1].Path, 5)
}

func TestTagNeighborhoodRejectsInvalidAuthorityAndBounds(t *testing.T) {
	s := newTestStore(t)
	validTag, err := s.CreateTag(t.Context(), "valid")
	require.NoError(t, err)
	tests := map[string]TagNeighborhoodRequest{
		"foreign vault": {Fence: TagGraphFence{VaultUID: "00000000-0000-4000-8000-000000000000"}, Seed: TagGraphSeed{TagID: validTag.ID}},
		"two seeds":     {Fence: TagGraphFence{VaultUID: s.VaultID()}, Seed: TagGraphSeed{NodeID: 1, TagID: validTag.ID}},
		"passage":       {Fence: TagGraphFence{VaultUID: s.VaultID()}, Seed: TagGraphSeed{PassageID: "future"}},
		"assignment":    {Fence: TagGraphFence{VaultUID: s.VaultID()}, Seed: TagGraphSeed{TagID: validTag.ID}, AssignmentKinds: []string{"passage"}},
		"limit":         {Fence: TagGraphFence{VaultUID: s.VaultID()}, Seed: TagGraphSeed{TagID: validTag.ID}, Limit: 101},
		"hops":          {Fence: TagGraphFence{VaultUID: s.VaultID()}, Seed: TagGraphSeed{TagID: validTag.ID}, MaxHops: 11},
		"visited":       {Fence: TagGraphFence{VaultUID: s.VaultID()}, Seed: TagGraphSeed{TagID: validTag.ID}, MaxVisited: 1001},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			_, graphErr := s.TagNeighborhood(t.Context(), request)
			require.ErrorIs(t, graphErr, ErrInvalidTagGraph)
		})
	}
}

func createTagGraphFile(t *testing.T, s *Store, name, hashSeed string) Node {
	t.Helper()
	node, err := s.CreateFile(t.Context(), s.RootID(), name, fakeHash(hashSeed), 1, "text/plain",
		BlobPhysical{Encoding: "raw", StoredBytes: 1, PackEligible: true, Created: true})
	require.NoError(t, err)
	return node
}

func tagGraphIDs(items []TagGraphTag) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}
