package store

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type snapshotFacetFixture struct {
	store       *Store
	selection   CoverageSelection
	collections []IngestRun
	tags        []Tag
	nodes       map[string]Node
}

func newSnapshotFacetFixture(t *testing.T) snapshotFacetFixture {
	t.Helper()
	s := newTestStore(t)
	ctx := t.Context()
	first, err := s.BeginIngest(ctx, "cli", "Synthetic first")
	require.NoError(t, err)
	a, err := s.IngestFileExact(ctx, first, s.RootID(), "a.PDF", testSHA256([]byte("facet-duplicate")), 500,
		"application/pdf", "/synthetic/a.PDF", "")
	require.NoError(t, err)
	b, err := s.IngestFileExact(ctx, first, s.RootID(), "b", fakeHash("facet-b"), 2<<20,
		"text/plain", "/synthetic/b", "")
	require.NoError(t, err)
	firstLabel := "First"
	_, err = s.SetCollectionLabel(ctx, first.ID(), 1, &firstLabel)
	require.NoError(t, err)

	second, err := s.BeginIngest(ctx, "cli", "Synthetic second")
	require.NoError(t, err)
	c, err := s.IngestFileExact(ctx, second, s.RootID(), "c.pdf", a.BlobHash, a.Size,
		a.MimeType, "/synthetic/c.pdf", "")
	require.NoError(t, err)
	addCollectionMembership(t, s, second, a.ID, "/synthetic/also-a.PDF", nil)
	secondLabel := "Second"
	_, err = s.SetCollectionLabel(ctx, second.ID(), 1, &secondLabel)
	require.NoError(t, err)

	zip, err := s.CreateFile(ctx, s.RootID(), "d.ZIP", fakeHash("facet-zip"), 20<<20, "application/zip")
	require.NoError(t, err)
	mp3, err := s.CreateFile(ctx, s.RootID(), "e.mp3", fakeHash("facet-mp3"), 200<<20, "audio/mpeg")
	require.NoError(t, err)
	bin, err := s.CreateFile(ctx, s.RootID(), "f.bin", fakeHash("facet-bin"), 2<<30, "application/octet-stream")
	require.NoError(t, err)

	red, err := s.CreateTag(ctx, "Red")
	require.NoError(t, err)
	blue, err := s.CreateTag(ctx, "Blue")
	require.NoError(t, err)
	for _, target := range []Node{a, c} {
		change, assignErr := s.AssignTag(ctx, red.ID, target.ID, target.Revision)
		require.NoError(t, assignErr)
		if target.ID == a.ID {
			a = change.Node
		} else {
			c = change.Node
		}
	}
	change, err := s.AssignTag(ctx, blue.ID, a.ID, a.Revision)
	require.NoError(t, err)
	a = change.Node

	times := map[int64]string{
		a.ID: "2026-01-02T03:04:05Z", b.ID: "2026-01-20T00:00:00Z",
		c.ID: "2026-02-01T00:00:00Z", zip.ID: "2026-03-01T00:00:00Z",
		mp3.ID: "2026-04-01T00:00:00Z", bin.ID: "",
	}
	for id, modified := range times {
		_, err = s.db.ExecContext(ctx, `UPDATE nodes SET modified_at=? WHERE id=?`, modified, id)
		require.NoError(t, err)
	}
	require.NoError(t, s.RecordExtraction(ctx, ExtractionResult{
		BlobHash: a.BlobHash, Extractor: "synthetic-snapshot", ExtractorVersion: 1,
		Status: ExtractionOK, Text: "synthetic searchable text",
	}))
	for _, node := range []Node{a, c} {
		_, err = s.db.ExecContext(ctx, `INSERT INTO text_searchable_versions(version_id) VALUES(?)`, node.CurrentVersionID)
		require.NoError(t, err)
	}
	return snapshotFacetFixture{
		store: s, selection: CoverageSelection{Configuration: "configured", ProfileFingerprint: testSHA256([]byte("facet-profile"))},
		collections: []IngestRun{first, second}, tags: []Tag{red, blue},
		nodes: map[string]Node{"a": a, "b": b, "c": c, "zip": zip, "mp3": mp3, "bin": bin},
	}
}

func facetByDimension(t *testing.T, projection SnapshotProjection, dimension string) SnapshotFacet {
	t.Helper()
	for _, facet := range projection.Facets {
		if facet.Dimension == dimension {
			return facet
		}
	}
	require.FailNow(t, "missing facet", dimension)
	return SnapshotFacet{}
}

func facetCounts(facet SnapshotFacet) map[string]int64 {
	result := make(map[string]int64, len(facet.Values))
	for _, value := range facet.Values {
		result[value.Key] = value.Count
	}
	return result
}

