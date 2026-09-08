package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestSearcherRealStoreRevalidatesPathMutationDuringQueryEncoding(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *retrievalStoreFixture)
	}{
		{name: "unchanged", mutate: func(*testing.T, *retrievalStoreFixture) {}},
		{name: "rename file", mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
			t.Helper()
			_, _, err := fixture.store.Move(t.Context(), fixture.target.ID,
				fixture.parent.ID, "renamed.txt", fixture.target.Revision)
			require.NoError(t, err)
		}},
		{name: "move file", mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
			t.Helper()
			destination, err := fixture.store.Mkdir(t.Context(), fixture.store.RootID(), "destination")
			require.NoError(t, err)
			_, _, err = fixture.store.Move(t.Context(), fixture.target.ID,
				destination.ID, fixture.target.Name, fixture.target.Revision)
			require.NoError(t, err)
		}},
		{name: "rename ancestor without changing descendant revision", mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
			t.Helper()
			before, err := fixture.store.NodeByID(t.Context(), fixture.target.ID)
			require.NoError(t, err)
			parent, err := fixture.store.NodeByID(t.Context(), fixture.parent.ID)
			require.NoError(t, err)
			_, _, err = fixture.store.Move(t.Context(), parent.ID, fixture.store.RootID(), "renamed-parent", parent.Revision)
			require.NoError(t, err)
			after, err := fixture.store.NodeByID(t.Context(), fixture.target.ID)
			require.NoError(t, err)
			assert.Equal(t, before.Revision, after.Revision)
		}},
		{name: "move ancestor without changing descendant revision", mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
			t.Helper()
			before, err := fixture.store.NodeByID(t.Context(), fixture.target.ID)
			require.NoError(t, err)
			destination, err := fixture.store.Mkdir(t.Context(), fixture.store.RootID(), "destination")
			require.NoError(t, err)
			parent, err := fixture.store.NodeByID(t.Context(), fixture.parent.ID)
			require.NoError(t, err)
			_, _, err = fixture.store.Move(t.Context(), parent.ID, destination.ID, parent.Name, parent.Revision)
			require.NoError(t, err)
			after, err := fixture.store.NodeByID(t.Context(), fixture.target.ID)
			require.NoError(t, err)
			assert.Equal(t, before.Revision, after.Revision)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetrievalStoreFixture(t)
			provider := fixture.provider(func() { test.mutate(t, fixture) })
			searcher := fixture.searcher(t, provider)

			report, err := searcher.Search(t.Context(), fixture.query(ModeHybrid, 1, store.SearchOptions{}))

			require.NoError(t, err)
			assert.Equal(t, 1, provider.calls)
			if test.name == "unchanged" {
				require.Len(t, report.Results, 1)
				assert.Equal(t, fixture.target.ID, report.Results[0].Document.NodeID)
				assert.Equal(t, "/docs/match-alpha.txt", report.Results[0].Path)
				return
			}
			assert.Empty(t, report.Results, "the originally collected path must not escape")
			assert.True(t, report.Truncated)
		})
	}
}

func TestSearcherRealStoreRejectsSourceMutationDuringQueryEncoding(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *retrievalStoreFixture)
	}{
		{name: "replace content", mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
			t.Helper()
			contents := []byte("synthetic replacement")
			_, _, err := fixture.store.ReplaceContent(t.Context(), fixture.target.ID, fixture.target.Revision,
				retrievalHashBytes(contents), int64(len(contents)), "text/plain")
			require.NoError(t, err)
		}},
		{name: "trash", mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
			t.Helper()
			_, _, err := fixture.store.Trash(t.Context(), fixture.target.ID, fixture.target.Revision)
			require.NoError(t, err)
		}},
		{name: "replace embedding head", mutate: func(t *testing.T, fixture *retrievalStoreFixture) {
			t.Helper()
			fixture.replaceEmbeddingHead(t, fixture.target, []float64{0, 1})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRetrievalStoreFixture(t)
			provider := fixture.provider(func() { test.mutate(t, fixture) })
			searcher := fixture.searcher(t, provider)

			_, err := searcher.Search(t.Context(), fixture.query(ModeHybrid, 1, store.SearchOptions{}))

			require.ErrorIs(t, err, store.ErrVectorIndexSourceStale)
			assert.Equal(t, 1, provider.calls)
		})
	}
}

