package store

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestContentMapDeltaExplainsVersionOrderAndUnavailable(t *testing.T) {
	entry := func(uid, version string) ContentMapSnapshotEntry {
		return ContentMapSnapshotEntry{DocumentUID: uid, Availability: mapAvailabilityAvailable,
			Member: SnapshotMember{ContentVersionID: version}}
	}
	before := ContentMapSnapshot{ID: "before", MapID: "map", MapRevision: 1,
		Sections: []ContentMapSnapshotSection{{ID: "core", Entries: []ContentMapSnapshotEntry{
			entry("a", "v1"), entry("b", "v1"), entry("c", "v1"), entry("gone", "v1"),
		}}}}
	after := ContentMapSnapshot{ID: "after", MapID: "map", MapRevision: 2,
		Sections: []ContentMapSnapshotSection{{ID: "core", Entries: []ContentMapSnapshotEntry{
			entry("b", "v2"), entry("a", "v1"), entry("new", "v1"),
			{DocumentUID: "c", Availability: mapAvailabilityUnavailable},
		}}}}
	delta := diffContentMapSnapshots(before, after)
	require.True(t, delta.Changed)
	require.True(t, delta.DefinitionChanged)
	require.Equal(t, []string{"new"}, deltaDocumentUIDs(delta.Added))
	require.Equal(t, []string{"gone"}, deltaDocumentUIDs(delta.Removed))
	require.Equal(t, []string{"b"}, deltaDocumentUIDs(delta.VersionChanged))
	require.Equal(t, []string{"b", "a"}, deltaDocumentUIDs(delta.Reordered))
	require.Equal(t, []string{"c"}, deltaDocumentUIDs(delta.Unavailable))
	require.Equal(t, 1, delta.Counts.VersionChanged)

	unchanged := diffContentMapSnapshots(after, after)
	require.False(t, unchanged.Changed)
	require.Empty(t, unchanged.Unavailable, "unchanged tombstones are not change events")
}

func TestContentMapDeltaBoundsListsButKeepsCounts(t *testing.T) {
	after := ContentMapSnapshot{ID: "after", MapID: "map", MapRevision: 1,
		Sections: []ContentMapSnapshotSection{{ID: "core"}}}
	for i := range MaxContentMapDeltaEntries + 1 {
		after.Sections[0].Entries = append(after.Sections[0].Entries, ContentMapSnapshotEntry{
			DocumentUID: fmt.Sprintf("id-%03d", i), Availability: mapAvailabilityAvailable,
			Member: SnapshotMember{ContentVersionID: "v1"},
		})
	}
	delta := diffContentMapSnapshots(ContentMapSnapshot{}, after)
	require.Len(t, delta.Added, MaxContentMapDeltaEntries)
	require.Equal(t, MaxContentMapDeltaEntries+1, delta.Counts.Added)
	require.True(t, delta.Truncated)
}

func TestContentMapDeltaReportsRemovedUnavailablePin(t *testing.T) {
	before := ContentMapSnapshot{ID: "before", MapID: "map", Sections: []ContentMapSnapshotSection{{
		ID: "core", Entries: []ContentMapSnapshotEntry{{DocumentUID: "pinned",
			Availability: mapAvailabilityUnavailable}},
	}}}
	after := ContentMapSnapshot{ID: "after", MapID: "map", Sections: []ContentMapSnapshotSection{{ID: "core"}}}
	delta := diffContentMapSnapshots(before, after)
	require.Equal(t, []string{"pinned"}, deltaDocumentUIDs(delta.Removed))
	require.True(t, delta.Changed)
}

func TestContentMapDeltaDistinguishesPassagesFromOneDocument(t *testing.T) {
	const documentUID = "11111111-1111-4111-8111-111111111111"
	const versionID = "22222222-2222-4222-8222-222222222222"
	first := document.PassageRefV1{Version: 1,
		VaultUID: "33333333-3333-4333-8333-333333333333", DocumentUID: documentUID,
		ContentVersionID: versionID, SourceSHA256: strings.Repeat("a", 64),
		RenditionBuildID: strings.Repeat("b", 64), AttachmentID: strings.Repeat("c", 64),
		BodySHA256: strings.Repeat("d", 64), ByteStart: 0, ByteEnd: 1,
		QuoteSHA256: strings.Repeat("e", 64)}
	second := first
	second.ByteStart, second.ByteEnd = 2, 3
	second.QuoteSHA256 = strings.Repeat("f", 64)
	before := ContentMapSnapshot{ID: "before", MapID: "map", Sections: []ContentMapSnapshotSection{{
		ID: "core", Entries: []ContentMapSnapshotEntry{
			{DocumentUID: documentUID, Availability: mapAvailabilityAvailable,
				Member: SnapshotMember{ContentVersionID: versionID}, Passage: &first},
			{DocumentUID: documentUID, Availability: mapAvailabilityAvailable,
				Member: SnapshotMember{ContentVersionID: versionID}, Passage: &second},
		},
	}}}
	after := ContentMapSnapshot{ID: "after", MapID: "map", Sections: []ContentMapSnapshotSection{{
		ID: "core", Entries: []ContentMapSnapshotEntry{{DocumentUID: documentUID,
			Availability: mapAvailabilityAvailable,
			Member:       SnapshotMember{ContentVersionID: versionID}, Passage: &second}},
	}}}
	delta := diffContentMapSnapshots(before, after)
	require.True(t, delta.Changed)
	require.Equal(t, 1, delta.Counts.Removed, "removing one of two passage pins must not disappear")
	require.Len(t, delta.Removed, 1)
	firstID, err := document.PassageIdentityV1(first)
	require.NoError(t, err)
	encoded, err := json.Marshal(delta)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"passage_id":"`+firstID+`"`,
		"the removed passage must be identifiable in the bounded delta")
}

func deltaDocumentUIDs(changes []ContentMapDeltaChange) []string {
	result := make([]string, 0, len(changes))
	for _, change := range changes {
		result = append(result, change.DocumentUID)
	}
	return result
}