// Returning a live filter population, counting memberships as documents, or
// dropping fixed zero buckets changes these literal facet counts.
func TestQuerySnapshotFacetsCoverEveryDimension(t *testing.T) {
	fixture := newSnapshotFacetFixture(t)
	projection, err := fixture.store.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{
		Query: snapshotTestQuery(t, `{}`), Coverage: fixture.selection,
		Facets: []string{"collections", "tags", "media_family", "extension", "modified", "size", "text_coverage", "duplicates"},
	})
	require.NoError(t, err)
	require.Len(t, projection.Facets, 8)
	for _, row := range projection.Rows {
		if row.NodeID == fixture.nodes["a"].ID {
			require.Len(t, row.CollectionIDs, 2)
			assert.True(t, sort.StringsAreSorted(row.CollectionIDs))
			assert.Equal(t, row.CollectionIDs[0], row.DisplayCollectionID)
			require.NotNil(t, row.DisplayCollectionLabel)
			expected := map[string]string{
				fixture.collections[0].ID(): "First", fixture.collections[1].ID(): "Second",
			}
			assert.Equal(t, expected[row.DisplayCollectionID], *row.DisplayCollectionLabel)
		}
	}

	collections := facetByDimension(t, projection, "collections")
	assert.Equal(t, int64(6), *collections.Total)
	assert.Equal(t, int64(3), *collections.Missing)
	assert.Equal(t, map[string]int64{
		fixture.collections[0].ID(): 2, fixture.collections[1].ID(): 2,
	}, facetCounts(collections), "one document belongs to both collections")

	tags := facetByDimension(t, projection, "tags")
	assert.Equal(t, int64(4), *tags.Missing)
	assert.Equal(t, map[string]int64{fixture.tags[0].ID: 2, fixture.tags[1].ID: 1}, facetCounts(tags))
	assert.Equal(t, map[string]int64{
		"email": 0, "document": 2, "spreadsheet": 0, "presentation": 0,
		"image": 0, "audio_video": 1, "text": 1, "source_code": 0,
		"web": 0, "calendar": 0, "archive": 1, "cad": 0, "unknown": 1,
	}, facetCounts(facetByDimension(t, projection, "media_family")))

	extension := facetByDimension(t, projection, "extension")
	assert.Equal(t, int64(1), *extension.Missing)
	assert.Equal(t, map[string]int64{"pdf": 2, "zip": 1, "mp3": 1, "bin": 1}, facetCounts(extension))
	modified := facetByDimension(t, projection, "modified")
	assert.Equal(t, int64(1), *modified.Missing)
	assert.Equal(t, map[string]int64{"2026-01": 2, "2026-02": 1, "2026-03": 1, "2026-04": 1}, facetCounts(modified))
	assert.Equal(t, map[string]int64{
		"lt_1_mib": 2, "1_mib_to_10_mib": 1, "10_mib_to_100_mib": 1,
		"100_mib_to_1_gib": 1, "gte_1_gib": 1,
	}, facetCounts(facetByDimension(t, projection, "size")))
	assert.Equal(t, map[string]int64{
		"complete": 2, "partial": 0, "failed": 0, "unprocessed": 4, "none": 0, "unavailable": 0,
	}, facetCounts(facetByDimension(t, projection, "text_coverage")))
	assert.Equal(t, map[string]int64{"duplicate": 2, "unique": 4},
		facetCounts(facetByDimension(t, projection, "duplicates")))
}

// Omitting the selected outer dimension must keep the other constraint and
// append a selected zero rather than presenting an empty facet.
func TestQuerySnapshotFacetsSelfExcludeAndRetainSelectedZero(t *testing.T) {
	fixture := newSnapshotFacetFixture(t)
	value := snapshotTestQuery(t, `{"filters":{"extensions":["pdf"],"media_families":["text"]}}`)
	projection, err := fixture.store.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{
		Query: value, Coverage: fixture.selection, Facets: []string{"extension", "media_family"},
	})
	require.NoError(t, err)
	assert.Zero(t, projection.Total)

	extension := facetByDimension(t, projection, "extension")
	assert.Equal(t, int64(1), *extension.Total)
	assert.Equal(t, int64(1), *extension.Missing)
	require.Len(t, extension.Values, 1)
	assert.Equal(t, SnapshotFacetValue{Key: "pdf", Label: "pdf", Count: 0, Selected: true}, extension.Values[0])

	media := facetByDimension(t, projection, "media_family")
	assert.Equal(t, int64(2), *media.Total)
	for _, value := range media.Values {
		if value.Key == "text" {
			assert.True(t, value.Selected)
			assert.Zero(t, value.Count)
		}
	}
}

