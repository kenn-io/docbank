package store

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDuplicatesFindsOnlyLiveCurrentContent(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	duplicateHash := fakeHash("11")
	replacementHash := fakeHash("22")
	singleHash := fakeHash("23")

	firstRun, err := s.BeginIngest(ctx, "cli", "First synthetic import")
	require.NoError(t, err)
	first, err := s.IngestFileExact(ctx, firstRun, s.RootID(), "first.txt",
		duplicateHash, 12, "text/plain", "/synthetic/first.txt", "")
	require.NoError(t, err)
	secondRun, err := s.BeginIngest(ctx, "cli", "Second synthetic import")
	require.NoError(t, err)
	second, err := s.IngestFileExact(ctx, secondRun, s.RootID(), "second.txt",
		duplicateHash, 12, "text/plain", "/synthetic/second.txt", "")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "single.txt", singleHash, 7, "text/plain")
	require.NoError(t, err)

	page, err := s.Duplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, duplicateHash, page.Items[0].Hash)
	assert.Equal(t, int64(12), page.Items[0].Size)
	assert.Equal(t, 2, page.Items[0].ReferenceCount)
	assert.Equal(t, 1, page.Total)
	assert.Equal(t, 2, page.TotalReferences)
	for _, ref := range page.Items[0].References {
		assert.True(t, ref.Reference.IsCurrent)
		assert.Equal(t, ref.Reference.Node.CurrentVersionID, ref.Reference.Version.ID)
		assert.NotEmpty(t, ref.Reference.Path)
		assert.NotNil(t, ref.Collections)
	}

	historicalVersion := second.CurrentVersionID
	second, _, err = s.ReplaceContent(ctx, second.ID, second.Revision,
		replacementHash, 7, "text/plain")
	require.NoError(t, err)
	page, err = s.Duplicates(ctx, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	assert.Zero(t, page.Total)
	assert.Zero(t, page.TotalReferences)

	refs, total, err := s.ContentReferencesByHash(ctx, duplicateHash, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, total, "the historical content lookup remains unchanged")
	assert.Contains(t, duplicateReferenceVersionIDs(refs), historicalVersion)

	_, _, err = s.ReplaceContent(ctx, second.ID, second.Revision,
		duplicateHash, 12, "text/plain")
	require.NoError(t, err)
	page, err = s.Duplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)

	_, _, err = s.Trash(ctx, first.ID, first.Revision)
	require.NoError(t, err)
	page, err = s.Duplicates(ctx, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	assert.Zero(t, page.TotalReferences)
	refs, total, err = s.ContentReferencesByHash(ctx, duplicateHash, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	assert.Contains(t, duplicateReferenceNodeIDs(refs), first.ID)
}

func TestDuplicateGroupByHashUsesExactLiveCurrentRelation(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("1234")
	first, err := s.CreateFile(ctx, s.RootID(), "exact-a.txt", hash, 12, "text/plain")
	require.NoError(t, err)
	second, err := s.CreateFile(ctx, s.RootID(), "exact-b.txt", hash, 12, "text/plain")
	require.NoError(t, err)

	group, err := s.DuplicateGroupByHash(ctx, hash, 12)
	require.NoError(t, err)
	require.Equal(t, 2, group.ReferenceCount)
	refs := make([]ContentReference, 0, len(group.References))
	for _, reference := range group.References {
		refs = append(refs, reference.Reference)
	}
	require.Equal(t, []int64{first.ID, second.ID}, duplicateReferenceNodeIDs(refs))

	_, err = s.DuplicateGroupByHash(ctx, fakeHash("5678"), 12)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestDuplicateReferencesExposeActiveOperationalCollections(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("33")

	primary, err := s.BeginIngestWithLabel(ctx, "cli", "Primary import", new("Primary"))
	require.NoError(t, err)
	first, err := s.IngestFileExact(ctx, primary, s.RootID(), "first.txt",
		hash, 8, "text/plain", "/synthetic/first.txt", "")
	require.NoError(t, err)
	_, err = s.CreateFile(ctx, s.RootID(), "second.txt", hash, 8, "text/plain")
	require.NoError(t, err)

	// Repeated facts in one run and membership in another run each remain one
	// displayed collection.
	addCollectionMembership(t, s, primary, first.ID, "/synthetic/repeated.txt", nil)
	secondary, err := s.BeginIngestWithLabel(ctx, "cli", "Secondary import", new("Secondary"))
	require.NoError(t, err)
	addCollectionMembership(t, s, secondary, first.ID, "/synthetic/secondary.txt", nil)

	// A superseded operational membership disappears and its active correction
	// replaces it. Caller-supplied embedded provenance never becomes a collection.
	superseded, err := s.BeginIngest(ctx, "cli", "Superseded import")
	require.NoError(t, err)
	addCollectionMembership(t, s, superseded, first.ID, "/synthetic/old.txt", nil)
	var prior string
	require.NoError(t, s.db.QueryRow(`SELECT identity FROM provenance
		WHERE ingest_id=? AND node_id=?`, superseded.ID(), first.ID).Scan(&prior))
	correction, err := s.BeginIngestWithLabel(ctx, "cli", "Corrected import", new("Corrected"))
	require.NoError(t, err)
	addCollectionMembership(t, s, correction, first.ID, "/synthetic/new.txt", &prior)
	embedded, err := s.BeginCallerSuppliedIngest(ctx, "application", "Embedded assertion")
	require.NoError(t, err)
	addCollectionMembership(t, s, embedded, first.ID, "opaque/item", nil)

	page, err := s.Duplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	var got DuplicateReference
	for _, ref := range page.Items[0].References {
		if ref.Reference.Node.ID == first.ID {
			got = ref
		}
	}
	require.Equal(t, first.ID, got.Reference.Node.ID)
	assert.Equal(t, 3, got.CollectionCount)
	assert.False(t, got.CollectionsTruncated)
	wantIDs := []string{primary.ID(), secondary.ID(), correction.ID()}
	sort.Strings(wantIDs)
	assert.Equal(t, wantIDs, duplicateCollectionIDs(got.Collections))
	wantLabels := map[string]string{
		primary.ID(): "Primary", secondary.ID(): "Secondary", correction.ID(): "Corrected",
	}
	for _, collection := range got.Collections {
		require.NotNil(t, collection.Label)
		assert.Equal(t, wantLabels[collection.ID], *collection.Label)
	}
}

func TestDuplicateReferencesAndCollectionsAreBounded(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hash := fakeHash("44")
	nodes := make([]Node, 17)
	for index := range nodes {
		var err error
		nodes[index], err = s.CreateFile(ctx, s.RootID(), fmt.Sprintf("copy-%02d.txt", index),
			hash, 5, "text/plain")
		require.NoError(t, err)
	}

	base := time.Date(2026, time.September, 11, 12, 0, 0, 0, time.UTC)
	early := base.Add(time.Nanosecond).Format(timestampLayout)
	tied := base.Add(2 * time.Nanosecond).Format(timestampLayout)
	later := base.Add(3 * time.Nanosecond).Format(timestampLayout)
	for _, node := range nodes {
		_, err := s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`, later, node.ID)
		require.NoError(t, err)
	}
	_, err := s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`, early, nodes[2].ID)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id IN (?,?)`,
		tied, nodes[1].ID, nodes[3].ID)
	require.NoError(t, err)

	collectionIDs := make([]string, 17)
	for index := range collectionIDs {
		run, err := s.BeginIngest(ctx, "cli", fmt.Sprintf("Synthetic collection %02d", index))
		require.NoError(t, err)
		addCollectionMembership(t, s, run, nodes[2].ID,
			fmt.Sprintf("/synthetic/source-%02d.txt", index), nil)
		collectionIDs[index] = run.ID()
	}
	sort.Strings(collectionIDs)

	page, err := s.Duplicates(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	group := page.Items[0]
	assert.Equal(t, 17, group.ReferenceCount)
	assert.Len(t, group.References, 16)
	assert.True(t, group.ReferencesTruncated)
	assert.Equal(t, nodes[2].ID, group.RepresentativeNodeID)
	assert.Equal(t, nodes[2].ID, group.References[0].Reference.Node.ID)
	assert.Equal(t, nodes[1].ID, group.References[1].Reference.Node.ID)
	assert.Equal(t, nodes[3].ID, group.References[2].Reference.Node.ID,
		"equal timestamps use node ID as the deterministic tie break")

	representative := group.References[0]
	assert.Equal(t, 17, representative.CollectionCount)
	assert.Len(t, representative.Collections, 16)
	assert.True(t, representative.CollectionsTruncated)
	assert.Equal(t, collectionIDs[:16], duplicateCollectionIDs(representative.Collections))
}

func TestDuplicatesPagesGroupsByHashWithExactTotals(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	hashes := []string{fakeHash("c3"), fakeHash("a1"), fakeHash("b2")}
	for index, hash := range hashes {
		for copy := range 2 {
			_, err := s.CreateFile(ctx, s.RootID(),
				fmt.Sprintf("group-%d-copy-%d.txt", index, copy), hash, int64(index+1), "text/plain")
			require.NoError(t, err)
		}
	}
	sort.Strings(hashes)

	for offset, wantHash := range hashes {
		page, err := s.Duplicates(ctx, 1, offset)
		require.NoError(t, err)
		assert.Equal(t, 3, page.Total)
		assert.Equal(t, 6, page.TotalReferences)
		assert.Equal(t, 1, page.Limit)
		assert.Equal(t, offset, page.Offset)
		require.Len(t, page.Items, 1)
		assert.Equal(t, wantHash, page.Items[0].Hash)
	}
	exhausted, err := s.Duplicates(ctx, 1, 3)
	require.NoError(t, err)
	assert.Equal(t, 3, exhausted.Total)
	assert.Equal(t, 6, exhausted.TotalReferences)
	assert.Equal(t, 1, exhausted.Limit)
	assert.Equal(t, 3, exhausted.Offset)
	assert.NotNil(t, exhausted.Items)
	assert.Empty(t, exhausted.Items)
}

func TestDuplicatesRejectsMalformedBounds(t *testing.T) {
	s := newTestStore(t)
	for _, test := range []struct {
		limit, offset int
	}{{0, 0}, {101, 0}, {1, -1}} {
		page, err := s.Duplicates(t.Context(), test.limit, test.offset)
		require.ErrorIs(t, err, ErrInvalidDuplicatePage)
		assert.Empty(t, page)
	}
}

func TestDuplicatesPropagatesCancellationAndBackendErrors(t *testing.T) {
	t.Run("canceled context", func(t *testing.T) {
		s := newTestStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := s.Duplicates(ctx, 10, 0)
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("closed backend", func(t *testing.T) {
		s := newTestStore(t)
		require.NoError(t, s.Close())
		_, err := s.Duplicates(t.Context(), 10, 0)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrInvalidDuplicatePage)
	})
}

func duplicateReferenceVersionIDs(refs []ContentReference) []string {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.Version.ID)
	}
	return ids
}

func duplicateReferenceNodeIDs(refs []ContentReference) []int64 {
	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.Node.ID)
	}
	return ids
}

func duplicateCollectionIDs(collections []DuplicateCollection) []string {
	ids := make([]string, 0, len(collections))
	for _, collection := range collections {
		ids = append(ids, collection.ID)
	}
	return ids
}
