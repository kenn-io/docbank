package maps

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

const (
	testMapID      = "11111111-1111-4111-8111-111111111111"
	testSnapshotID = "22222222-2222-4222-8222-222222222222"
	testVersionID  = "33333333-3333-4333-8333-333333333333"
	testDocumentID = "44444444-4444-4444-8444-444444444444"
)

func TestRenderSnapshotBoundsAndEscapesSourceText(t *testing.T) {
	snapshot := store.ContentMapSnapshot{ID: testSnapshotID, MapID: testMapID,
		CreatedAt: "2026-09-24T12:00:00Z", ScopeDigest: "sha256:scope",
		Sections: []store.ContentMapSnapshotSection{{ID: "core", Heading: "Core <script>",
			Description: "Read [carefully](https://unsafe.example)", Entries: []store.ContentMapSnapshotEntry{
				{DocumentUID: testDocumentID, Availability: "available", Name: "A <script>\n# forged",
					Member:     store.SnapshotMember{NodeID: 7, ContentVersionID: testVersionID},
					ModifiedAt: "2026-09-20T00:00:00Z", Passage: &document.PassageRefV1{
						Version: 1, VaultUID: testMapID, DocumentUID: testDocumentID,
						ContentVersionID: testVersionID, SourceSHA256: strings.Repeat("a", 64),
						RenditionBuildID: strings.Repeat("b", 64), AttachmentID: strings.Repeat("c", 64),
						BodySHA256: strings.Repeat("d", 64), ByteStart: 4, ByteEnd: 10,
						QuoteSHA256: strings.Repeat("e", 64)}},
				{DocumentUID: "55555555-5555-4555-8555-555555555555", Availability: "unavailable",
					RequestedContentVersionID: "66666666-6666-4666-8666-666666666666", PinMode: "version-pinned"},
			}}}}
	first, err := RenderSnapshot(snapshot, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, first.TotalEntries)
	assert.Equal(t, 1, first.ReturnedEntries)
	assert.Equal(t, 2, first.NextOffset)
	assert.True(t, first.Truncated)
	assert.Contains(t, first.Markdown, testVersionID)
	assert.Contains(t, first.Markdown, strings.Repeat("a", 64), "Markdown must carry the exact source hash")
	assert.Contains(t, first.Markdown, strings.Repeat("b", 64), "Markdown must carry the exact rendition build")
	assert.Contains(t, first.Markdown, "Snapshot created: 2026-09-24T12:00:00Z")
	assert.NotContains(t, first.Markdown, "<script>")
	assert.NotContains(t, first.Markdown, "\n# forged")
	assert.NotContains(t, first.Markdown, "[carefully](https://unsafe.example)")
	assert.NotContains(t, first.Markdown, "2026-09-20", "source metadata is not verified chronology")
	encoded, err := json.Marshal(first)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "modified_at")
	assert.NotContains(t, string(encoded), "2026-09-20")
	second, err := RenderSnapshot(snapshot, first.NextOffset, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, second.NextOffset)
	assert.False(t, second.Truncated)
	assert.Contains(t, second.Markdown, "unavailable")
	assert.Contains(t, second.Markdown, "66666666-6666-4666-8666-666666666666")
	assert.NotContains(t, second.Markdown, "A &lt;script&gt;")
	_, err = RenderSnapshot(snapshot, -1, 1)
	require.Error(t, err)
	_, err = RenderSnapshot(snapshot, 0, 101)
	require.Error(t, err)
	assert.LessOrEqual(t, len(first.Markdown), MaxMapReadBytes)
	assert.NotContains(t, first.Markdown, "javascript:")
}

