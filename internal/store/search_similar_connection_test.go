package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestConnectionSelectedVectorSpaceRejectsModelDimensionAndRecipeMismatch(t *testing.T) {
	s, sourceVersion, record, sourceAttachment := newEmbeddingCatalogFixture(t)
	view, err := s.ActiveRendition(t.Context(), sourceVersion, record.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, sourceAttachment, view.Attachment.ID)
	var baseline document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(record.CanonicalProfile, &baseline))
	_, selected, err := document.CanonicalProfile(baseline)
	require.NoError(t, err)
	const bindingName = "chunk"
	selectedSpace := selected.VectorSpace[bindingName]
	require.True(t, document.SameVectorSpace(selectedSpace, selectedSpace))

	tests := []struct {
		name   string
		mutate func(*document.EmbeddingBindingV1)
	}{
		{name: "model", mutate: func(binding *document.EmbeddingBindingV1) { binding.Model = "synthetic-v2" }},
		{name: "dimension", mutate: func(binding *document.EmbeddingBindingV1) { binding.Dimensions++ }},
		{name: "model input recipe", mutate: func(binding *document.EmbeddingBindingV1) {
			contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
				Profile: document.ModelInputProfileCustom, CompatibilityID: binding.CompatibilityID,
				Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document-v2: {{content}}"},
				Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
			})
			require.NoError(t, err)
			binding.ModelInput = contract
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var candidate document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(record.CanonicalProfile, &candidate))
			for i := range candidate.Embeddings {
				if candidate.Embeddings[i].Name == bindingName {
					test.mutate(&candidate.Embeddings[i])
				}
			}
			_, fingerprints, err := document.CanonicalProfile(candidate)
			require.NoError(t, err)
			candidateSpace := fingerprints.VectorSpace[bindingName]
			require.NotEqual(t, selectedSpace, candidateSpace, "selected %s change must rotate the vector space", test.name)
			assert.False(t, document.SameVectorSpace(selectedSpace, candidateSpace), "incompatible %s vectors compared", test.name)
		})
	}
}

