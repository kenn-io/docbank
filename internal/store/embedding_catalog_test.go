package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	docsqlite "go.kenn.io/docbank/sqlite"
	"go.kenn.io/docbank/sqlite/modernc"
)

func TestEmbeddingCatalogChunkAndDirectFileHeadsCoexist(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	direct := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), direct))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
		SetID: direct.ID, VectorSpaceID: direct.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk},
		SetID: chunk.ID, VectorSpaceID: chunk.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	directHead, err := s.ActiveEmbeddingHead(t.Context(), EmbeddingHeadKey{
		ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile,
	})
	require.NoError(t, err)
	chunkHead, err := s.ActiveEmbeddingHead(t.Context(), EmbeddingHeadKey{
		ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk,
	})
	require.NoError(t, err)
	assert.Equal(t, direct.ID, directHead.Set.ID)
	assert.Equal(t, chunk.ID, chunkHead.Set.ID)
	assert.NotEqual(t, directHead.Record.Key.InputKind, chunkHead.Record.Key.InputKind)
}

func TestEmbeddingCoveragePublishesBindingsIndependently(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	set := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), set))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
		SetID: set.ID, VectorSpaceID: set.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	require.NoError(t, s.RecordEmbeddingFailure(t.Context(), EmbeddingFailureRecord{
		ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
		BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
		FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
	}))

	coverage, err := s.EmbeddingCoverage(t.Context(), CoverageScope{
		ContentVersionIDs: []string{versionID}, ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings: []CoverageBinding{
			{BindingID: "required", InputKind: document.EmbeddingInputOriginalFile, Required: true},
			{BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile, Required: false, VectorSpaceID: set.VectorSpace.ID},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, CoveragePartial, coverage.State)
	require.Len(t, coverage.Required, 1)
	assert.Equal(t, CoverageUnavailable, coverage.Required[0].State)
	require.Len(t, coverage.Optional, 1)
	assert.Equal(t, CoverageComplete, coverage.Optional[0].State)

	_, err = s.ActiveRendition(t.Context(), versionID, profile.Fingerprint)
	require.NoError(t, err, "an embedding failure must not revoke the rendition head")
}

func TestEmbeddingCatalogDeduplicatesExactSetsButFencesAttachmentContext(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))

	var sets, generations, vectorSets int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_sets),
		(SELECT COUNT(*) FROM embedding_input_generations),
		(SELECT COUNT(*) FROM embedding_vector_sets)`).Scan(&sets, &generations, &vectorSets))
	assert.Equal(t, 1, sets)
	assert.Equal(t, 1, generations)
	assert.Equal(t, 1, vectorSets)

	var secondVersion, buildID string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? ORDER BY version_id LIMIT 1`, versionID, catalogSourceHash).Scan(&secondVersion))
	require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, attachmentID).Scan(&buildID))
	secondAttachment := testSHA256([]byte("second-embedding-attachment"))
	require.NoError(t, s.AttachRenditionBuild(t.Context(), RenditionAttachmentRecord{
		ID: secondAttachment, VaultID: s.VaultID(), ContentVersionID: secondVersion,
		BuildID: buildID, Profile: profile, AttachedAt: embeddingCatalogTime,
	}))
	require.NoError(t, s.PublishRenditionHead(t.Context(), RenditionHeadRecord{
		ContentVersionID: secondVersion, ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: secondAttachment, PublishedAt: embeddingCatalogTime,
	}))
	second := embeddingSetFixture(s, secondVersion, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", secondAttachment)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), second))
	assert.NotEqual(t, record.InputGeneration.ID, second.InputGeneration.ID)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_input_generations`).Scan(&generations))
	assert.Equal(t, 2, generations, "attachment identities must not share catalog generation authority")
}

func TestEmbeddingCatalogRejectsInvalidRowsAndPublicationFences(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	base := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")

	for _, testCase := range []struct {
		name   string
		mutate func(*EmbeddingSetRecord)
	}{
		{"corrupt payload", func(value *EmbeddingSetRecord) { value.VectorSet.Payload[len(value.VectorSet.Payload)-1] ^= 1 }},
		{"wrong vector space", func(value *EmbeddingSetRecord) { value.VectorSet.VectorSpaceID = fakeHash("wrong-space") }},
		{"checksum mismatch", func(value *EmbeddingSetRecord) { value.VectorSet.PayloadChecksum = fakeHash("wrong") }},
		{"identity mismatch", func(value *EmbeddingSetRecord) { value.VectorSet.ID = fakeHash("wrong-id") }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := cloneEmbeddingSetRecord(base)
			testCase.mutate(&record)
			err := s.StageEmbeddingSet(t.Context(), record)
			require.Error(t, err)
		})
	}

	require.NoError(t, s.StageEmbeddingSet(t.Context(), base))
	for _, testCase := range []struct {
		name   string
		mutate func(*EmbeddingHeadRecord)
		want   string
	}{
		{"source", func(value *EmbeddingHeadRecord) { value.Key.ContentVersionID = "00000000-0000-4000-8000-000000000176" }, "stale source"},
		{"profile", func(value *EmbeddingHeadRecord) { value.ProcessingProfileFingerprint = fakeHash("missing-profile") }, "profile fingerprint"},
		{"space", func(value *EmbeddingHeadRecord) { value.VectorSpaceID = fakeHash("missing-space") }, "vector-space ID"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			head := EmbeddingHeadRecord{
				Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
				SetID: base.ID, VectorSpaceID: base.VectorSpace.ID,
				ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
			}
			testCase.mutate(&head)
			err := s.PublishEmbeddingHead(t.Context(), head)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestEmbeddingCatalogStagingRejectsMalformedVectorRowsAtRowValidation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*EmbeddingVectorSetRecord)
		want   string
	}{
		{"missing", func(vectorSet *EmbeddingVectorSetRecord) { vectorSet.Rows = vectorSet.Rows[:len(vectorSet.Rows)-1] }, "row count"},
		{"duplicate", func(vectorSet *EmbeddingVectorSetRecord) { vectorSet.Rows[1].RowID = vectorSet.Rows[0].RowID }, "duplicate row IDs"},
		{"row ID", func(vectorSet *EmbeddingVectorSetRecord) { vectorSet.Rows[0].RowID = "arbitrary-unique-row-id" }, "canonical input key"},
		{"unexpected", func(vectorSet *EmbeddingVectorSetRecord) { vectorSet.Rows[0].InputID = "unexpected-input" }, "unexpected input row"},
		{"dimension", func(vectorSet *EmbeddingVectorSetRecord) { vectorSet.Rows[0].Dimensions++ }, "row dimension mismatch"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
			canonical, err := normalizeEmbeddingSetRecord(cloneEmbeddingSetRecord(record))
			require.NoError(t, err)
			record.VectorSet.Rows = append([]EmbeddingVectorRowRecord(nil), canonical.VectorSet.Rows...)
			testCase.mutate(&record.VectorSet)
			record.VectorSet.ManifestChecksum = embeddingVectorManifestChecksum(record.VectorSet.Rows)
			err = s.StageEmbeddingSet(t.Context(), record)
			require.ErrorContains(t, err, testCase.want)
		})
	}
}

func TestEmbeddingCatalogRejectsPublishedV1GenerationAsPolicyAuthority(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	legacy, err := os.ReadFile("../../document/testdata/embedding-input-generation-v1.golden.json")
	require.NoError(t, err)
	record.InputGeneration.GenerationJSON = legacy
	err = s.StageEmbeddingSet(t.Context(), record)
	require.ErrorContains(t, err, "current policy-complete version")
}

func TestEmbeddingCoverageExcludesTrashedAndDetachedContent(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "required", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "required", InputKind: document.EmbeddingInputOriginalFile},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
	_, _, err := s.Trash(t.Context(), nodeID, revision)
	require.NoError(t, err)
	_, err = s.ActiveEmbeddingHead(t.Context(), EmbeddingHeadKey{
		ContentVersionID: versionID, BindingID: "required", InputKind: document.EmbeddingInputOriginalFile,
	})
	require.ErrorIs(t, err, ErrNotFound)

	coverage, err := s.EmbeddingCoverage(t.Context(), CoverageScope{
		ContentVersionIDs: []string{versionID}, ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings: []CoverageBinding{{BindingID: "required", InputKind: document.EmbeddingInputOriginalFile, Required: true}},
	})
	require.NoError(t, err)
	assert.Equal(t, CoverageIneligible, coverage.State)
}

func TestEmbeddingCoverageClassifiesDetachedAttachmentHeadIneligible(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM rendition_heads WHERE content_version_id=? AND profile_fingerprint=?`, versionID, profile.Fingerprint)
		return err
	}))
	coverage, err := s.EmbeddingCoverage(t.Context(), CoverageScope{
		ContentVersionIDs: []string{versionID}, ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings: []CoverageBinding{{BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk, Required: false, VectorSpaceID: record.VectorSpace.ID}},
	})
	require.NoError(t, err)
	require.Len(t, coverage.Optional, 1)
	assert.Equal(t, CoverageIneligible, coverage.Optional[0].State)

	corpus := EmbeddingCorpusGenerationRecord{
		ID: testSHA256([]byte("detached-corpus")), ContractVersion: EmbeddingCorpusGenerationV1,
		BindingID: "chunk", VectorSpaceID: record.VectorSpace.ID, SetIDs: []string{record.ID},
		ManifestChecksum: embeddingCorpusManifestChecksumForTest([]string{record.ID}), CreatedAt: embeddingCatalogTime,
	}
	require.ErrorContains(t, s.StageEmbeddingCorpusGeneration(t.Context(), corpus), "ineligible")
}