func TestRenderDefinitionShowsCuratedPinsWithoutInterpretingProse(t *testing.T) {
	record := store.ContentMap{ID: testMapID, Revision: 3, UpdatedAt: "2026-09-24T12:00:00Z",
		Definition: store.ContentMapDefinition{Title: "Topic <script>", Scope: "tag:research",
			Sections: []store.ContentMapSection{{ID: "core", Heading: "Core # heading",
				Description: "Review [this](https://unsafe.example)", Ordering: "explicit", MaxEntries: 10,
				Include: []document.ContentMapPin{{DocumentUID: testDocumentID, Mode: document.MapPinVersionPinned,
					ContentVersionID: testVersionID}}}}}}
	view, err := RenderDefinition(record, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, "definition", view.Kind)
	assert.Equal(t, 1, view.TotalEntries)
	assert.Equal(t, 1, view.ReturnedEntries)
	assert.Contains(t, view.Markdown, testDocumentID)
	assert.Contains(t, view.Markdown, testVersionID)
	assert.NotContains(t, view.Markdown, "<script>")
	assert.NotContains(t, view.Markdown, "[this](https://unsafe.example)")
	assert.Contains(t, view.Markdown, "Definition updated: 2026-09-24T12:00:00Z")
	assert.NotContains(t, view.Markdown, "Source date:")
}

func TestRenderDeltaKeepsFullCountsAndBoundsEventPage(t *testing.T) {
	delta := store.ContentMapDelta{MapID: testMapID, ScopeDigest: "sha256:scope",
		BeforeSnapshotID: "before", BeforeSnapshotCreated: "2026-09-23T12:00:00Z",
		AfterSnapshotID: "after", AfterSnapshotCreated: "2026-09-24T12:00:00Z",
		Changed: true, Truncated: true,
		Counts: store.ContentMapDeltaCounts{Added: 101, VersionChanged: 1},
		Added: []store.ContentMapDeltaChange{{SectionID: "core", DocumentUID: testDocumentID,
			BeforeIndex: -1, AfterIndex: 0, AfterContentVersionID: testVersionID}},
		VersionChanged: []store.ContentMapDeltaChange{{SectionID: "core", DocumentUID: "55555555-5555-4555-8555-555555555555",
			BeforeIndex: 0, AfterIndex: 1, BeforeContentVersionID: "old", AfterContentVersionID: "new"}}}
	first, err := RenderDelta(delta, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, 102, first.TotalEntries)
	assert.Equal(t, 2, first.AvailableEntries)
	assert.Equal(t, 1, first.ReturnedEntries)
	assert.Equal(t, 1, first.NextOffset)
	assert.True(t, first.Truncated)
	assert.Contains(t, first.Markdown, "101 added")
	assert.Equal(t, "sha256:scope", first.ScopeDigest)
	assert.Equal(t, delta.BeforeSnapshotCreated, first.BeforeSnapshotCreated)
	assert.Equal(t, delta.AfterSnapshotCreated, first.SnapshotCreated)
	assert.Contains(t, first.Markdown, "Before snapshot created: 2026-09-23T12:00:00Z")
	assert.Contains(t, first.Markdown, "After snapshot created: 2026-09-24T12:00:00Z")
	assert.Contains(t, first.Markdown, testVersionID)
	second, err := RenderDelta(delta, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, second.NextOffset)
	assert.True(t, second.Truncated, "the source delta itself omitted events")
	assert.Contains(t, second.Markdown, "version changed")
}

func TestRenderDeltaIdentifiesExactPassageChange(t *testing.T) {
	passageID := strings.Repeat("a", 64)
	delta := store.ContentMapDelta{MapID: testMapID, AfterSnapshotID: testSnapshotID,
		Counts: store.ContentMapDeltaCounts{Removed: 1},
		Removed: []store.ContentMapDeltaChange{{SectionID: "core", DocumentUID: testDocumentID,
			PassageID: passageID, BeforeIndex: 0, AfterIndex: -1}}}
	view, err := RenderDelta(delta, 0, 10)
	require.NoError(t, err)
	assert.Contains(t, view.Markdown, "passage `"+passageID+"`",
		"the readable delta must distinguish two pins from the same document")
}

