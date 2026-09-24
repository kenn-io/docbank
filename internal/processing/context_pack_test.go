package processing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestAssembleContextPackMergesOverlapAndChargesEnvelope(t *testing.T) {
	body := []byte("alpha βeta gamma")
	identity := document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     contextTestHash("source"), RenditionBuildID: contextTestHash("build"),
		AttachmentID: contextTestHash("attachment"),
	}
	first, err := document.NewPassageRefV1(identity, body, 0, 10)
	require.NoError(t, err)
	second, err := document.NewPassageRefV1(identity, body, 6, len(body))
	require.NoError(t, err)
	candidates := []contextCandidate{
		{Ref: first, Body: body, Path: "/synthetic.md", Reasons: []string{"lexical_segment"}},
		{Ref: second, Body: body, Path: "/synthetic.md", Reasons: []string{"lexical_segment"}},
	}
	pack, err := assembleContextPack("sha256:"+contextTestHash("fence"), 1, candidates, 4096, 2, 20)
	require.NoError(t, err)
	require.Len(t, pack.Passages, 1)
	require.Equal(t, string(body), pack.Passages[0].Text)
	require.Equal(t, 1, pack.Coverage.SelectedSources)
	encoded, err := json.Marshal(pack)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 4096)

	_, err = assembleContextPack("sha256:"+contextTestHash("fence"), 1, candidates, 1, 2, 20)
	require.ErrorIs(t, err, ErrContextBudget)
}

func TestAssembleContextPackMergesTransitiveWindows(t *testing.T) {
	body := []byte("abcdefghij")
	identity := document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     contextTestHash("source"), RenditionBuildID: contextTestHash("build"),
		AttachmentID: contextTestHash("attachment"),
	}
	var candidates []contextCandidate
	for _, span := range [][2]int{{0, 3}, {7, 10}, {2, 8}} {
		ref, err := document.NewPassageRefV1(identity, body, span[0], span[1])
		require.NoError(t, err)
		candidates = append(candidates, contextCandidate{Ref: ref, Body: body, Path: "/synthetic.md"})
	}
	pack, err := assembleContextPack("sha256:"+contextTestHash("fence"), 1, candidates, 4096, 3, 20)
	require.NoError(t, err)
	require.Len(t, pack.Passages, 1)
	require.Equal(t, string(body), pack.Passages[0].Text)
	require.Equal(t, 2, pack.Deduplicated)
}

func TestContextPackBudgetIncludesPathsAndOmissionMetadata(t *testing.T) {
	body := []byte("short")
	identity := document.PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     contextTestHash("source"), RenditionBuildID: contextTestHash("build"),
		AttachmentID: contextTestHash("attachment"),
	}
	ref, err := document.NewPassageRefV1(identity, body, 0, len(body))
	require.NoError(t, err)
	pack, err := assembleContextPack("sha256:"+contextTestHash("fence"), 1,
		[]contextCandidate{{Ref: ref, Body: body, Path: "/" + strings.Repeat("p", 1000)}},
		512, 2, 20)
	require.NoError(t, err)
	require.Empty(t, pack.Passages)
	require.Equal(t, 1, pack.Omitted["byte_budget"])
	require.False(t, pack.Complete)
	encoded, err := json.Marshal(pack)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 512)
}

func TestSeedContextCandidatesUseVerifiedCurrentRetainedBytes(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.authority.Fresh = true
	candidates, truncated, err := seedContextCandidates(t.Context(), fixture.catalog, fixture.blobs,
		"embedded:operator", fixture.ref, true)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Len(t, candidates, 1)
	require.Equal(t, "/renamed/source.pdf", candidates[0].Path)
	require.Contains(t, string(candidates[0].Body[candidates[0].Ref.ByteStart:candidates[0].Ref.ByteEnd]), "😀 evidence")
	require.NoError(t, document.ValidatePassageRefV1(candidates[0].Ref, candidates[0].Body))
	require.Equal(t, 1, fixture.blobs.calls)

	fixture = newPassageResolutionFixture(t)
	_, _, err = seedContextCandidates(t.Context(), fixture.catalog, fixture.blobs,
		"embedded:operator", fixture.ref, false)
	require.ErrorIs(t, err, ErrPassageUnavailable)
}

