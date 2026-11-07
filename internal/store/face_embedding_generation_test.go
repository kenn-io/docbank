package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestFaceEmbeddingIdentityLiteral(t *testing.T) {
	t.Parallel()
	face := PhotoFace{FaceID: "00000000-0000-4000-8000-000000000001", Box: document.FaceBox{X: .125, Y: .25, Width: .5, Height: .375}}
	checksum := FaceEmbeddingChecksum(fakeHash("a1"), face)
	require.Equal(t, "9a45296aeab1e7db9b7af51435d3a1b019bfcfae747c8ac0d29369038a56e5fd", checksum)
	generation := EmbeddingInputGenerationRecord{SourceVersionID: "00000000-0000-4000-8000-000000000002", ProcessingProfileFingerprint: fakeHash("b2"), EvidenceFingerprint: fakeHash("c3"), GenerationChecksum: fakeHash("a1"), ChunkPolicyFingerprint: FaceEmbeddingPolicyFingerprint(), Inputs: []EmbeddingInputReference{{ID: face.FaceID, RenderedChecksum: checksum}}}
	generation.ID = FaceEmbeddingGenerationID(generation, "face")
	require.Equal(t, "3903c30f51ca9eb9da1ad7ce9ce2b720329f0fca6981dc48ab4b08f965112629", generation.ID)
	require.NoError(t, validateFaceEmbeddingGeneration(generation, "face"))
	face.Box.X = .25
	require.NotEqual(t, checksum, FaceEmbeddingChecksum(fakeHash("a1"), face))
	require.Error(t, validateFaceEmbeddingGeneration(generation, "other-model"))
	generation.Inputs = append([]EmbeddingInputReference{{ID: "00000000-0000-4000-8000-000000000003", RenderedChecksum: checksum}}, generation.Inputs...)
	generation.ID = FaceEmbeddingGenerationID(generation, "face")
	require.Error(t, validateFaceEmbeddingGeneration(generation, "face"))
}

func TestFaceVectorSubsetMembership(t *testing.T) {
	t.Parallel()
	first := "00000000-0000-4000-8000-000000000001"
	second := "00000000-0000-4000-8000-000000000002"
	generation := EmbeddingInputGenerationRecord{ChunkPolicyFingerprint: FaceEmbeddingPolicyFingerprint(), Inputs: []EmbeddingInputReference{{ID: first, RenderedChecksum: fakeHash("a1")}, {ID: second, RenderedChecksum: fakeHash("a1")}}}
	space := EmbeddingVectorSpaceRecord{ID: fakeHash("b2"), Dimensions: 512}
	set := EmbeddingVectorSetRecord{ContractVersion: EmbeddingVectorSetContractV1, ID: fakeHash("c3"), PayloadChecksum: fakeHash("c3"), PayloadBlobHash: fakeHash("d4"), PayloadSize: 2048, VectorSpaceID: space.ID, Dimensions: 512, RowCount: 1, ManifestChecksum: "b7d17261f5a101b8d5cdcc80690adb98381aec32dd486df2b90adff7c28b9612", rows: []EmbeddingVectorRowRecord{{RowID: second, InputID: second, Dimensions: 512, Checksum: fakeHash("a1")}}}
	require.NoError(t, validateEmbeddingVectorSet(set, space, generation))
	generation.Inputs = generation.Inputs[:1]
	require.EqualError(t, validateEmbeddingVectorSet(set, space, generation), "embedding vector set contains an unexpected input row")
}