func TestRenderDefinitionBoundsSectionsWithoutPins(t *testing.T) {
	record := store.ContentMap{ID: testMapID, Revision: 1, Definition: store.ContentMapDefinition{
		Title: "Synthetic", Scope: "local"}}
	for index := range 50 {
		record.Definition.Sections = append(record.Definition.Sections, store.ContentMapSection{
			ID: fmt.Sprintf("s%d", index), Heading: "Section", Description: strings.Repeat("界", 512)})
	}
	first, err := RenderDefinition(record, 0, 100)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(first.Markdown), MaxMapReadBytes)
	assert.True(t, first.Truncated)
	assert.Positive(t, first.NextOffset)
	assert.Equal(t, "section", first.OffsetUnit)
	second, err := RenderDefinition(record, first.NextOffset, 100)
	require.NoError(t, err)
	assert.NotEmpty(t, second.Sections)
	assert.LessOrEqual(t, len(second.Markdown), MaxMapReadBytes)
}

func TestRenderSnapshotShowsBoundedEmptySections(t *testing.T) {
	snapshot := store.ContentMapSnapshot{ID: testSnapshotID, MapID: testMapID}
	for index := range 50 {
		snapshot.Sections = append(snapshot.Sections, store.ContentMapSnapshotSection{
			ID: fmt.Sprintf("s%d", index), Heading: "Section", Description: strings.Repeat("界", 512)})
	}
	first, err := RenderSnapshot(snapshot, 0, 100)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(first.Markdown), MaxMapReadBytes)
	assert.NotEmpty(t, first.Sections)
	assert.True(t, first.Truncated)
	assert.Equal(t, "section", first.OffsetUnit)
	assert.Positive(t, first.NextOffset)
}

func TestRenderViewsKeepEmptySectionAlongsideMembers(t *testing.T) {
	definition := store.ContentMap{ID: testMapID, Definition: store.ContentMapDefinition{Title: "Synthetic", Scope: "local",
		Sections: []store.ContentMapSection{{ID: "intro", Heading: "Introduction"},
			{ID: "core", Heading: "Core", Include: []document.ContentMapPin{{DocumentUID: testDocumentID,
				Mode: document.MapPinVersionPinned, ContentVersionID: testVersionID}}}}}}
	definitionView, err := RenderDefinition(definition, 0, 10)
	require.NoError(t, err)
	assert.Len(t, definitionView.Sections, 2)
	assert.Contains(t, definitionView.Markdown, "## Introduction")
	snapshot := store.ContentMapSnapshot{ID: testSnapshotID, MapID: testMapID,
		Sections: []store.ContentMapSnapshotSection{{ID: "intro", Heading: "Introduction"},
			{ID: "core", Heading: "Core", Entries: []store.ContentMapSnapshotEntry{{
				DocumentUID: testDocumentID, Availability: "available", Name: "Source",
				Member: store.SnapshotMember{NodeID: 7, ContentVersionID: testVersionID},
			}}}}}
	snapshotView, err := RenderSnapshot(snapshot, 0, 10)
	require.NoError(t, err)
	assert.Len(t, snapshotView.Sections, 2)
	assert.Contains(t, snapshotView.Markdown, "## Introduction")
}