func TestEmbeddingCatalogCorpusManifestIsDeterministicAndBounded(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	var secondVersion string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? ORDER BY version_id LIMIT 1`, versionID, catalogSourceHash).Scan(&secondVersion))
	first := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	second := embeddingSetFixture(s, secondVersion, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), first))
	require.NoError(t, s.StageEmbeddingSet(t.Context(), second))
	for _, value := range []EmbeddingSetRecord{first, second} {
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			Key:   EmbeddingHeadKey{ContentVersionID: value.ContentVersionID, BindingID: value.BindingID, InputKind: value.InputKind},
			SetID: value.ID, VectorSpaceID: value.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	sortedIDs := []string{second.ID, first.ID}
	if strings.Compare(sortedIDs[0], sortedIDs[1]) > 0 {
		sortedIDs[0], sortedIDs[1] = sortedIDs[1], sortedIDs[0]
	}
	record := EmbeddingCorpusGenerationRecord{
		ID: testSHA256([]byte("corpus-generation")), ContractVersion: EmbeddingCorpusGenerationV1,
		BindingID: "optional", VectorSpaceID: first.VectorSpace.ID,
		SetIDs: []string{first.ID, second.ID}, ManifestChecksum: embeddingCorpusManifestChecksumForTest(sortedIDs),
		CreatedAt: embeddingCatalogTime,
	}
	require.NoError(t, s.StageEmbeddingCorpusGeneration(t.Context(), record))

	rows, err := s.db.Query(`SELECT embedding_set_id FROM embedding_corpus_members
		WHERE corpus_generation_id=? ORDER BY member_order`, record.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var stored []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		stored = append(stored, id)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, sortedIDs, stored)

	bad := record
	bad.ID = testSHA256([]byte("bad-corpus-generation"))
	bad.ManifestChecksum = fakeHash("bad")
	require.ErrorContains(t, s.StageEmbeddingCorpusGeneration(t.Context(), bad), "manifest")
}

func TestEmbeddingCatalogMetadataRoundTripsDeterministically(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), chunk))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk},
		SetID: chunk.ID, VectorSpaceID: chunk.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	var first, second bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &first))
	require.NoError(t, s.ExportMetadata(t.Context(), &second))
	assert.Equal(t, first.Bytes(), second.Bytes())
	assert.NotContains(t, first.String(), "synthetic source pdf")
	assert.NotContains(t, first.String(), "input text")

	restored := newTestStoreWithDriver(t, modernc.Driver{})
	require.NoError(t, restored.ImportMetadataForRestore(t.Context(), bytes.NewReader(first.Bytes())))
	var roundTrip bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &roundTrip))
	assert.Equal(t, first.Bytes(), roundTrip.Bytes())
	head, err := restored.ActiveEmbeddingHead(t.Context(), EmbeddingHeadKey{
		ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile,
	})
	require.NoError(t, err)
	assert.Equal(t, record.ID, head.Set.ID)
	chunkHead, err := restored.ActiveEmbeddingHead(t.Context(), EmbeddingHeadKey{
		ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk,
	})
	require.NoError(t, err)
	assert.Equal(t, chunk.ID, chunkHead.Set.ID)
}

func TestEmbeddingCatalogDerivativePurgeAndGCLeaveOriginalAuthority(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "optional", InputKind: document.EmbeddingInputOriginalFile},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))

	plan, err := s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plan.EmbeddingSets, "an active embedding head must retain its complete set")

	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM embedding_heads WHERE embedding_set_id=?`, record.ID)
		return err
	}))
	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
	_, _, err = s.Trash(t.Context(), nodeID, revision)
	require.NoError(t, err)
	plan, err = s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.EmbeddingSets, 1)
	assert.Equal(t, record.ID, plan.EmbeddingSets[0].SetID)
	assert.Equal(t, record.VectorSet.PayloadBlobHash, plan.EmbeddingSets[0].PayloadBlobHash)

	report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.RemovedEmbeddingSets)
	assert.Equal(t, 1, report.RemovedEmbeddingInputGenerations)
	assert.Equal(t, 1, report.RemovedEmbeddingVectorSets)
	assert.Contains(t, report.PhysicalDerivativeBlobsPendingGC, record.VectorSet.PayloadBlobHash)
	var versions, vectorRows int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE version_id=?),
		(SELECT COUNT(*) FROM embedding_vector_rows WHERE vector_set_id=?)`,
		versionID, record.VectorSet.ID).Scan(&versions, &vectorRows))
	assert.Equal(t, 1, versions, "derivative collection must not remove original content authority")
	assert.Zero(t, vectorRows)
}

func TestEmbeddingCatalogRootsRetainEveryAuthorityTransitively(t *testing.T) {
	testCases := []struct {
		name       string
		kind       CurrentRenditionRootKind
		targetKind CurrentRenditionTargetKind
		target     func(EmbeddingSetRecord) string
		lease      bool
	}{
		{"reader lease set", RenditionRootReaderLease, RenditionRootEmbeddingSet, func(value EmbeddingSetRecord) string { return value.ID }, true},
		{"worker lease generation", RenditionRootWorkerLease, RenditionRootEmbeddingGeneration, func(value EmbeddingSetRecord) string { return value.InputGeneration.ID }, true},
		{"policy vector set", RenditionRootPolicyPin, RenditionRootEmbeddingVectorSet, func(value EmbeddingSetRecord) string { return value.VectorSet.ID }, false},
		{"restore payload", RenditionRootRestorePin, RenditionRootEmbeddingPayload, func(value EmbeddingSetRecord) string { return value.VectorSet.PayloadBlobHash }, false},
		{"backup set", RenditionRootBackupPin, RenditionRootEmbeddingSet, func(value EmbeddingSetRecord) string { return value.ID }, false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
			var nodeID, revision int64
			require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
			_, _, err := s.Trash(t.Context(), nodeID, revision)
			require.NoError(t, err)
			root := CurrentEmbeddingRoot{
				ID: "embedding-root-" + strings.ReplaceAll(testCase.name, " ", "-"), Kind: testCase.kind,
				TargetKind: testCase.targetKind, TargetID: testCase.target(record), FencingToken: 1,
				RecordedAt: embeddingCatalogTime,
			}
			if testCase.lease {
				root.ExpiresAt = "2099-08-25T10:00:00.000000000Z"
			}
			require.NoError(t, s.PutCurrentEmbeddingRoot(t.Context(), root))
			plan, err := s.DerivativeGCPlan(t.Context())
			require.NoError(t, err)
			assert.Empty(t, plan.EmbeddingSets)
			report, err := s.PurgeDerivatives(t.Context(), PurgeRequest{})
			require.NoError(t, err)
			assert.Zero(t, report.RemovedEmbeddingSets)
			released, err := s.ReleaseCurrentEmbeddingRoot(t.Context(), root.ID, root.FencingToken)
			require.NoError(t, err)
			assert.True(t, released)
			plan, err = s.DerivativeGCPlan(t.Context())
			require.NoError(t, err)
			require.Len(t, plan.EmbeddingSets, 1)
			assert.Equal(t, record.ID, plan.EmbeddingSets[0].SetID)
		})
	}
	t.Run("expired lease", func(t *testing.T) {
		s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
		record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		var nodeID, revision int64
		require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
		_, _, err := s.Trash(t.Context(), nodeID, revision)
		require.NoError(t, err)
		root := CurrentEmbeddingRoot{
			ID: "expired-embedding-reader", Kind: RenditionRootReaderLease,
			TargetKind: RenditionRootEmbeddingSet, TargetID: record.ID, FencingToken: 1,
			RecordedAt: embeddingCatalogTime, ExpiresAt: "2020-08-25T10:00:00.000000000Z",
		}
		require.NoError(t, s.PutCurrentEmbeddingRoot(t.Context(), root))
		plan, err := s.DerivativeGCPlan(t.Context())
		require.NoError(t, err)
		require.Len(t, plan.EmbeddingSets, 1)
		assert.Contains(t, plan.ExpiredRootIDs, root.ID)
	})
}

func TestEmbeddingCatalogVersionPruneDeletesDirectAndChunkAuthority(t *testing.T) {
	s, firstVersion, profile, firstAttachment := newEmbeddingCatalogFixture(t)
	var versionID, buildID string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? ORDER BY version_id LIMIT 1`, firstVersion, catalogSourceHash).Scan(&versionID))
	require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, firstAttachment).Scan(&buildID))
	attachmentID := testSHA256([]byte("pruned-embedding-attachment"))
	require.NoError(t, s.AttachRenditionBuild(t.Context(), RenditionAttachmentRecord{
		ID: attachmentID, VaultID: s.VaultID(), ContentVersionID: versionID, BuildID: buildID,
		Profile: profile, AttachedAt: embeddingCatalogTime,
	}))
	require.NoError(t, s.PublishRenditionHead(t.Context(), RenditionHeadRecord{
		ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachmentID, PublishedAt: embeddingCatalogTime,
	}))
	for _, record := range []EmbeddingSetRecord{
		embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", ""),
		embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID),
	} {
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	var nodeID, revision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
	replacementHash := fakeHash("e3")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return s.EnsureBlobTx(tx, replacementHash, 24) }))
	updated, _, err := s.ReplaceContent(t.Context(), nodeID, revision, replacementHash, 24, "application/pdf")
	require.NoError(t, err)
	result, err := s.PruneContentVersions(t.Context(), nodeID, updated.Revision, VersionPruneSelector{VersionIDs: []string{versionID}}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result.DeletedVersions)
	var remaining int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM content_versions WHERE version_id=?) +
		(SELECT COUNT(*) FROM embedding_sets WHERE content_version_id=?) +
		(SELECT COUNT(*) FROM embedding_input_generations WHERE source_version_id=?) +
		(SELECT COUNT(*) FROM rendition_attachments WHERE attachment_id=?)`,
		versionID, versionID, versionID, attachmentID).Scan(&remaining))
	assert.Zero(t, remaining)
}

func TestEmbeddingCatalogVersionPrunePreservesSharedAuthorityRoots(t *testing.T) {
	s, sourceVersion, profile, _ := newEmbeddingCatalogFixture(t)
	var deletedVersion string
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? ORDER BY version_id LIMIT 1`, sourceVersion, catalogSourceHash).Scan(&deletedVersion))
	third, err := s.CreateFile(t.Context(), s.RootID(), "shared-embedding-consumer.pdf", catalogSourceHash, int64(len(catalogBlobContents[catalogSourceHash])), "application/pdf")
	require.NoError(t, err)
	record := embeddingSetFixture(s, sourceVersion, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	deletedSetID := fakeHash("deleted-shared-version-embedding-set")
	survivingSetID := fakeHash("surviving-shared-version-embedding-set")
	for setID, versionID := range map[string]string{deletedSetID: deletedVersion, survivingSetID: third.CurrentVersionID} {
		_, err = s.db.Exec(`INSERT INTO embedding_sets(
		embedding_set_id,vault_uid,binding_id,input_kind,content_version_id,
		profile_fingerprint,embedding_input_fingerprint,vector_space_id,
		input_generation_id,vector_set_id,created_at)
		SELECT ?,vault_uid,binding_id,input_kind,?,profile_fingerprint,
		embedding_input_fingerprint,vector_space_id,input_generation_id,vector_set_id,created_at
		FROM embedding_sets WHERE embedding_set_id=?`, setID, versionID, record.ID)
		require.NoError(t, err)
	}

	roots := []CurrentEmbeddingRoot{
		{ID: "deleted-set-pin", Kind: RenditionRootBackupPin, TargetKind: RenditionRootEmbeddingSet, TargetID: deletedSetID, FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "shared-generation-lease", Kind: RenditionRootReaderLease, TargetKind: RenditionRootEmbeddingGeneration, TargetID: record.InputGeneration.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime, ExpiresAt: "2099-08-25T10:00:00.000000000Z"},
		{ID: "shared-vector-pin", Kind: RenditionRootPolicyPin, TargetKind: RenditionRootEmbeddingVectorSet, TargetID: record.VectorSet.ID, FencingToken: 1, RecordedAt: embeddingCatalogTime},
		{ID: "shared-payload-pin", Kind: RenditionRootRestorePin, TargetKind: RenditionRootEmbeddingPayload, TargetID: record.VectorSet.PayloadBlobHash, FencingToken: 1, RecordedAt: embeddingCatalogTime},
	}
	for _, root := range roots {
		require.NoError(t, s.PutCurrentEmbeddingRoot(t.Context(), root))
	}

	var deletedNodeID, deletedRevision int64
	require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, deletedVersion).Scan(&deletedNodeID, &deletedRevision))
	replacementHash := fakeHash("shared-authority-replacement")
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error { return s.EnsureBlobTx(tx, replacementHash, 24) }))
	updated, _, err := s.ReplaceContent(t.Context(), deletedNodeID, deletedRevision, replacementHash, 24, "application/pdf")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), deletedNodeID, updated.Revision, VersionPruneSelector{VersionIDs: []string{deletedVersion}}, true)
	require.NoError(t, err)

	var survivingAuthorities, survivingRoots, deletedSetRoots int
	require.NoError(t, s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM embedding_sets WHERE embedding_set_id=?) +
		(SELECT COUNT(*) FROM embedding_input_generations WHERE generation_id=?) +
		(SELECT COUNT(*) FROM embedding_vector_sets WHERE vector_set_id=?)`,
		survivingSetID, record.InputGeneration.ID, record.VectorSet.ID).Scan(&survivingAuthorities))
	require.Equal(t, 3, survivingAuthorities)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM current_rendition_roots WHERE root_id IN (?,?,?)`,
		roots[1].ID, roots[2].ID, roots[3].ID).Scan(&survivingRoots))
	require.Equal(t, 3, survivingRoots)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM current_rendition_roots WHERE root_id=?`, roots[0].ID).Scan(&deletedSetRoots))
	assert.Zero(t, deletedSetRoots)

	for _, versionID := range []string{sourceVersion, third.CurrentVersionID} {
		var nodeID, revision int64
		require.NoError(t, s.db.QueryRow(`SELECT id,revision FROM nodes WHERE current_version_id=?`, versionID).Scan(&nodeID, &revision))
		_, _, err = s.Trash(t.Context(), nodeID, revision)
		require.NoError(t, err)
	}
	plan, err := s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plan.EmbeddingSets, "shared generation/vector/payload roots must retain their surviving consumer")
}

func TestEmbeddingCatalogMetadataRejectsCorruptionAndOversizedCounts(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))

	for _, testCase := range []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{"unknown contract", func(value string) string {
			return strings.Replace(value, EmbeddingVectorSpaceContractV1, "embedding-vector-space/v2", 1)
		}, "constraint"},
		{"oversized count", func(value string) string {
			return strings.Replace(value, `"input_count":1`, `"input_count":100001`, 1)
		}, "exceeds bounds"},
		{"missing input row", func(value string) string {
			lines := strings.Split(value, "\n")
			for index, line := range lines {
				if strings.Contains(line, `"type":"embedding_generation_input"`) {
					return strings.Join(append(lines[:index], lines[index+1:]...), "\n")
				}
			}
			return value
		}, "count"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			target := newTestStore(t)
			err := target.ImportMetadataForRestore(t.Context(), strings.NewReader(testCase.mutate(exported.String())))
			require.ErrorContains(t, err, testCase.want)
			var sets int
			require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM embedding_sets`).Scan(&sets))
			assert.Zero(t, sets, "corrupt import must roll back atomically")
		})
	}
}

