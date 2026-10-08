package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestQuerySnapshotCaptureDayCompleteCalendar(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for i := range 120 {
		day := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
		node := browsePhotoNode(t, s, fmt.Sprintf("day-%03d.jpg", i), browseHash(day), "image/jpeg")
		stamp := day + "T23:30:00-12:00"
		browsePhotoMetadata(t, s, node, day, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneOffset, "-12:00")))
	}
	browsePhotoNode(t, s, "undated.jpg", browseHash("undated-calendar"), "image/jpeg")
	browsePhotoNode(t, s, "ordinary.txt", browseHash("ordinary-calendar"), "text/plain")
	request := SnapshotRequest{FacetsOnly: true, Query: snapshotTestQuery(t, `{}`), Facets: []string{"capture_day"}}
	projection, err := s.MaterializeQuerySnapshot(t.Context(), request)
	require.NoError(t, err)
	facet := facetByDimension(t, projection, "capture_day")
	require.True(t, facet.Available)
	require.Len(t, facet.Values, 120)
	require.Equal(t, int64(121), *facet.Total)
	require.Equal(t, int64(1), *facet.Missing)
	require.Zero(t, *facet.Other)
	require.Equal(t, "2024-04-29", facet.Values[0].Key)
	require.Equal(t, int64(1), facetCounts(facet)["2024-02-29"])
	page := browsePhotoPage(t, s, `{}`)
	require.Equal(t, page.Total, *facet.Total)
	for _, options := range []snapshotMaterializeOptions{{FacetMemberLimit: 1}, {FacetTimeout: -time.Nanosecond}} {
		projection, err := s.materializeQuerySnapshot(t.Context(), request, options)
		require.NoError(t, err)
		facet := facetByDimension(t, projection, "capture_day")
		require.False(t, facet.Available)
		require.Nil(t, facet.Total)
		require.Empty(t, facet.Values)
	}
}