func TestRenderViewsKeepEmptySectionOnLaterEntryPage(t *testing.T) {
	ids := []string{testDocumentID, "55555555-5555-4555-8555-555555555555",
		"66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777"}
	definition := store.ContentMap{ID: testMapID, Definition: store.ContentMapDefinition{Title: "Synthetic", Scope: "local",
		Sections: []store.ContentMapSection{
			{ID: "first", Heading: "First", Include: []document.ContentMapPin{
				{DocumentUID: ids[0], Mode: document.MapPinFollowCurrent},
				{DocumentUID: ids[1], Mode: document.MapPinFollowCurrent},
				{DocumentUID: ids[2], Mode: document.MapPinFollowCurrent}}},
			{ID: "empty", Heading: "Curated empty section"},
			{ID: "last", Heading: "Last", Include: []document.ContentMapPin{
				{DocumentUID: ids[3], Mode: document.MapPinFollowCurrent}}}}}}
	firstDefinition, err := RenderDefinition(definition, 0, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, firstDefinition.NextOffset)
	secondDefinition, err := RenderDefinition(definition, firstDefinition.NextOffset, 3)
	require.NoError(t, err)
	if assert.Len(t, secondDefinition.Sections, 3, "later pages must retain empty curated sections") {
		assert.Equal(t, []string{"first", "empty", "last"}, []string{
			secondDefinition.Sections[0].ID, secondDefinition.Sections[1].ID, secondDefinition.Sections[2].ID})
	}
	assert.Contains(t, secondDefinition.Markdown, "## Curated empty section")
	boundaryDefinition := definition
	boundaryDefinition.Definition.Sections = append([]store.ContentMapSection(nil), definition.Definition.Sections...)
	boundaryDefinition.Definition.Sections[0].Include = definition.Definition.Sections[0].Include[:2]
	firstBoundaryDefinition, err := RenderDefinition(boundaryDefinition, 0, 2)
	require.NoError(t, err)
	assert.NotContains(t, firstBoundaryDefinition.Markdown, "## Curated empty section")
	secondBoundaryDefinition, err := RenderDefinition(boundaryDefinition, firstBoundaryDefinition.NextOffset, 2)
	require.NoError(t, err)
	assert.Contains(t, secondBoundaryDefinition.Markdown, "## Curated empty section")

	snapshot := store.ContentMapSnapshot{ID: testSnapshotID, MapID: testMapID,
		Sections: []store.ContentMapSnapshotSection{
			{ID: "first", Heading: "First", Entries: []store.ContentMapSnapshotEntry{
				{DocumentUID: ids[0], Availability: "available"},
				{DocumentUID: ids[1], Availability: "available"},
				{DocumentUID: ids[2], Availability: "available"}}},
			{ID: "empty", Heading: "Curated empty section"},
			{ID: "last", Heading: "Last", Entries: []store.ContentMapSnapshotEntry{
				{DocumentUID: ids[3], Availability: "available"}}}}}
	firstSnapshot, err := RenderSnapshot(snapshot, 0, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, firstSnapshot.NextOffset)
	secondSnapshot, err := RenderSnapshot(snapshot, firstSnapshot.NextOffset, 3)
	require.NoError(t, err)
	if assert.Len(t, secondSnapshot.Sections, 3, "later pages must retain empty curated sections") {
		assert.Equal(t, []string{"first", "empty", "last"}, []string{
			secondSnapshot.Sections[0].ID, secondSnapshot.Sections[1].ID, secondSnapshot.Sections[2].ID})
	}
	assert.Contains(t, secondSnapshot.Markdown, "## Curated empty section")
	boundarySnapshot := snapshot
	boundarySnapshot.Sections = append([]store.ContentMapSnapshotSection(nil), snapshot.Sections...)
	boundarySnapshot.Sections[0].Entries = snapshot.Sections[0].Entries[:2]
	firstBoundarySnapshot, err := RenderSnapshot(boundarySnapshot, 0, 2)
	require.NoError(t, err)
	assert.NotContains(t, firstBoundarySnapshot.Markdown, "## Curated empty section")
	secondBoundarySnapshot, err := RenderSnapshot(boundarySnapshot, firstBoundarySnapshot.NextOffset, 2)
	require.NoError(t, err)
	assert.Contains(t, secondBoundarySnapshot.Markdown, "## Curated empty section")
}

func TestRenderSnapshotByteCapPagesWholeCitations(t *testing.T) {
	snapshot := store.ContentMapSnapshot{ID: testSnapshotID, MapID: testMapID,
		Sections: []store.ContentMapSnapshotSection{{ID: "core", Heading: "Core"}}}
	for index := range 100 {
		snapshot.Sections[0].Entries = append(snapshot.Sections[0].Entries, store.ContentMapSnapshotEntry{
			DocumentUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", index), Availability: "available",
			Name: strings.Repeat("界", 256), Member: store.SnapshotMember{NodeID: int64(index + 1),
				ContentVersionID: testVersionID}})
	}
	first, err := RenderSnapshot(snapshot, 0, 100)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(first.Markdown), MaxMapReadBytes)
	assert.Positive(t, first.ReturnedEntries)
	assert.Less(t, first.ReturnedEntries, 100)
	assert.Equal(t, first.ReturnedEntries+1, first.NextOffset)
	second, err := RenderSnapshot(snapshot, first.NextOffset, 100)
	require.NoError(t, err)
	assert.NotEmpty(t, second.Sections)
	assert.NotEqual(t, first.Sections[0].Entries[0].DocumentUID, second.Sections[0].Entries[0].DocumentUID)
}