func TestSearcherRealStoreRevalidatesRevokedScopeDuringQueryEncoding(t *testing.T) {
	fixture := newRetrievalStoreFixture(t)
	tag, err := fixture.store.CreateTag(t.Context(), "selected")
	require.NoError(t, err)
	assignedTarget, err := fixture.store.AssignTag(t.Context(), tag.ID, fixture.target.ID, fixture.target.Revision)
	require.NoError(t, err)
	fixture.target = assignedTarget.Node
	assignedSafe, err := fixture.store.AssignTag(t.Context(), tag.ID, fixture.safe.ID, fixture.safe.Revision)
	require.NoError(t, err)
	fixture.safe = assignedSafe.Node
	provider := fixture.provider(func() {
		_, unassignErr := fixture.store.UnassignTag(t.Context(), tag.ID, fixture.target.ID, fixture.target.Revision)
		require.NoError(t, unassignErr)
	})
	searcher := fixture.searcher(t, provider)

	report, err := searcher.Search(t.Context(), fixture.query(ModeHybrid, 1, store.SearchOptions{TagID: tag.ID}))

	require.NoError(t, err)
	assert.Empty(t, report.Results)
	assert.True(t, report.Truncated)
}

func TestSearcherRealStoreAutoReportsCoverageAfterProviderAddsRequiredDocument(t *testing.T) {
	fixture := newRetrievalStoreFixture(t)
	provider := fixture.provider(func() {
		contents := []byte("synthetic late document")
		_, err := fixture.store.CreateFile(t.Context(), fixture.store.RootID(), "match-late.txt",
			retrievalHashBytes(contents), int64(len(contents)), "text/plain")
		require.NoError(t, err)
	})
	searcher := fixture.searcher(t, provider)

	report, err := searcher.Search(t.Context(), fixture.query(ModeAuto, 10, store.SearchOptions{}))

	require.NoError(t, err)
	assert.Equal(t, ModeLexical, report.ActualMode)
	assert.Equal(t, DegradationIncompleteCoverage, report.Degradation)
	assert.Equal(t, Coverage{BindingRequired: true, ScopedDocuments: 3,
		CompleteDocuments: 2, State: CoverageIncomplete}, report.Coverage)
	assert.Equal(t, 1, provider.calls)
	for _, result := range report.Results {
		assert.NotEmpty(t, result.Path)
		assert.NotEmpty(t, result.Excerpt)
	}
}

func TestSearcherRealStoreSemanticLimitUsesDistinctDocumentsAndBestNeighbor(t *testing.T) {
	fixture := newRetrievalStoreFixture(t)
	provider := fixture.provider(nil)
	searcher := fixture.searcher(t, provider)

	report, err := searcher.Search(t.Context(), fixture.query(ModeSemantic, 1, store.SearchOptions{}))

	require.NoError(t, err)
	assert.True(t, report.Truncated)
	require.Len(t, report.Results, 1)
	assert.Equal(t, fixture.safe.ID, report.Results[0].Document.NodeID)
	assert.Equal(t, 1, report.Results[0].Rank)
	assert.Empty(t, report.Results[0].Excerpt)
}

func TestSearcherRealStoreSemanticLimitKeepsBestChunkBeforeDistinctDocumentTruncation(t *testing.T) {
	fixture, bestChunkInputID := newRetrievalChunkStoreFixture(t)
	provider := fixture.provider(nil)
	searcher := fixture.searcher(t, provider)

	report, err := searcher.Search(t.Context(), fixture.query(ModeSemantic, 1, store.SearchOptions{}))

	require.NoError(t, err)
	assert.True(t, report.Truncated)
	require.Len(t, report.Results, 1)
	assert.Equal(t, fixture.target.ID, report.Results[0].Document.NodeID)
	require.Len(t, report.Results[0].Evidence, 1)
	assert.Equal(t, bestChunkInputID, report.Results[0].Evidence[0].InputID)
}