func TestQuerySnapshotCaptureDayMatchesPhotoScope(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	raw := browsePhotoNode(t, s, "pair.raw", browseHash("calendar-raw"), "application/octet-stream")
	jpeg := browsePhotoNode(t, s, "pair.jpg", browseHash("calendar-jpeg"), "image/jpeg")
	asset, err := s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, asset.Files[0].ID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err = s.PromotePhotoNode(ctx, raw.ID, nil, PhotoRoleRAW, "")
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, jpeg.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	sidecar := browsePhotoNode(t, s, "pair.xmp", browseHash("calendar-sidecar"), "application/octet-stream")
	rawID := fileByRole(asset.Files, PhotoRoleRAW).ID
	asset, err = s.AttachPhotoFile(ctx, asset.ID, asset.Revision, sidecar.ID, PhotoRoleSidecar, &rawID)
	require.NoError(t, err)
	for _, item := range []struct {
		node Node
		day  string
	}{{raw, "2024-02-29"}, {jpeg, "2025-01-01"}} {
		stamp := item.day + "T00:30:00"
		browsePhotoMetadata(t, s, item.node, item.day, photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp(stamp, stamp, document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneOmitted, "")))
	}
	_, err = s.CreateSavedQuery(ctx, "Sidecars", "", SavedQueryKindQuery, []byte(`{"filters":{"extensions":["xmp"],"capture_before":"2025-01-01"}}`))
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Calendar")
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, sidecar.ID, sidecar.Revision)
	require.NoError(t, err)
	album, err := s.CreatePhotoSet(ctx, "Calendar")
	require.NoError(t, err)
	_, err = s.ChangePhotoSetMembers(ctx, album.ID, album.Revision, true, PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	for i := range 5 {
		browsePhotoNode(t, s, fmt.Sprintf("unrelated-%d.txt", i), browseHash(fmt.Sprintf("unrelated-%d", i)), "text/plain")
	}
	photoRequest := SnapshotRequest{FacetsOnly: true, Query: snapshotTestQuery(t, `{}`), Facets: []string{"capture_day"}}
	photos, err := s.materializeQuerySnapshot(ctx, photoRequest, snapshotMaterializeOptions{MaxRows: 1})
	require.NoError(t, err)
	require.Empty(t, photos.Rows)
	require.Equal(t, int64(1), *facetByDimension(t, photos, "capture_day").Total)
	photoRequest.Query = snapshotTestQuery(t, `{"filters":{"extensions":["xmp"]}}`)
	matched, err := s.materializeQuerySnapshot(ctx, photoRequest, snapshotMaterializeOptions{MaxRows: 1})
	require.NoError(t, err)
	require.Empty(t, matched.Rows)
	require.Equal(t, int64(1), *facetByDimension(t, matched, "capture_day").Total)
	_, err = s.materializeQuerySnapshot(ctx, SnapshotRequest{Query: snapshotTestQuery(t, `{}`)}, snapshotMaterializeOptions{MaxRows: 1})
	require.ErrorIs(t, err, ErrQuerySnapshotTooLarge)
	outside := snapshotTestQuery(t, `{"syntax":"advanced","text":"saved:Sidecars AND capture_after:2025-01-01 AND capture_before:2025-01-02"}`)
	photos, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{FacetsOnly: true, Query: outside, Facets: []string{"capture_day"}})
	require.NoError(t, err)
	require.Zero(t, photos.Total)
	require.Zero(t, *facetByDimension(t, photos, "capture_day").Total)
	for _, value := range []string{`{}`, `{"filters":{"extensions":["xmp"]}}`, `{"syntax":"advanced","text":"saved:Sidecars AND capture_before:2025-01-01"}`, `{"filters":{"capture_after":"2024-02-29","capture_before":"2024-03-01"}}`, `{"filters":{"collapse_duplicates":true}}`, fmt.Sprintf(`{"filters":{"tag_ids":[%q]}}`, tag.ID), fmt.Sprintf(`{"filters":{"set_ids":[%q]}}`, album.ID), `{"sort":{"field":"capture_time","direction":"desc"}}`, `{"sort":{"field":"import_time","direction":"asc"}}`, fmt.Sprintf(`{"filters":{"set_ids":[%q]},"sort":{"field":"added_time","direction":"desc"}}`, album.ID)} {
		projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{FacetsOnly: true, Query: snapshotTestQuery(t, value), Facets: []string{"capture_day"}})
		require.NoError(t, err, value)
		facet := facetByDimension(t, projection, "capture_day")
		require.Equal(t, browsePhotoPage(t, s, value).Total, *facet.Total, value)
		require.Equal(t, map[string]int64{"2024-02-29": 1}, facetCounts(facet), value)
	}
	_, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, new(fileByRole(asset.Files, PhotoRoleImage).ID))
	require.NoError(t, err)
	projection, err := s.MaterializeQuerySnapshot(ctx, SnapshotRequest{FacetsOnly: true, Query: snapshotTestQuery(t, `{}`), Facets: []string{"capture_day"}})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"2025-01-01": 1}, facetCounts(facetByDimension(t, projection, "capture_day")))
	browsePhotoMetadata(t, s, jpeg, "updated-jpeg", photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp("2026-01-01T12:00:00", "2026-01-01T12:00:00", document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneOmitted, "")))
	projection, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{FacetsOnly: true, Query: snapshotTestQuery(t, `{}`), Facets: []string{"capture_day"}})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"2026-01-01": 1}, facetCounts(facetByDimension(t, projection, "capture_day")))
	asset, err = s.PhotoAssetForNode(ctx, jpeg.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	projection, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{FacetsOnly: true, Query: snapshotTestQuery(t, `{}`), Facets: []string{"capture_day"}})
	require.NoError(t, err)
	require.Zero(t, *facetByDimension(t, projection, "capture_day").Total)
	_, err = s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, false)
	require.NoError(t, err)
	_, _, err = s.Trash(ctx, jpeg.ID, jpeg.Revision)
	require.NoError(t, err)
	projection, err = s.MaterializeQuerySnapshot(ctx, SnapshotRequest{FacetsOnly: true, Query: snapshotTestQuery(t, `{}`), Facets: []string{"capture_day"}})
	require.NoError(t, err)
	require.Zero(t, *facetByDimension(t, projection, "capture_day").Total)
}

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
	t.Parallel()
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
	for _, value := range collections.Values {
		assert.Equal(t, map[string]string{
			fixture.collections[0].ID(): "First", fixture.collections[1].ID(): "Second",
		}[value.Key], value.Label)
	}

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