func TestEmbeddingCoverageRejectsAmplifiedScopeBeforeQueryWork(t *testing.T) {
	s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
	versions := make([]string, 1001)
	bindings := make([]CoverageBinding, 1001)
	for index := range versions {
		versions[index] = versionID
		bindings[index] = CoverageBinding{
			BindingID: fmt.Sprintf("binding-%04d", index), InputKind: document.EmbeddingInputOriginalFile,
		}
	}
	_, err := s.EmbeddingCoverage(t.Context(), CoverageScope{
		ContentVersionIDs: versions, ProcessingProfileFingerprint: profile.Fingerprint,
		Bindings: bindings,
	})
	require.ErrorContains(t, err, "work")
}

func TestEmbeddingCatalogSchemaCorruptionFailsClosedOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	driver := DefaultSQLiteDriver()
	db, err := driver.Open(path, docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate,
	})
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE embedding_sets ADD COLUMN unexpected TEXT`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(path, driver)
	require.ErrorContains(t, err, "embedding catalog schema")
}

const embeddingCatalogTime = "2026-08-25T10:00:00.000000000Z"

func newEmbeddingCatalogFixture(t *testing.T) (*Store, string, ProcessingProfileRecord, string) {
	t.Helper()
	s, versions := newRenditionCatalogFixture(t)
	profile := embeddingCatalogProfile(t)
	build := catalogRenditionBuild(s, profile)
	build.EvidenceChecksum = embeddingCatalogEvidence(t).Checksum
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	attachment := RenditionAttachmentRecord{
		ID: catalogAttachmentFirst, VaultID: s.VaultID(), ContentVersionID: versions[0],
		BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime,
	}
	require.NoError(t, s.AttachRenditionBuild(t.Context(), attachment))
	require.NoError(t, s.PublishRenditionHead(t.Context(), RenditionHeadRecord{
		ContentVersionID: versions[0], ProcessingProfileFingerprint: profile.Fingerprint,
		AttachmentID: attachment.ID, PublishedAt: embeddingCatalogTime,
	}))
	return s, versions[0], profile, attachment.ID
}

func embeddingSetFixture(
	s *Store, versionID, profileFingerprint string, kind EmbeddingInputKind, bindingID, attachmentID string,
) EmbeddingSetRecord {
	seed := bindingID + string(kind) + versionID + attachmentID
	var canonicalProfile string
	if err := s.db.QueryRow(`SELECT canonical_profile FROM processing_profiles WHERE profile_fingerprint=?`, profileFingerprint).Scan(&canonicalProfile); err != nil {
		panic(err)
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal([]byte(canonicalProfile), &profile); err != nil {
		panic(err)
	}
	_, fingerprints, err := document.CanonicalProfile(profile)
	if err != nil {
		panic(err)
	}
	var binding document.EmbeddingBindingV1
	for _, candidate := range profile.Embeddings {
		if candidate.Name == bindingID {
			binding = candidate
			break
		}
	}
	descriptor := embeddingCatalogDescriptor()
	space := EmbeddingVectorSpaceRecord{
		ID: fingerprints.VectorSpace[bindingID], ContractVersion: EmbeddingVectorSpaceContractV1,
		Descriptor: descriptor,
	}
	generation := EmbeddingInputGenerationRecord{
		ID:              testSHA256([]byte(seed + "generation")),
		SourceVersionID: versionID, ProcessingProfileFingerprint: profileFingerprint,
		EvidenceFingerprint: testSHA256([]byte(seed + "evidence")), TokenizerFingerprint: testSHA256([]byte(seed + "tokenizer")),
		ChunkPolicyFingerprint: testSHA256([]byte(seed + "chunk-policy")), FormatterFingerprint: testSHA256([]byte(seed + "formatter")),
		AttachmentID: attachmentID, GenerationChecksum: catalogSourceHash,
		CreatedAt: embeddingCatalogTime,
	}
	if kind == document.EmbeddingInputRenditionChunk {
		generated := embeddingCatalogGeneration(profile, fingerprints.EvidenceLexical, attachmentID)
		generation.ID = testSHA256([]byte("embedding-generation-attachment/v1\x00" + generated.Checksum + "\x00" + attachmentID))
		generation.GenerationJSON, err = json.Marshal(generated)
		if err != nil {
			panic(err)
		}
		generation.GenerationBlobHash = testSHA256(generation.GenerationJSON)
		generation.GenerationEncodedSize = int64(len(generation.GenerationJSON))
		generation.GenerationChecksum = generated.Checksum
		if err := s.withStorageTx(context.Background(), func(tx *sql.Tx) error {
			return s.EnsureBlobTx(tx, generation.GenerationBlobHash, generation.GenerationEncodedSize)
		}); err != nil {
			panic(err)
		}
		generation.Inputs = make([]EmbeddingInputReference, len(generated.Inputs))
		for index, input := range generated.Inputs {
			generation.Inputs[index] = EmbeddingInputReference{ID: input.Key, RenderedChecksum: input.Checksum}
		}
	} else {
		generation.Inputs = []EmbeddingInputReference{{ID: versionID, RenderedChecksum: catalogSourceHash}}
	}
	expectedKeys := make([]string, len(generation.Inputs))
	expectedChecksums := make([]string, len(generation.Inputs))
	if kind == document.EmbeddingInputRenditionChunk {
		var generated document.EmbeddingInputGeneration
		if err := json.Unmarshal(generation.GenerationJSON, &generated); err != nil {
			panic(err)
		}
		for index, input := range generated.Inputs {
			expectedKeys[index], expectedChecksums[index] = input.Key, input.Checksum
		}
	} else {
		expectedKeys[0], expectedChecksums[0] = versionID, catalogSourceHash
	}
	values := make([][]float64, len(expectedKeys))
	for index := range values {
		values[index] = []float64{float64(index + 1), 2, 3, 4, 5, 6, 7, 8}
	}
	canonicalVectors, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: space.ID, Metric: binding.Metric, Normalization: binding.Normalization,
		Dimension: binding.Dimensions, InputKeys: expectedKeys, InputChecksums: expectedChecksums, Values: values,
	})
	if err != nil {
		panic(err)
	}
	payload, payloadChecksum, err := document.EncodeVectorSetV1(canonicalVectors)
	if err != nil {
		panic(err)
	}
	payloadHash := testSHA256(payload)
	if err := s.withStorageTx(context.Background(), func(tx *sql.Tx) error { return s.EnsureBlobTx(tx, payloadHash, int64(len(payload))) }); err != nil {
		panic(err)
	}
	vectorSet := EmbeddingVectorSetRecord{
		ID: payloadChecksum, ContractVersion: EmbeddingVectorSetContractV1, VectorSpaceID: space.ID,
		PayloadBlobHash: payloadHash, PayloadChecksum: payloadChecksum, Payload: payload,
	}
	return EmbeddingSetRecord{
		ID: testSHA256([]byte(seed + "set")), VaultID: s.VaultID(), BindingID: bindingID, InputKind: kind,
		ContentVersionID: versionID, ProcessingProfileFingerprint: profileFingerprint,
		EmbeddingInputFingerprint: fingerprints.EmbeddingInput[bindingID],
		VectorSpace:               space, InputGeneration: generation, VectorSet: vectorSet, CreatedAt: embeddingCatalogTime,
	}
}

type embeddingCatalogTokenizer struct{}

func (embeddingCatalogTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "catalog-runes", Revision: "v1", PrefixTokenCountsMonotonic: true}
}

func (embeddingCatalogTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	runes := []rune(text)
	if len(runes) > limit {
		return nil, document.ErrTokenizerLimit
	}
	result := make([]document.TokenBoundary, len(runes))
	for index := range runes {
		result[index] = document.TokenBoundary{Start: index, End: index + 1}
	}
	return result, nil
}

func embeddingCatalogEvidence(t *testing.T) document.NormalizedEvidenceV1 {
	t.Helper()
	policy, err := document.NewEvidencePolicy(4096)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{
			{Order: 0, Text: "Synthetic evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}},
			{Order: 1, Text: "Second evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 2, End: 2}},
		},
	}, policy)
	require.NoError(t, err)
	return evidence
}

func embeddingCatalogDescriptor() document.EmbeddingDescriptor {
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	if err != nil {
		panic(err)
	}
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-embedding", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: fakeHash("e1"), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-v1", ModelRevision: "2026-08-25", Dimension: 8,
		Metric: document.VectorMetricCosine, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds:      []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk},
		CompatibilityID: "synthetic-space", SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	if err != nil {
		panic(err)
	}
	return descriptor
}

func embeddingCatalogProfile(t *testing.T) ProcessingProfileRecord {
	t.Helper()
	descriptor := embeddingCatalogDescriptor()
	base := catalogProcessingProfile(t, false)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(base.CanonicalProfile, &profile))
	makeBinding := func(name string, activation document.EmbeddingActivation, kind document.EmbeddingInputKind) document.EmbeddingBindingV1 {
		binding := document.EmbeddingBindingV1{
			Activation: activation, AuthorizationFingerprint: fakeHash("d1"), CompatibilityID: descriptor.CompatibilityID,
			CredentialBinding: "credential:catalog-embedding", Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			Dimensions: descriptor.Dimension, DisclosureFingerprint: fakeHash("d3"), DocumentFormatter: descriptor.DocumentFormatter,
			InputKind: kind, MaxBatchItems: 128, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
			Metric: descriptor.Metric, Model: descriptor.Model, ModelInput: descriptor.ModelInput, Name: name,
			Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
			ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
		}
		if kind == document.EmbeddingInputRenditionChunk {
			binding.Chunk = &document.EmbeddingChunkPolicyV1{ContextFingerprint: fakeHash("d4"), Formatter: "evidence-text/v1", MaxTokens: 128, OverlapTokens: 0, Tokenizer: "catalog-runes@v1", TruncationPolicy: string(document.TruncationPolicyReject)}
		}
		return binding
	}
	profile.Embeddings = []document.EmbeddingBindingV1{
		makeBinding("optional", document.EmbeddingOptional, document.EmbeddingInputOriginalFile),
		makeBinding("required", document.EmbeddingRequired, document.EmbeddingInputOriginalFile),
		makeBinding("chunk", document.EmbeddingOptional, document.EmbeddingInputRenditionChunk),
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: canonical,
		RenditionRequestFingerprint: fingerprints.RenditionRequest, EvidenceLexicalFingerprint: fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func embeddingCatalogGeneration(profile document.ProcessingProfileV1, lexicalFingerprint, attachmentID string) document.EmbeddingInputGeneration {
	policy, err := document.NewEvidencePolicy(4096)
	if err != nil {
		panic(err)
	}
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{
			{Order: 0, Text: "Synthetic evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}},
			{Order: 1, Text: "Second evidence", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 2, End: 2}},
		},
	}, policy)
	if err != nil {
		panic(err)
	}
	binding := profile.Embeddings[0]
	for _, candidate := range profile.Embeddings {
		if candidate.Name == "chunk" {
			binding = candidate
		}
	}
	inputPolicy := document.InputPolicy{
		Tokenizer: embeddingCatalogTokenizer{}, ContentTokenBudget: 128, OverlapTokens: 0,
		MaxProviderTokens: 512, MaxProviderBytes: 1 << 20, MaxGeneratedInputs: 128,
		MaxTotalContentTokens: 4096, MaxTotalRenderedTokens: 8192, MaxTotalContentBytes: 1 << 20,
		MaxTotalRenderedBytes: 2 << 20, MaxFittingWorkTokens: 1 << 20, MaxFittingWorkBytes: 8 << 20,
		ModelInput: binding.ModelInput, Formatter: binding.Chunk.Formatter,
		LexicalEvidenceFingerprint: lexicalFingerprint, ContextFingerprint: binding.Chunk.ContextFingerprint,
		TruncationPolicy: document.TruncationPolicyReject,
	}
	if attachmentID != "" {
		context, contextErr := document.NewAttachmentContextSnapshot(document.AttachmentContextSnapshotConfig{Title: "Synthetic title", Context: "Synthetic context"})
		if contextErr != nil {
			panic(contextErr)
		}
		inputPolicy.AttachmentContext = &context
	}
	generation, err := document.BuildEmbeddingInputs(evidence, inputPolicy)
	if err != nil {
		panic(err)
	}
	return generation
}

func cloneEmbeddingSetRecord(value EmbeddingSetRecord) EmbeddingSetRecord {
	clone := value
	clone.InputGeneration.GenerationJSON = append([]byte(nil), value.InputGeneration.GenerationJSON...)
	clone.InputGeneration.Inputs = append([]EmbeddingInputReference(nil), value.InputGeneration.Inputs...)
	clone.VectorSet.Payload = append([]byte(nil), value.VectorSet.Payload...)
	clone.VectorSet.Rows = append([]EmbeddingVectorRowRecord(nil), value.VectorSet.Rows...)
	return clone
}

func embeddingCorpusManifestChecksumForTest(setIDs []string) string {
	return testSHA256([]byte(strings.Join(setIDs, "\n") + "\n"))
}