func TestRenderDefinitionMixedEmptySectionsMakeBoundedProgress(t *testing.T) {
	record := store.ContentMap{ID: testMapID, Definition: store.ContentMapDefinition{Title: "Synthetic", Scope: "local"}}
	wantIDs := make([]string, 0, 50)
	for index := range 49 {
		wantIDs = append(wantIDs, fmt.Sprintf("s%d", index))
		record.Definition.Sections = append(record.Definition.Sections, store.ContentMapSection{
			ID: fmt.Sprintf("s%d", index), Heading: "Section", Description: strings.Repeat("界", 512)})
	}
	wantIDs = append(wantIDs, "pinned")
	record.Definition.Sections = append(record.Definition.Sections, store.ContentMapSection{
		ID: "pinned", Heading: "Pinned", Include: []document.ContentMapPin{{
			DocumentUID: testDocumentID, Mode: document.MapPinFollowCurrent,
		}},
	})
	var seen []string
	offset, returnedPins := 0, 0
	for page := range 50 {
		view, err := RenderDefinition(record, offset, 100)
		require.NoError(t, err)
		assert.Equal(t, "item", view.OffsetUnit)
		assert.LessOrEqual(t, len(view.Markdown), MaxMapReadBytes)
		for _, section := range view.Sections {
			seen = append(seen, section.ID)
		}
		returnedPins += view.ReturnedEntries
		if !view.Truncated {
			break
		}
		require.Greater(t, view.NextOffset, offset, "a truncated page must advance")
		offset = view.NextOffset
		if page == 49 {
			t.Fatal("map did not finish within its bounded section count")
		}
	}
	assert.Equal(t, wantIDs, seen, "every curated section must remain readable in order")
	assert.Equal(t, 1, returnedPins)
}

func TestRenderSnapshotMixedEmptySectionsMakeBoundedProgress(t *testing.T) {
	snapshot := store.ContentMapSnapshot{ID: testSnapshotID, MapID: testMapID}
	wantIDs := make([]string, 0, 50)
	for index := range 49 {
		wantIDs = append(wantIDs, fmt.Sprintf("s%d", index))
		snapshot.Sections = append(snapshot.Sections, store.ContentMapSnapshotSection{
			ID: fmt.Sprintf("s%d", index), Heading: "Section", Description: strings.Repeat("界", 512)})
	}
	wantIDs = append(wantIDs, "pinned")
	snapshot.Sections = append(snapshot.Sections, store.ContentMapSnapshotSection{
		ID: "pinned", Heading: "Pinned", Entries: []store.ContentMapSnapshotEntry{{
			DocumentUID: testDocumentID, Availability: "available", Name: "Source",
			Member: store.SnapshotMember{NodeID: 7, ContentVersionID: testVersionID},
		}},
	})
	var seen []string
	offset, returnedEntries := 0, 0
	for page := range 50 {
		view, err := RenderSnapshot(snapshot, offset, 100)
		require.NoError(t, err)
		assert.Equal(t, "item", view.OffsetUnit)
		assert.LessOrEqual(t, len(view.Markdown), MaxMapReadBytes)
		for _, section := range view.Sections {
			seen = append(seen, section.ID)
		}
		returnedEntries += view.ReturnedEntries
		if !view.Truncated {
			break
		}
		require.Greater(t, view.NextOffset, offset, "a truncated page must advance")
		offset = view.NextOffset
		if page == 49 {
			t.Fatal("snapshot did not finish within its bounded section count")
		}
	}
	assert.Equal(t, wantIDs, seen, "every frozen section must remain readable in order")
	assert.Equal(t, 1, returnedEntries)
}