func TestQuerySnapshotFacetsRetainNestedFiltersAndGlobalDuplicates(t *testing.T) {
	t.Parallel()
	fixture := newSnapshotFacetFixture(t)
	_, err := fixture.store.CreateSavedQuery(t.Context(), "PDFs", "", SavedQueryKindQuery,
		[]byte(`{"filters":{"extensions":["pdf"]}}`))
	require.NoError(t, err)
	projection, err := fixture.store.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{
		Query:  snapshotTestQuery(t, `{"syntax":"advanced","text":"saved:PDFs"}`),
		Facets: []string{"extension"},
	})
	require.NoError(t, err)
	extension := facetByDimension(t, projection, "extension")
	require.True(t, extension.Available)
	assert.Equal(t, map[string]int64{"pdf": 2}, facetCounts(extension))
	assert.Equal(t, int64(2), *extension.Total)

	projection, err = fixture.store.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{
		Query:  snapshotTestQuery(t, `{"syntax":"advanced","text":"name:a.PDF"}`),
		Facets: []string{"duplicates"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), projection.Total)
	assert.Equal(t, map[string]int64{"duplicate": 1, "unique": 0},
		facetCounts(facetByDimension(t, projection, "duplicates")), "the duplicate peer is outside the query result")
}

// Omitting the selected outer dimension must keep the other constraint and
// append a selected zero rather than presenting an empty facet.
func TestQuerySnapshotFacetsSelfExcludeAndRetainSelectedZero(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	_, err := s.CreateFile(t.Context(), s.RootID(), "a.txt", fakeHash("facet-timeout"), 1, "text/plain")
	require.NoError(t, err)
	projection, err := s.materializeQuerySnapshot(t.Context(), SnapshotRequest{
		Query: snapshotTestQuery(t, `{}`), Facets: []string{"tags", "size"},
	}, snapshotMaterializeOptions{FacetTimeout: -time.Nanosecond})
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
	t.Parallel()
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
	t.Parallel()
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

func TestCaptureDayCountsIgnoreUnusedLargeTags(t *testing.T) {
	for _, driverCase := range walkTestDrivers() {
		t.Run(driverCase.name, func(t *testing.T) {
			s := newTestStoreWithDriver(t, driverCase.driver)
			node := browsePhotoNode(t, s, "large-tag.jpg", browseHash("calendar-large-tag"), "image/jpeg")
			browsePhotoMetadata(t, s, node, "large-tag", photoMetadataField("created", "image.exif", "DateTimeOriginal", photoTimestamp("2024-02-29T12:00:00", "2024-02-29T12:00:00", document.SourceMetadataPrecisionSecond, document.SourceMetadataTimezoneOmitted, "")))
			tag, err := s.CreateTag(t.Context(), strings.Repeat("x", 65<<10))
			require.NoError(t, err)
			_, err = s.AssignTag(t.Context(), tag.ID, node.ID, node.Revision)
			require.NoError(t, err)
			service := NewQuerySnapshotService(s)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			counts, err := service.CreateFacets(t.Context(), "owner", SnapshotRequest{Query: snapshotTestQuery(t, `{}`), FacetsOnly: true, Facets: []string{"capture_day"}})
			require.NoError(t, err)
			require.Equal(t, map[string]int64{"2024-02-29": 1}, facetCounts(facetByDimension(t, counts, "capture_day")))
			require.Empty(t, counts.Rows)
			require.Empty(t, counts.MemberHash)
			require.Equal(t, snapshotCacheStats{}, service.stats())
		})
	}
}
