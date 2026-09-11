package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

// Losing the exact head, profile, or serving-generation join changes these
// independently constructed complete/partial/failed/unprocessed/empty counts.
func TestCollectionCoverageFiveStatesAndExactAuthority(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 5)
	selection := CoverageSelection{Configuration: "configured", ProfileFingerprint: profile.Fingerprint}
	collectionCoveragePublish(t, s, nodes[0], profile, "complete")
	collectionCoveragePublish(t, s, nodes[1], profile, "partial")
	collectionCoverageFail(t, s, nodes[2], profile, false)
	collectionCoveragePublish(t, s, nodes[4], profile, "none")
	addCollectionMembership(t, s, run, nodes[0].ID, "duplicate fact", nil)
	got, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, int64(5), got.FileCount)
	require.Equal(t, CoverageCounts{Complete: 1, Partial: 1, Failed: 1, Unprocessed: 1, None: 1}, *got.Coverage.Counts)
	require.NotEmpty(t, got.Coverage.GenerationID)
	list, _, err := s.Collections(t.Context(), 10, 0, selection)
	require.NoError(t, err)
	require.Equal(t, got.Coverage, list[0].Coverage)
	page, err := s.CollectionMembers(t.Context(), run.ID(), 1, 0, selection)
	require.NoError(t, err)
	require.Equal(t, got.Coverage, page.Collection.Coverage)

	other := catalogProcessingProfile(t, true)
	isolated, err := s.CollectionByID(t.Context(), run.ID(), CoverageSelection{"configured", other.Fingerprint})
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Unprocessed: 5}, *isolated.Coverage.Counts)
	_, _, err = s.ReplaceContent(t.Context(), nodes[0].ID, nodes[0].Revision, nodes[0].BlobHash, nodes[0].Size, nodes[0].MimeType)
	require.NoError(t, err)
	// Same bytes still create an exact new version; prior output cannot transfer.
	updated, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Partial: 1, Failed: 1, Unprocessed: 2, None: 1}, *updated.Coverage.Counts)
}

func TestCollectionCoverageRetainsActivatedOutputAfterFailure(t *testing.T) {
	for _, state := range []string{"complete", "none", "degraded", "truncated", "partial_success"} {
		t.Run(state, func(t *testing.T) {
			s, run, nodes, profile := collectionCoverageFixture(t, 1)
			collectionCoveragePublish(t, s, nodes[0], profile, state)
			collectionCoverageFail(t, s, nodes[0], profile, false)
			got, err := s.CollectionByID(t.Context(), run.ID(), CoverageSelection{"configured", profile.Fingerprint})
			require.NoError(t, err)
			want := CoverageCounts{Complete: 1}
			if state == "none" {
				want = CoverageCounts{None: 1}
			}
			if state == "truncated" || state == "partial_success" {
				want = CoverageCounts{Partial: 1}
			}
			require.Equal(t, want, *got.Coverage.Counts)
		})
	}
}

func TestCollectionCoverageNativeTextFollowsServingSearchPath(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 1)
	native, err := s.IngestFileExact(t.Context(), run, s.RootID(), "native.txt", fakeHash("c9"), 19, "text/plain", "native.txt", "")
	require.NoError(t, err)
	require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{
		BlobHash: native.BlobHash, Extractor: "synthetic-native", ExtractorVersion: 1, Status: ExtractionOK, Text: "nativeuniqueterm",
	}))
	selection := CoverageSelection{"configured", profile.Fingerprint}
	before, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Complete: 1, Unprocessed: 1}, *before.Coverage.Counts)
	hits, _, err := s.SearchPage(t.Context(), "nativeuniqueterm", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	collectionCoveragePublish(t, s, nodes[0], profile, "complete")
	after, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Complete: 1, Unprocessed: 1}, *after.Coverage.Counts)
	hits, _, err = s.SearchPage(t.Context(), "nativeuniqueterm", 10)
	require.NoError(t, err)
	require.Empty(t, hits, "legacy native text stops serving after a rendition generation activates")
}

func TestCollectionCoverageSelectionAndUnavailableStates(t *testing.T) {
	s, run, _, profile := collectionCoverageFixture(t, 1)
	for _, selection := range []CoverageSelection{{}, {Configuration: "unconfigured"}, {Configuration: "profile_required"}} {
		got, err := s.CollectionByID(t.Context(), run.ID(), selection)
		require.NoError(t, err)
		require.Nil(t, got.Coverage.Counts)
		require.NotEmpty(t, got.Coverage.Configuration)
	}
	got, err := s.CollectionByID(t.Context(), run.ID(), CoverageSelection{"configured", profile.Fingerprint})
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Unprocessed: 1}, *got.Coverage.Counts, "valid policy need not have catalog rows yet")
	_, err = s.CollectionByID(t.Context(), run.ID(), CoverageSelection{}, CoverageSelection{})
	require.ErrorIs(t, err, ErrInvalidCoverageSelection)
	_, _, err = s.Collections(t.Context(), 10, 0, CoverageSelection{}, CoverageSelection{})
	require.ErrorIs(t, err, ErrInvalidCoverageSelection)
	_, err = s.CollectionMembers(t.Context(), run.ID(), 10, 0, CoverageSelection{}, CoverageSelection{})
	require.ErrorIs(t, err, ErrInvalidCoverageSelection)
	_, err = s.CollectionByID(t.Context(), run.ID(), CoverageSelection{Configuration: "configured"})
	require.ErrorIs(t, err, ErrInvalidCoverageSelection)
}