func TestContextPackSearchReturnsVerifiedExactSegmentWithoutProvider(t *testing.T) {
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t,
		publicationIDs{"context-build", "context-attachment", "context-generation"},
		"# Heading\n\nalpha βeta gamma", "Heading")
	staged.Rendition, _, err = document.EnvelopeRenditionV1(staged.Rendition,
		document.RenditionEnvelopeV1{BuildID: staged.Build.ID,
			SourceSHA256: staged.Build.SourceSHA256, SourceFormat: "pdf",
			SourceMediaType: "application/pdf", UnitKind: document.EvidenceUnitPage,
			RenditionRequestFingerprint: staged.Build.RenditionRequestFingerprint,
			EvidenceLexicalFingerprint:  staged.Build.EvidenceLexicalFingerprint,
			NormalizedEvidenceContract:  document.NormalizedEvidenceContractV1})
	require.NoError(t, err)
	staged.Build.MarkdownChecksum = staged.Rendition.MarkdownChecksum
	staged.Build.RenditionChecksum = staged.Rendition.Checksum
	for index := range staged.Build.Artifacts {
		if staged.Build.Artifacts[index].Role == sanitizedMarkdownRole {
			staged.Build.Artifacts[index].BlobHash = staged.Rendition.MarkdownChecksum
			staged.Build.Artifacts[index].Checksum = staged.Rendition.MarkdownChecksum
			staged.Build.Artifacts[index].Size = int64(len(staged.Rendition.Markdown))
		}
	}
	staged.Artifacts[1].Payload = bytes.NewReader(staged.Rendition.Markdown)
	for index, artifact := range staged.Artifacts {
		receipt, writeErr := fixture.blobs.WriteDetailedContext(t.Context(), artifact.Payload)
		require.NoError(t, writeErr)
		require.Equal(t, staged.Build.Artifacts[index].BlobHash, receipt.Hash)
		require.NoError(t, publisher.recordRenditionReceipt(t.Context(),
			staged.Build.Artifacts[index].Role, receipt))
	}
	require.NoError(t, fixture.catalog.StageRenditionBuild(t.Context(), staged.Build))
	_, err = fixture.catalog.StageLexicalGeneration(t.Context(), staged.LexicalGenerationID)
	require.NoError(t, err)
	require.NoError(t, fixture.catalog.PublishRenditionAndLexicalHeads(t.Context(),
		staged.Attachment, staged.Head, staged.LexicalGenerationID))
	node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
	require.NoError(t, err)
	var portable document.ProcessingProfileV1
	require.NoError(t, jsonv2.Unmarshal(fixture.profile.CanonicalProfile, &portable))
	service := &Service{catalog: fixture.catalog, blobs: fixture.blobs,
		profiles: map[string]configuredProfile{"private": {
			record: fixture.profile, portable: portable,
		}}, principal: "embedded:operator", scope: "document-processing"}
	withoutIdentity, err := service.ContextPack(t.Context(), ContextPackRequest{
		Fence: SourceFence{VaultUID: fixture.catalog.VaultID(),
			ContentVersionIDs: []string{fixture.versionID}},
		Query: "alpha", Profile: "private", MaxBytes: 4096,
	})
	require.NoError(t, err)
	require.Empty(t, withoutIdentity.Passages)
	require.Equal(t, 1, withoutIdentity.Omitted["missing_identity"])
	require.False(t, withoutIdentity.Complete)
	_, err = fixture.catalog.DocumentIdentityByNode(t.Context(), node.ID)
	require.ErrorIs(t, err, store.ErrDocumentIdentityUnavailable)
	_, err = fixture.catalog.EnsureDocumentIdentity(t.Context(), node.ID)
	require.NoError(t, err)
	pack, err := service.ContextPack(t.Context(), ContextPackRequest{
		Fence: SourceFence{VaultUID: fixture.catalog.VaultID(),
			ContentVersionIDs: []string{fixture.versionID}},
		Query: "alpha", Profile: "private", MaxBytes: 4096,
	})
	require.NoError(t, err)
	require.Len(t, pack.Passages, 1)
	require.Equal(t, 1, pack.Coverage.RequestedSources)
	require.Equal(t, 1, pack.Coverage.AvailableSources)
	require.Contains(t, pack.Passages[0].Text, "alpha βeta")
	quote := sha256.Sum256([]byte(pack.Passages[0].Text))
	require.Equal(t, hex.EncodeToString(quote[:]), pack.Passages[0].Ref.QuoteSHA256)
	encoded, err := json.Marshal(pack)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 4096)

	missingSource := []byte("synthetic not yet rendered")
	receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(missingSource))
	require.NoError(t, err)
	missingNode, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(),
		"missing.pdf", receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	withMissing, err := service.ContextPack(t.Context(), ContextPackRequest{
		Fence: SourceFence{VaultUID: fixture.catalog.VaultID(),
			ContentVersionIDs: []string{fixture.versionID, missingNode.CurrentVersionID}},
		Query: "alpha", Profile: "private", MaxBytes: 4096,
	})
	require.NoError(t, err)
	require.Equal(t, 2, withMissing.Coverage.RequestedSources)
	require.Equal(t, 1, withMissing.Coverage.RenditionMissingSources)
	require.Equal(t, 1, withMissing.Omitted["scope_rendition_missing"])
	require.False(t, withMissing.Complete)
	require.True(t, withMissing.Truncated)
}

