package docbank_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/plaintext"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"uuid"
)

func TestSelectedMediaPublicSuppliedEmbeddingComposition(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
		require.NoError(t, err)
		embedder := newSyntheticEmbeddingProvider(t)
		profile := embeddedProcessingProfile(t, renderer.Descriptor())
		profile.Embeddings = []document.EmbeddingBindingV1{syntheticChunkEmbeddingBinding(embedder.descriptor)}
		cfg := docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{SuppliedMediaEmbeddingProfile: "donor", Profiles: map[string]docbank.ProcessingProfileConfig{"donor": {Profile: profile, RenditionProvider: renderer, EmbeddingProviders: map[string]document.EmbeddingProvider{"chunks": embedder}, Tokenizers: map[string]document.Tokenizer{"chunks": syntheticRuneTokenizer{}}}}}}
		vault, err := docbank.New(t.Context(), cfg)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, vault.Close()) })
		raw := mediatest.WAV()
		hash := sha256.Sum256(raw)
		digest := hex.EncodeToString(hash[:])
		receipt, err := vault.SubmitSuppliedMedia(t.Context(), docbank.SuppliedMediaRequest{OperationID: uuid.New().String(), Filename: "fixture.wav", MediaType: "audio/wav", SHA256: digest, ByteLength: int64(len(raw)), Content: bytes.NewReader(raw), Occurrence: docbank.MediaOccurrenceInput{Ref: "synthetic", Revision: "1"}})
		require.NoError(t, err)
		text := "The telescope delivery arrives Friday at three."
		textHash := sha256.Sum256([]byte(text))
		artifact, err := vault.ImportRecordingArtifact(t.Context(), docbank.MediaArtifactRequest{OperationID: uuid.New().String(), SourceID: receipt.SourceID, OccurrenceID: receipt.OccurrenceID, Kind: "transcript", Filename: "fixture.txt", MediaType: "text/plain", SHA256: hex.EncodeToString(textHash[:]), ByteLength: int64(len(text)), Content: strings.NewReader(text)})
		require.NoError(t, err)
		node, err := vault.Stat(t.Context(), "/media/"+digest[:2]+"/"+digest+".wav")
		require.NoError(t, err)
		request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: node.ID, ContentVersionID: receipt.ContentVersionID, Profile: "supplied-transcript"}}
		plan, err := vault.PlanProcessing(t.Context(), request)
		require.NoError(t, err)
		_, err = vault.GrantProcessingPlanConsent(t.Context(), docbank.ProcessingConsentGrantRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint})
		require.NoError(t, err)
		_, err = vault.RetryMedia(t.Context(), uuid.New().String(), receipt.SourceID, docbank.MediaProcessingRequest{Profile: "supplied-transcript", SuppliedInputID: artifact.SuppliedInputID})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			status, e := vault.MediaStatus(t.Context(), receipt.SourceID)
			return e == nil && status.OperationState == "succeeded"
		}, time.Minute, time.Millisecond)
		report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "telescope", Mode: docbank.DocumentSearchSemantic, Profile: "supplied-transcript", Fence: docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.ContentVersionID}}, MediaSources: []docbank.DocumentMediaSourceSelector{{SourceID: receipt.SourceID, SourceVersionID: receipt.SourceVersionID, ContentVersionID: receipt.ContentVersionID, SuppliedInputIDs: []string{artifact.SuppliedInputID}}}})
		require.NoError(t, err)
		require.Len(t, report.Results, 1)
		require.Equal(t, "supplied", report.MediaSelections[0].Origin)
		require.Equal(t, artifact.SuppliedInputID, report.MediaSelections[0].SuppliedInputID)
		require.NoError(t, vault.Close())
		donor := cfg.Processing.Profiles["donor"]
		binding := donor.Profile.Embeddings[0]
		chunk := *binding.Chunk
		chunk.OverlapTokens = 4
		binding.Chunk = &chunk
		donor.Profile.Embeddings = []document.EmbeddingBindingV1{binding}
		cfg.Processing.Profiles["donor"] = donor
		vault, err = docbank.New(t.Context(), cfg)
		require.NoError(t, err)
		changed, err := vault.PlanProcessing(t.Context(), request)
		require.NoError(t, err)
		require.NotEqual(t, plan.Fingerprint, changed.Fingerprint)
		_, err = vault.RetryMedia(t.Context(), uuid.New().String(), receipt.SourceID, docbank.MediaProcessingRequest{Profile: "supplied-transcript", SuppliedInputID: artifact.SuppliedInputID})
		require.Error(t, err, "a changed donor must not inherit consent across restart")
	})
}