type retrievalStoreFixture struct {
	store        *store.Store
	parent       store.Node
	target       store.Node
	safe         store.Node
	profile      store.ProcessingProfileRecord
	binding      document.EmbeddingBindingV1
	descriptor   document.EmbeddingDescriptor
	fingerprints document.FingerprintSet
	sets         []document.VectorSetV1
	nextHead     int64
}

func newRetrievalStoreFixture(t *testing.T) *retrievalStoreFixture {
	t.Helper()
	metadata, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	parent, err := metadata.Mkdir(t.Context(), metadata.RootID(), "docs")
	require.NoError(t, err)
	targetContents := []byte("synthetic target")
	target, err := metadata.CreateFile(t.Context(), parent.ID, "match-alpha.txt",
		retrievalHashBytes(targetContents), int64(len(targetContents)), "text/plain")
	require.NoError(t, err)
	safeContents := []byte("synthetic safe")
	safe, err := metadata.CreateFile(t.Context(), metadata.RootID(), "match-zulu.txt",
		retrievalHashBytes(safeContents), int64(len(safeContents)), "text/plain")
	require.NoError(t, err)
	profile, descriptor, binding, fingerprints := retrievalStoreProfile(
		t, document.EmbeddingRequired, document.EmbeddingInputOriginalFile)
	fixture := &retrievalStoreFixture{store: metadata, parent: parent, target: target, safe: safe,
		profile: profile, descriptor: descriptor, binding: binding, fingerprints: fingerprints, nextHead: 1}
	fixture.registerProfile(t)
	fixture.stageEmbedding(t, target, []float64{0, 1}, "target")
	fixture.stageEmbedding(t, safe, []float64{1, 0}, "safe")
	fixture.publishVectorGeneration(t)
	return fixture
}