func TestCollectionCoverageStagedAndMismatchedGenerationAreUnprocessed(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 1)
	selection := CoverageSelection{"configured", profile.Fingerprint}
	build := catalogRenditionBuild(s, profile)
	build.SourceSHA256 = nodes[0].BlobHash
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	_, err := s.StageLexicalGeneration(t.Context(), fakeHash("d1"))
	require.NoError(t, err)
	staged, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Unprocessed: 1}, *staged.Coverage.Counts)
	require.Empty(t, staged.Coverage.GenerationID)
	require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: nodes[0].CurrentVersionID,
		BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339(),
	}))
	active, err := s.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	// Deliberately damage derivative membership; a read must neither repair it
	// nor count a retained catalog head outside the generation that serves search.
	_, err = s.db.Exec(`DELETE FROM rendition_lexical_generation_builds WHERE generation_id=?`, active.ID)
	require.NoError(t, err)
	mismatch, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Unprocessed: 1}, *mismatch.Coverage.Counts)
	require.Equal(t, active.ID, mismatch.Coverage.GenerationID)
	hits, _, err := s.SearchPage(t.Context(), "Synthetic", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM rendition_lexical_generation_builds WHERE generation_id=?`, active.ID).Scan(&count))
	require.Zero(t, count)
}

func TestCollectionCoverageBrokenGenerationIsNotMissingCollection(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 1)
	collectionCoveragePublish(t, s, nodes[0], profile, "complete")
	_, err := s.db.Exec(`DELETE FROM rendition_lexical_generation_manifests`)
	require.NoError(t, err)
	_, err = s.CollectionByID(t.Context(), run.ID(), CoverageSelection{"configured", profile.Fingerprint})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound, "broken serving catalog must not become a missing-collection response")
}

func TestCollectionCoverageSupersessionMultiImportAndEmptyRetainedCollection(t *testing.T) {
	s, first, nodes, profile := collectionCoverageFixture(t, 1)
	selection := CoverageSelection{"configured", profile.Fingerprint}
	collectionCoveragePublish(t, s, nodes[0], profile, "complete")
	second, err := s.BeginIngest(t.Context(), "watch", "Another synthetic import")
	require.NoError(t, err)
	addCollectionMembership(t, s, second, nodes[0].ID, "source.pdf", nil)
	for _, id := range []string{first.ID(), second.ID()} {
		got, err := s.CollectionByID(t.Context(), id, selection)
		require.NoError(t, err)
		require.Equal(t, CoverageCounts{Complete: 1}, *got.Coverage.Counts)
	}
	_, err = s.SetCollectionLabel(t.Context(), first.ID(), 1, new("Retained synthetic collection"))
	require.NoError(t, err)
	var prior string
	require.NoError(t, s.db.QueryRow(`SELECT identity FROM provenance WHERE ingest_id=?`, first.ID()).Scan(&prior))
	addCollectionMembership(t, s, second, nodes[0].ID, "corrected.pdf", &prior)
	got, err := s.CollectionByID(t.Context(), first.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, int64(0), got.FileCount)
	require.Equal(t, CoverageCounts{}, *got.Coverage.Counts)
	quality, err := s.CollectionQuality(t.Context(), first.ID(), selection, nil)
	require.NoError(t, err)
	require.Equal(t, got, quality.Collection)
	for _, dimension := range quality.Dimensions {
		require.Empty(t, dimension.Values)
		require.Zero(t, dimension.Missing)
		require.Zero(t, dimension.Other)
	}
}

func TestCollectionCoverageOperatorRequired(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 1)
	collectionCoverageFail(t, s, nodes[0], profile, true)
	selection := CoverageSelection{"configured", profile.Fingerprint}
	got, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Failed: 1}, *got.Coverage.Counts)
}

func TestCollectionCoverageLatestExactAttemptUsesWaiterAndJobTimes(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 1)
	selection := CoverageSelection{"configured", profile.Fingerprint}
	first := renditionJobTestRequest(nodes[0].CurrentVersionID, profile)
	first.ExecutionIdentity.Upload.SHA256 = nodes[0].BlobHash
	first.ExecutionIdentity.Authorization.SourceSHA256 = nodes[0].BlobHash
	grantRenditionJobConsent(t, s, first)
	oldJob, _, err := s.EnqueueRenditionJob(t.Context(), first)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), oldJob.ID, "first-worker", at, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureTerminal, at.Add(time.Second)))
	second := first
	second.CapturedArtifactPolicy = []byte(strings.Replace(string(first.CapturedArtifactPolicy), `"max_count":1,"min_count":0`, `"max_count":2,"min_count":0`, 1))
	newJob, newWaiter, err := s.EnqueueRenditionJob(t.Context(), second)
	require.NoError(t, err)
	require.NotEqual(t, oldJob.ID, newJob.ID)
	// Only mutable attempt timestamps are controlled; jobs and exact waiters
	// were created by their catalog APIs. New waiter activity beats old failure.
	_, err = s.db.Exec(`UPDATE rendition_jobs SET updated_at='2026-01-01T00:00:00.000000000Z'`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE rendition_job_waiters SET updated_at='2026-01-01T00:00:00.000000000Z'`)
	require.NoError(t, err)
	_, err = s.db.Exec(`UPDATE rendition_job_waiters SET updated_at='2026-01-02T00:00:00.000000000Z' WHERE waiter_id=?`, newWaiter.ID)
	require.NoError(t, err)
	got, err := s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Unprocessed: 1}, *got.Coverage.Counts)
	// A later job transition also counts even if its waiter timestamp is older.
	_, err = s.db.Exec(`UPDATE rendition_jobs SET updated_at='2026-01-03T00:00:00.000000000Z' WHERE job_id=?`, oldJob.ID)
	require.NoError(t, err)
	got, err = s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Failed: 1}, *got.Coverage.Counts)
	// Rejection is waiter-local and outranks a queued job for that exact waiter.
	_, err = s.db.Exec(`UPDATE rendition_job_waiters SET state='rejected', updated_at='2026-01-04T00:00:00.000000000Z' WHERE waiter_id=?`, newWaiter.ID)
	require.NoError(t, err)
	got, err = s.CollectionByID(t.Context(), run.ID(), selection)
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Failed: 1}, *got.Coverage.Counts)
}