func TestContextPackSourceFenceHardBoundary(t *testing.T) {
	ids := make([]string, 4097)
	for index := range ids {
		ids[index] = fmt.Sprintf("00000000-0000-4000-8000-%012x", index)
	}
	_, err := normalizeFenceIDs(ids[:4096])
	require.NoError(t, err)
	_, err = normalizeFenceIDs(ids)
	require.Error(t, err)
}

func TestBoundedContextWindowKeepsLargeSectionAndUnicodeWithinCap(t *testing.T) {
	body := []byte(strings.Repeat("🙂", 512<<10))
	seedStart := 4 * 200_000
	start, end := boundedContextWindow(body, 0, len(body), seedStart, seedStart+40, 32<<10)
	require.LessOrEqual(t, end-start, 32<<10)
	require.LessOrEqual(t, start, seedStart)
	require.GreaterOrEqual(t, end, seedStart+40)
	require.True(t, isContextRuneBoundary(body, start))
	require.True(t, isContextRuneBoundary(body, end))

	start, end = boundedContextWindow(body, 0, len(body), seedStart, seedStart+64<<10, 32<<10)
	require.LessOrEqual(t, end-start, 32<<10)
	require.True(t, isContextRuneBoundary(body, end))
}

func TestContextPackFinalVisibilityCheckRejectsRevokedInput(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.authority.Fresh = true
	fixture.catalog.inputBinding = contextTestHash("synthetic-input")
	pack := ContextPack{Passages: []ContextPassage{{Ref: fixture.ref, Text: "😀 evidence"}}}
	err := revalidateContextPack(t.Context(), fixture.catalog, "embedded:operator", pack)
	require.ErrorIs(t, err, ErrPassageUnavailable)
	require.Zero(t, fixture.blobs.calls)
	fixture.catalog.inputVisible = true
	require.NoError(t, revalidateContextPack(t.Context(), fixture.catalog, "embedded:operator", pack))
}

func contextTestHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