func newRetrievalChunkStoreFixture(t *testing.T) (*retrievalStoreFixture, string) {
	t.Helper()
	metadata, err := store.Open(filepath.Join(t.TempDir(), "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	parent, err := metadata.Mkdir(t.Context(), metadata.RootID(), "docs")
	require.NoError(t, err)
	targetContents := []byte("synthetic chunk target")
	target, err := metadata.CreateFile(t.Context(), parent.ID, "match-alpha.txt",
		retrievalHashBytes(targetContents), int64(len(targetContents)), "text/plain")
	require.NoError(t, err)
	safeContents := []byte("synthetic chunk safe")
	safe, err := metadata.CreateFile(t.Context(), metadata.RootID(), "match-zulu.txt",
		retrievalHashBytes(safeContents), int64(len(safeContents)), "text/plain")
	require.NoError(t, err)
	profile, descriptor, binding, fingerprints := retrievalStoreProfile(
		t, document.EmbeddingRequired, document.EmbeddingInputRenditionChunk)
	fixture := &retrievalStoreFixture{store: metadata, parent: parent, target: target, safe: safe,
		profile: profile, descriptor: descriptor, binding: binding, fingerprints: fingerprints, nextHead: 1}
	targetEvidence := retrievalNormalizedEvidence(t, "abcdefghij")
	safeEvidence := retrievalNormalizedEvidence(t, "safe")
	targetAttachment := fixture.attachProfileWithEvidence(t, target, "target", targetEvidence.Checksum)
	safeAttachment := fixture.attachProfileWithEvidence(t, safe, "safe", safeEvidence.Checksum)
	targetGeneration := fixture.stageChunkEmbedding(t, target, targetAttachment,
		targetEvidence, [][]float64{{1, 0}, {0.1, 0}}, "target")
	fixture.stageChunkEmbedding(t, safe, safeAttachment, safeEvidence, [][]float64{{0.8, 0}}, "safe")
	fixture.publishVectorGeneration(t)
	require.Len(t, targetGeneration.Inputs, 2)
	return fixture, targetGeneration.Inputs[0].Key
}

func (fixture *retrievalStoreFixture) registerProfile(t *testing.T) {
	t.Helper()
	fixture.attachProfile(t, fixture.target, "profile")
}

func (fixture *retrievalStoreFixture) attachProfile(t *testing.T, node store.Node, suffix string) string {
	t.Helper()
	return fixture.attachProfileWithEvidence(t, node, suffix, retrievalHash("evidence"))
}

func (fixture *retrievalStoreFixture) attachProfileWithEvidence(t *testing.T, node store.Node,
	suffix, evidenceChecksum string,
) string {
	t.Helper()
	const capturedPolicy = `{"roles":[],"version":1}`
	build := store.RenditionBuildRecord{
		ID: retrievalHash("profile-build-" + suffix), VaultID: fixture.store.VaultID(),
		SourceSHA256:                      node.BlobHash,
		RenditionRequestFingerprint:       fixture.fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:        fixture.fingerprints.EvidenceLexical,
		CapturedArtifactPolicyFingerprint: retrievalHash(capturedPolicy),
		CapturedArtifactPolicy:            jsontext.Value(capturedPolicy),
		AuthorizationChecksum:             retrievalHash("rendition-authorization"),
		ProviderOperationID:               "synthetic-retrieval-profile",
		ProviderReceipt:                   jsontext.Value(`{"provider":"synthetic"}`),
		EvidenceChecksum:                  evidenceChecksum,
		RenditionChecksum:                 retrievalHash("rendition"),
		MarkdownChecksum:                  retrievalHash("markdown"),
		Completeness:                      document.EvidenceComplete,
		Warnings:                          []string{},
		CompletedAt:                       "2026-09-07T12:00:00.000000000Z",
	}
	require.NoError(t, fixture.store.StageRenditionBuild(t.Context(), build))
	lexical, err := fixture.store.StageLexicalGeneration(t.Context(), retrievalHash("lexical-generation-"+suffix))
	require.NoError(t, err)
	attachment := store.RenditionAttachmentRecord{
		ID: retrievalHash("profile-attachment-" + suffix), VaultID: fixture.store.VaultID(),
		ContentVersionID: node.CurrentVersionID, BuildID: build.ID,
		Profile: fixture.profile, AttachedAt: "2026-09-07T12:01:00.000000000Z",
	}
	require.NoError(t, fixture.store.PublishRenditionAndLexicalHeads(t.Context(), attachment,
		store.RenditionHeadRecord{ContentVersionID: node.CurrentVersionID,
			ProcessingProfileFingerprint: fixture.profile.Fingerprint, AttachmentID: attachment.ID,
			PublishedAt: "2026-09-07T12:02:00.000000000Z"}, lexical.ID))
	return attachment.ID
}

func (fixture *retrievalStoreFixture) stageEmbedding(t *testing.T, node store.Node,
	values []float64, suffix string,
) {
	t.Helper()
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: fixture.fingerprints.VectorSpace[fixture.binding.Name],
		Metric:                 fixture.binding.Metric, Normalization: fixture.binding.Normalization,
		Dimension: fixture.binding.Dimensions, InputKeys: []string{node.CurrentVersionID},
		InputChecksums: []string{node.BlobHash}, Values: [][]float64{values},
	})
	require.NoError(t, err)
	payload, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	payloadHash := retrievalHashBytes(payload)
	require.NoError(t, fixture.store.RecordRenditionBlob(t.Context(), payloadHash, int64(len(payload)),
		store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(payload)), PackEligible: true, Created: true}))
	record := store.EmbeddingSetRecord{
		ID: retrievalHash("embedding-set-" + suffix), VaultID: fixture.store.VaultID(),
		BindingID: fixture.binding.Name, InputKind: document.EmbeddingInputOriginalFile,
		ContentVersionID: node.CurrentVersionID, ProcessingProfileFingerprint: fixture.profile.Fingerprint,
		EmbeddingInputFingerprint: fixture.fingerprints.EmbeddingInput[fixture.binding.Name],
		VectorSpace: store.EmbeddingVectorSpaceRecord{
			ID:              fixture.fingerprints.VectorSpace[fixture.binding.Name],
			ContractVersion: store.EmbeddingVectorSpaceContractV1, Descriptor: fixture.descriptor,
		},
		InputGeneration: store.EmbeddingInputGenerationRecord{
			ID: retrievalHash("input-generation-" + suffix), SourceVersionID: node.CurrentVersionID,
			ProcessingProfileFingerprint: fixture.profile.Fingerprint,
			EvidenceFingerprint:          retrievalHash("input-evidence-" + suffix),
			TokenizerFingerprint:         retrievalHash("tokenizer-" + suffix),
			ChunkPolicyFingerprint:       retrievalHash("chunk-policy-" + suffix),
			FormatterFingerprint:         retrievalHash("formatter-" + suffix),
			GenerationChecksum:           node.BlobHash,
			Inputs:                       []store.EmbeddingInputReference{{ID: node.CurrentVersionID, RenderedChecksum: node.BlobHash}},
			CreatedAt:                    "2026-09-07T12:03:00.000000000Z",
		},
		VectorSet: store.EmbeddingVectorSetRecord{
			ID: setID, ContractVersion: store.EmbeddingVectorSetContractV1,
			VectorSpaceID:   fixture.fingerprints.VectorSpace[fixture.binding.Name],
			PayloadBlobHash: payloadHash, PayloadChecksum: setID, Payload: payload,
		},
		CreatedAt: "2026-09-07T12:04:00.000000000Z",
	}
	require.NoError(t, fixture.store.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, fixture.store.PublishEmbeddingHead(t.Context(), store.EmbeddingHeadRecord{
		FencingToken: fixture.nextHead,
		Key: store.EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID,
			BindingID: fixture.binding.Name, InputKind: document.EmbeddingInputOriginalFile},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: fixture.profile.Fingerprint,
		PublishedAt:                  "2026-09-07T12:05:00.000000000Z",
	}))
	fixture.nextHead++
	fixture.sets = append(fixture.sets, set)
}