func TestConnectionSearchSelectsChunkRowsAndScopesDuplicates(t *testing.T) {
	s, sourceVersion, profile, sourceAttachment := newEmbeddingCatalogFixture(t)
	source, err := s.ContentVersionByID(t.Context(), sourceVersion)
	require.NoError(t, err)
	var buildID, copyVersion string
	require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, sourceAttachment).Scan(&buildID))
	require.NoError(t, s.db.QueryRow(`SELECT version_id FROM content_versions WHERE version_id<>? AND blob_hash=? LIMIT 1`,
		sourceVersion, catalogSourceHash).Scan(&copyVersion))
	copyAttachment := RenditionAttachmentRecord{ID: testSHA256([]byte("copy-attachment")), VaultID: s.VaultID(),
		ContentVersionID: copyVersion, BuildID: buildID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, publishAttachmentForTest(t, s, copyAttachment))
	third, err := s.CreateFile(t.Context(), s.RootID(), "third-copy.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	thirdAttachment := RenditionAttachmentRecord{ID: testSHA256([]byte("third-copy-attachment")), VaultID: s.VaultID(),
		ContentVersionID: third.CurrentVersionID, BuildID: buildID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, publishAttachmentForTest(t, s, thirdAttachment))
	fourth, err := s.CreateFile(t.Context(), s.RootID(), "fourth-copy.pdf", catalogSourceHash, 20, "application/pdf")
	require.NoError(t, err)
	fourthAttachment := RenditionAttachmentRecord{ID: testSHA256([]byte("fourth-copy-attachment")), VaultID: s.VaultID(),
		ContentVersionID: fourth.CurrentVersionID, BuildID: buildID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, publishAttachmentForTest(t, s, fourthAttachment))
	versions := []string{sourceVersion, copyVersion, third.CurrentVersionID, fourth.CurrentVersionID}
	attachments := []string{sourceAttachment, copyAttachment.ID, thirdAttachment.ID, fourthAttachment.ID}
	var records []EmbeddingSetRecord
	for i, version := range versions {
		record := embeddingSetFixture(s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachments[i])
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{FencingToken: 1,
			Key:   EmbeddingHeadKey{ContentVersionID: version, BindingID: "chunk", InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime}))
		records = append(records, record)
	}
	set, _, err := document.DecodeVectorSetV1(records[0].VectorSet.Payload, document.VectorBounds{
		MaxRows: 100, MaxDimension: records[0].VectorSpace.Descriptor.Dimension,
		MaxBytes: len(records[0].VectorSet.Payload)})
	require.NoError(t, err)
	manifest, err := vectorindex.NewManifest([]string{records[0].VectorSet.ID})
	require.NoError(t, err)
	index, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	indexSource, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
	require.NoError(t, err)
	require.NoError(t, putVectorIndexGenerationForTest(t, s, VectorIndexGenerationRecord{
		ID: hashVectorIndexTest("connection-search-generation"), VectorSpaceID: records[0].VectorSpace.ID,
		SourceManifestChecksum: indexSource.ManifestChecksum, IndexManifestChecksum: index.Metadata().Manifest.Checksum,
		Bytes: index.Bytes(), RowCount: index.Metadata().RowCount, BuiltAt: embeddingCatalogTime}))

	seed := SimilarSource{NodeID: source.NodeID, ContentVersionID: sourceVersion,
		InputIDs: []string{records[0].InputGeneration.Inputs[0].ID}}
	fence := SearchOptions{ContentVersionIDs: versions}
	authority, err := s.AcquireSimilarSearchAuthority(t.Context(), profile.Fingerprint, "chunk", "connection-test",
		time.Now().UTC(), time.Minute, fence, seed)
	require.NoError(t, err)
	require.Len(t, authority.SourceRows, 1)
	assert.Equal(t, seed.InputIDs[0], authority.SourceRows[0].InputKey)
	require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), authority.Lease.ID,
		authority.Lease.FencingToken, time.Now().UTC()))
	result, metric, generation, err := s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10, fence)
	require.NoError(t, err)
	assert.Equal(t, document.VectorMetricCosine, metric)
	assert.NotEmpty(t, generation)
	require.Len(t, result.Candidates, 1)
	assert.NotEqual(t, source.NodeID, result.Candidates[0].NodeID)
	assert.Equal(t, seed.InputIDs[0], result.Candidates[0].SourceInputID)
	assert.Equal(t, 2, result.Candidates[0].DuplicateCount)
	require.Len(t, result.Candidates[0].DuplicateMembers, 2)
	memberVersions := make(map[string]bool)
	for _, member := range result.Candidates[0].DuplicateMembers {
		assert.NotEqual(t, result.Candidates[0].NodeID, member.NodeID)
		assert.Contains(t, []string{copyVersion, third.CurrentVersionID, fourth.CurrentVersionID}, member.ContentVersionID)
		assert.Equal(t, records[0].VectorSpace.ID, member.VectorSpaceID)
		assert.NotEmpty(t, member.EmbeddingSetID)
		assert.NotEmpty(t, member.InputGenerationID)
		assert.Equal(t, seed.InputIDs[0], member.SourceInputID)
		assert.NotEmpty(t, member.InputID)
		assert.NotEmpty(t, member.Path)
		memberVersions[member.ContentVersionID] = true
	}
	assert.False(t, memberVersions[result.Candidates[0].ContentVersionID])
	assert.Len(t, memberVersions, 2)
	assert.Equal(t, 4, result.ScopedDocuments)
	allInputs := make([]string, len(records[0].InputGeneration.Inputs))
	for i, input := range records[0].InputGeneration.Inputs {
		allInputs[i] = input.ID
	}
	seed.InputIDs = allInputs
	result, _, _, err = s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10, fence)
	require.NoError(t, err)
	require.Len(t, result.Candidates, 1, "explicit document seed scores retained chunk rows")
	seed.InputIDs = []string{records[0].InputGeneration.Inputs[0].ID}
	result, _, _, err = s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10, SearchOptions{})
	require.ErrorIs(t, err, ErrInvalidProcessingSourceFence, "member enumeration requires an explicit bounded fence")
	assert.Empty(t, result.Candidates)
	result, _, _, err = s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10,
		SearchOptions{ContentVersionIDs: []string{sourceVersion, copyVersion}})
	require.NoError(t, err)
	require.Len(t, result.Candidates, 1)
	assert.Zero(t, result.Candidates[0].DuplicateCount)
	assert.Empty(t, result.Candidates[0].DuplicateMembers)

	result, _, _, err = s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10,
		SearchOptions{ContentVersionIDs: []string{sourceVersion}})
	require.NoError(t, err)
	assert.Empty(t, result.Candidates)
	assert.Equal(t, 1, result.ScopedDocuments)

	result, _, _, err = s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10,
		SearchOptions{ContentVersionIDs: []string{copyVersion}})
	require.ErrorIs(t, err, ErrInvalidProcessingSourceFence)
	assert.Empty(t, result.Candidates)

	seed.InputIDs = []string{"absent-input"}
	result, _, _, err = s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10, fence)
	require.ErrorIs(t, err, ErrSimilarSourceUnavailable)
	assert.Empty(t, result.Candidates)
	seed.InputIDs = []string{records[0].InputGeneration.Inputs[0].ID}
	_, _, err = s.ReplaceContent(t.Context(), source.NodeID, UnconditionalRev,
		fakeHash("changed-source"), 20, "application/pdf")
	require.NoError(t, err)
	result, _, _, err = s.SearchConnectionCandidates(t.Context(), profile.Fingerprint, "chunk", seed, 10, fence)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, result.Candidates)
	_, err = s.ResolveSimilarCandidates(t.Context(), profile.Fingerprint, "chunk",
		document.EmbeddingInputRenditionChunk, records[0].VectorSpace.ID, indexSource.ManifestChecksum,
		nil, 10, fence, seed)
	require.ErrorIs(t, err, ErrProcessingSourceFenceStaleVersion,
		"a source changed after index acquisition must not be resolved against the old version")
}