func collectionCoverageFixture(t *testing.T, count int) (*Store, IngestRun, []Node, ProcessingProfileRecord) {
	t.Helper()
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "Synthetic coverage collection")
	require.NoError(t, err)
	nodes := make([]Node, count)
	for i := range nodes {
		nodes[i], err = s.IngestFileExact(t.Context(), run, s.RootID(), fmt.Sprintf("source-%d.pdf", i),
			testSHA256([]byte(fmt.Sprintf("synthetic source %d", i))), 20, "application/pdf", fmt.Sprintf("source-%d.pdf", i), "")
		require.NoError(t, err)
	}
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		for _, hash := range []string{catalogEvidenceBlobHash, catalogMarkdownBlobHash} {
			if err := s.EnsureBlobTx(tx, hash, int64(len(catalogBlobContents[hash]))); err != nil {
				return err
			}
		}
		return nil
	}))
	return s, run, nodes, catalogProcessingProfile(t, false)
}

func collectionCoveragePublish(t *testing.T, s *Store, node Node, profile ProcessingProfileRecord, state string) {
	t.Helper()
	build := catalogRenditionBuild(s, profile)
	build.ID = testSHA256([]byte(node.CurrentVersionID + state))
	build.SourceSHA256 = node.BlobHash
	switch state {
	case "partial":
		build.Completeness = document.EvidencePartial
	case "degraded":
		build.Completeness = document.EvidenceDegradedProvenance
	case "truncated":
		build.Truncated = true
	case "partial_success":
		build.PartialSuccess = true
	case "none":
		build.Units = nil
		build.LexicalSegments = nil
	}
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	require.NoError(t, publishAttachmentForTest(t, s, RenditionAttachmentRecord{
		ID: testSHA256([]byte(node.CurrentVersionID + profile.Fingerprint)), VaultID: s.VaultID(),
		ContentVersionID: node.CurrentVersionID, BuildID: build.ID, Profile: profile, AttachedAt: nowRFC3339(),
	}))
}

func collectionCoverageFail(t *testing.T, s *Store, node Node, profile ProcessingProfileRecord, operator bool) {
	t.Helper()
	request := renditionJobTestRequest(node.CurrentVersionID, profile)
	request.ExecutionIdentity.Upload.SHA256 = node.BlobHash
	request.ExecutionIdentity.Authorization.SourceSHA256 = node.BlobHash
	grantRenditionJobConsent(t, s, request)
	job, _, err := s.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Second)
	claim, err := s.ClaimRenditionJob(t.Context(), job.ID, "coverage-worker", at, time.Minute)
	require.NoError(t, err)
	if operator {
		require.NoError(t, s.MarkRenditionJobOperatorRequired(t.Context(), claim, at.Add(time.Second)))
	} else {
		require.NoError(t, s.MarkRenditionJobFailed(t.Context(), claim, RenditionFailureTerminal, at.Add(time.Second)))
	}
}