func (fixture *retrievalStoreFixture) replaceEmbeddingHead(t *testing.T, node store.Node, values []float64) {
	t.Helper()
	priorSets := len(fixture.sets)
	fixture.stageEmbedding(t, node, values, "replacement")
	fixture.sets = fixture.sets[:priorSets]
}

func (fixture *retrievalStoreFixture) stageChunkEmbedding(t *testing.T, node store.Node,
	attachmentID string, evidence document.NormalizedEvidenceV1, values [][]float64, suffix string,
) document.EmbeddingInputGeneration {
	t.Helper()
	attachmentContext, err := document.NewAttachmentContextSnapshot("Synthetic title", "Synthetic context")
	require.NoError(t, err)
	inputPolicy, err := document.NewInputPolicy(fixture.binding, retrievalRuneTokenizer{},
		fixture.fingerprints.EvidenceLexical, &attachmentContext)
	require.NoError(t, err)
	generation, err := document.BuildEmbeddingInputs(evidence, inputPolicy, document.GenerationLimits{
		MaxInputs: 16, MaxTotalContentTokens: 1024, MaxTotalRenderedTokens: 2048,
		MaxTotalContentBytes: 1 << 20, MaxTotalRenderedBytes: 2 << 20,
		MaxFittingWorkTokens: 1 << 20, MaxFittingWorkBytes: 8 << 20,
	})
	require.NoError(t, err)
	require.Len(t, generation.Inputs, len(values))
	generationJSON, err := document.MarshalEmbeddingInputGeneration(generation)
	require.NoError(t, err)
	evidenceJSON, evidenceChecksum, err := document.MarshalNormalizedEvidenceV1(evidence)
	require.NoError(t, err)
	require.Equal(t, evidence.Checksum, evidenceChecksum)
	generationBlobHash := retrievalHashBytes(generationJSON)
	for _, artifact := range []struct {
		hash string
		data []byte
	}{{evidenceChecksum, evidenceJSON}, {generationBlobHash, generationJSON}} {
		require.NoError(t, fixture.store.RecordRenditionBlob(t.Context(), artifact.hash, int64(len(artifact.data)),
			store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(artifact.data)), PackEligible: true, Created: true}))
	}
	keys := make([]string, len(generation.Inputs))
	checksums := make([]string, len(generation.Inputs))
	for index, input := range generation.Inputs {
		keys[index], checksums[index] = input.Key, input.Checksum
	}
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: fixture.fingerprints.VectorSpace[fixture.binding.Name],
		Metric:                 fixture.binding.Metric, Normalization: fixture.binding.Normalization,
		Dimension: fixture.binding.Dimensions, InputKeys: keys, InputChecksums: checksums, Values: values,
	})
	require.NoError(t, err)
	payload, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	payloadHash := retrievalHashBytes(payload)
	require.NoError(t, fixture.store.RecordRenditionBlob(t.Context(), payloadHash, int64(len(payload)),
		store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(payload)), PackEligible: true, Created: true}))
	generationID := retrievalHash("embedding-generation-attachment/v1\x00" + generation.Checksum + "\x00" + attachmentID)
	record := store.EmbeddingSetRecord{
		ID: retrievalHash("chunk-embedding-set-" + suffix), VaultID: fixture.store.VaultID(),
		BindingID: fixture.binding.Name, InputKind: document.EmbeddingInputRenditionChunk,
		ContentVersionID: node.CurrentVersionID, ProcessingProfileFingerprint: fixture.profile.Fingerprint,
		EmbeddingInputFingerprint: fixture.fingerprints.EmbeddingInput[fixture.binding.Name],
		VectorSpace: store.EmbeddingVectorSpaceRecord{ID: fixture.fingerprints.VectorSpace[fixture.binding.Name],
			ContractVersion: store.EmbeddingVectorSpaceContractV1, Descriptor: fixture.descriptor},
		InputGeneration: store.EmbeddingInputGenerationRecord{ID: generationID,
			SourceVersionID: node.CurrentVersionID, ProcessingProfileFingerprint: fixture.profile.Fingerprint,
			GenerationJSON: generationJSON, EvidenceJSON: evidenceJSON,
			GenerationBlobHash: generationBlobHash, GenerationEncodedSize: int64(len(generationJSON)),
			GenerationChecksum: generation.Checksum, EvidenceFingerprint: evidenceChecksum,
			AttachmentID: attachmentID, CreatedAt: "2026-09-07T12:03:00.000000000Z"},
		VectorSet: store.EmbeddingVectorSetRecord{ID: setID, ContractVersion: store.EmbeddingVectorSetContractV1,
			VectorSpaceID:   fixture.fingerprints.VectorSpace[fixture.binding.Name],
			PayloadBlobHash: payloadHash, PayloadChecksum: setID, Payload: payload},
		CreatedAt: "2026-09-07T12:04:00.000000000Z",
	}
	require.NoError(t, fixture.store.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, fixture.store.PublishEmbeddingHead(t.Context(), store.EmbeddingHeadRecord{
		FencingToken: fixture.nextHead,
		Key: store.EmbeddingHeadKey{ContentVersionID: node.CurrentVersionID,
			BindingID: fixture.binding.Name, InputKind: document.EmbeddingInputRenditionChunk},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
		ProcessingProfileFingerprint: fixture.profile.Fingerprint,
		PublishedAt:                  "2026-09-07T12:05:00.000000000Z"}))
	fixture.nextHead++
	fixture.sets = append(fixture.sets, set)
	return generation
}