func TestQuerySnapshotFacetUnavailableStatesAreExplicit(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("facet-unavailable-a"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "b.txt", fakeHash("facet-unavailable-b"), 1, "text/plain")
	require.NoError(t, err)
	value := snapshotTestQuery(t, `{}`)

	withoutFacets, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value})
	require.NoError(t, err)
	projection, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value, Facets: []string{"text_coverage"}})
	require.NoError(t, err)
	assert.Greater(t, projection.SerializedBytes, withoutFacets.SerializedBytes,
		"unavailable facet metadata still consumes cache admission bytes")
	unavailable := facetByDimension(t, projection, "text_coverage")
	assert.False(t, unavailable.Available)
	assert.Equal(t, "coverage_unconfigured", unavailable.Reason)
	assert.Nil(t, unavailable.Total)
	assert.Nil(t, unavailable.Missing)
	assert.Nil(t, unavailable.Other)
	assert.Nil(t, unavailable.Values)

	projection, err = s.materializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value, Facets: []string{"tags"}},
		snapshotMaterializeOptions{FacetMemberLimit: 1})
	require.NoError(t, err)
	unavailable = facetByDimension(t, projection, "tags")
	assert.False(t, unavailable.Available)
	assert.Equal(t, "member_budget_exceeded", unavailable.Reason)
}

func TestQuerySnapshotFacetMembershipBudgetBoundsOneMultivalueDocument(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "tagged.txt", fakeHash("facet-membership-budget"), 1, "text/plain")
	require.NoError(t, err)
	for _, name := range []string{"one", "two"} {
		tag, createErr := s.CreateTag(t.Context(), name)
		require.NoError(t, createErr)
		change, assignErr := s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
		require.NoError(t, assignErr)
		node = change.Node
	}
	projection, err := s.materializeQuerySnapshot(t.Context(), SnapshotRequest{
		Query: snapshotTestQuery(t, `{}`), Facets: []string{"tags"},
	}, snapshotMaterializeOptions{FacetMemberLimit: 1})
	require.NoError(t, err)
	facet := facetByDimension(t, projection, "tags")
	assert.False(t, facet.Available)
	assert.Equal(t, "member_budget_exceeded", facet.Reason)
}

func TestQuerySnapshotFacetValidationAndParentCancellation(t *testing.T) {
	s := newTestStore(t)
	value := snapshotTestQuery(t, `{}`)
	for _, facets := range [][]string{{"future"}, {"tags", "tags"}} {
		_, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value, Facets: facets})
		require.Error(t, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.MaterializeQuerySnapshot(canceled, SnapshotRequest{Query: value, Facets: []string{"tags"}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestQuerySnapshotFacetDeadlineKeepsCompletedRows(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("facet-timeout"), 1, "text/plain")
	require.NoError(t, err)
	projection, err := s.materializeQuerySnapshot(t.Context(), SnapshotRequest{
		Query: snapshotTestQuery(t, `{}`), Facets: []string{"tags", "size"},
	}, snapshotMaterializeOptions{FacetTimeout: time.Nanosecond})
	require.NoError(t, err)
	assert.Equal(t, int64(1), projection.Total)
	require.Len(t, projection.Facets, 2)
	for _, facet := range projection.Facets {
		assert.False(t, facet.Available)
		assert.Equal(t, "time_budget_exceeded", facet.Reason)
	}
}

// A selected value beyond the top fifty must be appended, while Other still
// accounts for the omitted membership instead of silently losing it.
func TestQuerySnapshotFacetTopFiftyAppendsSelectedValues(t *testing.T) {
	counts := make(map[string]int64)
	labels := make(map[string]string)
	for i := range 52 {
		key := string(rune('A' + i))
		counts[key] = 1
		labels[key] = key
	}
	selected := map[string]bool{"t": true}
	values, other := finalizeSnapshotFacetValues(counts, labels, selected, false)
	require.Len(t, values, 51)
	assert.Equal(t, int64(1), other)
	assert.True(t, values[len(values)-1].Selected)
	assert.Equal(t, "t", values[len(values)-1].Key)
	assert.True(t, sort.SliceIsSorted(values[:50], func(i, j int) bool { return values[i].Key < values[j].Key }))
}

func TestQuerySnapshotRejectsProductionRowOver64KiB(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "bounded.txt", fakeHash("bounded-row"), 1, "text/plain")
	require.NoError(t, err)
	tag, err := s.CreateTag(t.Context(), strings.Repeat("x", 66<<10))
	require.NoError(t, err)
	_, err = s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: snapshotTestQuery(t, `{}`)})
	require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)
}