func (fixture *retrievalStoreFixture) publishVectorGeneration(t *testing.T) {
	t.Helper()
	setIDs := make([]string, len(fixture.sets))
	for index, set := range fixture.sets {
		_, setID, err := document.EncodeVectorSetV1(set)
		require.NoError(t, err)
		setIDs[index] = setID
	}
	manifest, err := vectorindex.NewManifest(setIDs)
	require.NoError(t, err)
	generation, err := vectorindex.BuildGeneration(manifest, fixture.sets, vectorindex.Options{})
	require.NoError(t, err)
	source, err := fixture.store.CaptureVectorIndexSource(t.Context(),
		fixture.fingerprints.VectorSpace[fixture.binding.Name])
	require.NoError(t, err)
	at := time.Date(2026, 9, 7, 12, 10, 0, 0, time.UTC)
	claim, claimed, err := fixture.store.ClaimVectorIndexBuild(t.Context(), source.VectorSpaceID,
		source.ManifestChecksum, "retrieval-integration", at, time.Hour)
	require.NoError(t, err)
	require.True(t, claimed)
	record := store.VectorIndexGenerationRecord{
		ID:            store.VectorIndexGenerationID(source.ManifestChecksum, generation.Bytes()),
		VectorSpaceID: source.VectorSpaceID, SourceManifestChecksum: source.ManifestChecksum,
		IndexManifestChecksum: generation.Metadata().Manifest.Checksum,
		Bytes:                 generation.Bytes(), RowCount: generation.Metadata().RowCount,
		BuiltAt: "2026-09-07T12:11:00.000000000Z",
	}
	require.NoError(t, fixture.store.StageVectorIndexGeneration(t.Context(), claim, record, at.Add(time.Minute)))
	require.NoError(t, fixture.store.PublishVectorIndexGeneration(t.Context(), claim, record.ID, at.Add(2*time.Minute)))
}

func (fixture *retrievalStoreFixture) searcher(t *testing.T, provider *retrievalMutatingProvider) *Searcher {
	t.Helper()
	clockCalls := 0
	searcher, err := NewSearcher(SearcherConfig{Backend: fixture.store,
		Encoders: retrievalMutatingResolver{provider: provider}, Owner: "retrieval-integration",
		LeaseDuration: time.Hour, Clock: func() time.Time {
			clockCalls++
			return time.Date(2026, 9, 7, 12, 20, clockCalls, 0, time.UTC)
		}})
	require.NoError(t, err)
	return searcher
}

func (fixture *retrievalStoreFixture) provider(mutate func()) *retrievalMutatingProvider {
	return &retrievalMutatingProvider{descriptor: fixture.descriptor, vector: []float32{1, 0}, mutate: mutate}
}

func (fixture *retrievalStoreFixture) query(mode Mode, limit int, scope store.SearchOptions) Query {
	return Query{Text: "match", Mode: mode, Limit: limit, Scope: scope,
		ProcessingProfileFingerprint: fixture.profile.Fingerprint, BindingID: fixture.binding.Name,
		Authorization: retrievalAuthorization(fixture.descriptor)}
}

type retrievalMutatingResolver struct{ provider document.EmbeddingProvider }

func (resolver retrievalMutatingResolver) ResolveQueryEncoder(context.Context,
	document.EmbeddingDescriptor,
) (document.EmbeddingProvider, error) {
	return resolver.provider, nil
}

type retrievalMutatingProvider struct {
	descriptor document.EmbeddingDescriptor
	vector     []float32
	mutate     func()
	calls      int
}

func (provider *retrievalMutatingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (provider *retrievalMutatingProvider) Embed(_ context.Context, inputs []document.EmbeddingInput,
	_ document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	provider.calls++
	if provider.mutate != nil {
		provider.mutate()
	}
	return document.EmbeddingResult{Vectors: []document.EmbeddingVector{{
		Key: inputs[0].Key, Values: append([]float32(nil), provider.vector...),
	}}}, nil
}

func retrievalStoreProfile(t *testing.T, activation document.EmbeddingActivation,
	inputKind document.EmbeddingInputKind,
) (
	store.ProcessingProfileRecord, document.EmbeddingDescriptor, document.EmbeddingBindingV1,
	document.FingerprintSet,
) {
	t.Helper()
	modelInput, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-retrieval-space",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-retrieval-embedder", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: retrievalHash("embedding-policy"), TrustBoundary: document.EmbeddingTrustLocalProcess,
		Model: "synthetic-v1", ModelRevision: "2026-09-07", Dimension: 2,
		Metric: document.VectorMetricDotProduct, Normalization: document.VectorNormalizationNone,
		ScalarEncoding: "float32", DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds:      []document.EmbeddingInputKind{inputKind},
		CompatibilityID: modelInput.CompatibilityID, SupportsTextQuery: true, ModelInput: modelInput,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	binding := document.EmbeddingBindingV1{
		Name: "vectors", Activation: activation,
		AuthorizationFingerprint: retrievalHash("embedding-authorization"),
		CompatibilityID:          descriptor.CompatibilityID, CredentialBinding: "credential:synthetic-retrieval",
		Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
		Dimensions: descriptor.Dimension, DisclosureFingerprint: retrievalHash("embedding-disclosure"),
		DocumentFormatter: descriptor.DocumentFormatter, InputKind: inputKind,
		MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: descriptor.Metric, Model: descriptor.Model, ModelInput: descriptor.ModelInput,
		Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary),
	}
	if inputKind == document.EmbeddingInputRenditionChunk {
		binding.MaxInputTokens = 128
		binding.Chunk = &document.EmbeddingChunkPolicyV1{ContextFingerprint: retrievalHash("chunk-context"),
			Formatter: "evidence-text/v1", MaxTokens: 5, OverlapTokens: 0,
			Tokenizer: "retrieval-runes", TokenizerRevision: "v1",
			TruncationPolicy: document.TruncationPolicyReject}
	}
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			Name: "primary", AdapterContract: "rendition-adapter/v1",
			AuthorizationFingerprint: retrievalHash("rendition-authorization"),
			CredentialBinding:        "credential:synthetic-retrieval",
			DeploymentFingerprint:    retrievalHash("rendition-deployment"),
			Descriptor: document.ProviderDescriptorV1{ID: "synthetic-rendition",
				Fingerprint: retrievalHash("rendition-provider")},
			DisclosureFingerprint: retrievalHash("rendition-disclosure"),
			MaxDocumentBytes:      1 << 20, MaxResponseBytes: 1 << 20, MaxUnits: 10,
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "synthetic-vault", UploadOptionsFingerprint: retrievalHash("upload-options"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     retrievalHash("completeness"),
			LexicalSegmenterFingerprint: retrievalHash("segmenter"), MaxDocumentChars: 10_000,
			MaxSegmentRunes: 100, MaxUnitRunes: 1_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      retrievalHash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: retrievalHash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: retrievalHash("attachment-policy"),
			ConsentFingerprint:          retrievalHash("consent"), RetainSanitizedMarkdown: true,
			RetainTypedArtifacts: true, TrustBoundary: "synthetic-vault",
		},
		Retrieval:  document.RetrievalPolicyV1{LexicalLimit: 10, VectorLimit: 10},
		Embeddings: []document.EmbeddingBindingV1{binding},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	record := store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
	return record, descriptor, binding, fingerprints
}

func retrievalHash(value string) string { return retrievalHashBytes([]byte(value)) }

func retrievalHashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func retrievalNormalizedEvidence(t *testing.T, text string) document.NormalizedEvidenceV1 {
	t.Helper()
	policy, err := document.NewEvidencePolicy(4096)
	require.NoError(t, err)
	evidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete,
		Family: "pdf", UnitKind: document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{Order: 0, Text: text,
			Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
	}, policy)
	require.NoError(t, err)
	return evidence
}

type retrievalRuneTokenizer struct{}

func (retrievalRuneTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "retrieval-runes", Revision: "v1"}
}

func (retrievalRuneTokenizer) PrefixTokenCountsMonotonic() bool { return true }

func (retrievalRuneTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
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

var _ Backend = (*store.Store)(nil)
var _ SemanticBackend = (*store.Store)(nil)
var _ CandidateRevalidationBackend = (*store.Store)(nil)
