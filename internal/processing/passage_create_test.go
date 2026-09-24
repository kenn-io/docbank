package processing

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestEmbeddedServiceCreatesHistoricalPassageWithoutProvider(t *testing.T) {
	fixture := newPublicationFixture(t)
	staged := fixture.stage(t, publicationIDs{"passage-build", "passage-attachment", "passage-generation"},
		"synthetic Markdown 😀 e\u0301", "Synthetic heading")
	body := bytes.Clone(staged.Rendition.Markdown)
	var err error
	staged.Rendition, _, err = document.EnvelopeRenditionV1(staged.Rendition, document.RenditionEnvelopeV1{
		BuildID: staged.Build.ID, SourceSHA256: staged.Build.SourceSHA256,
		SourceFormat: "pdf", SourceMediaType: "application/pdf",
		RenditionRequestFingerprint: staged.Build.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:  staged.Build.EvidenceLexicalFingerprint,
		NormalizedEvidenceContract:  document.NormalizedEvidenceContractV1,
		UnitKind:                    document.EvidenceUnitPage,
	})
	require.NoError(t, err)
	staged.Build.MarkdownChecksum = staged.Rendition.MarkdownChecksum
	staged.Build.RenditionChecksum = staged.Rendition.Checksum
	for index := range staged.Build.Artifacts {
		if staged.Build.Artifacts[index].Role != "sanitized_markdown" {
			continue
		}
		staged.Build.Artifacts[index].BlobHash = staged.Rendition.MarkdownChecksum
		staged.Build.Artifacts[index].Checksum = staged.Rendition.MarkdownChecksum
		staged.Build.Artifacts[index].Size = int64(len(staged.Rendition.Markdown))
		staged.Artifacts[index].Payload = bytes.NewReader(staged.Rendition.Markdown)
	}
	for _, retained := range staged.Artifacts {
		receipt, writeErr := fixture.blobs.WriteDetailedContext(t.Context(), retained.Payload)
		require.NoError(t, writeErr)
		require.NoError(t, fixture.catalog.RecordRenditionBlob(t.Context(), receipt.Hash,
			receipt.Size, processingBlobPhysical(t, receipt)))
	}
	require.NoError(t, fixture.catalog.StageRenditionBuild(t.Context(), staged.Build))
	generation, err := fixture.catalog.StageLexicalGeneration(t.Context(), staged.LexicalGenerationID)
	require.NoError(t, err)
	require.NoError(t, fixture.catalog.PublishRenditionAndLexicalHeads(t.Context(),
		staged.Attachment, staged.Head, generation.ID))
	node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
	require.NoError(t, err)
	start := bytes.Index(body, []byte("synthetic Markdown"))
	require.GreaterOrEqual(t, start, 0)
	end := start + bytes.IndexByte(body[start:], '\n')
	require.Greater(t, end, start)
	request := PassageCreateRequest{NodeID: node.ID, ContentVersionID: fixture.versionID,
		RenditionBuildID: staged.Build.ID, AttachmentID: staged.Attachment.ID,
		ByteStart: start, ByteEnd: end}
	created, err := service.CreatePassage(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, string(body[start:end]), created.Text)
	require.Equal(t, fixture.catalog.VaultID(), created.Ref.VaultUID)
	require.Equal(t, fixture.versionID, created.Ref.ContentVersionID)
	require.Equal(t, staged.Build.ID, created.Ref.RenditionBuildID)
	require.Equal(t, staged.Attachment.ID, created.Ref.AttachmentID)
	resolved, err := service.ResolvePassage(t.Context(), PassageResolveRequest{Ref: created.Ref})
	require.NoError(t, err)
	require.Equal(t, created.Text, resolved.Text)

	moved, _, err := fixture.catalog.MoveToPath(t.Context(), node.ID, node.Revision, "/renamed.pdf")
	require.NoError(t, err)
	_, _, err = fixture.catalog.ReplaceContent(t.Context(), moved.ID, moved.Revision,
		passageProcessingHash("replacement"), 11, "application/pdf")
	require.NoError(t, err)
	historical, err := service.CreatePassage(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, created.Ref, historical.Ref)
}

func TestCreatePassageSealsExactRetainedMarkdown(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.authority.Node.ID = 27
	fixture.catalog.authority.Version = store.ContentVersion{ID: fixture.ref.ContentVersionID,
		NodeID: 27, BlobHash: fixture.ref.SourceSHA256}
	fixture.catalog.authority.Attachment.ID = fixture.ref.AttachmentID
	fixture.catalog.authority.Build.ID = fixture.ref.RenditionBuildID
	catalog := &passageCreateCatalogStub{passageCatalogStub: fixture.catalog}
	request := PassageCreateRequest{NodeID: 27, ContentVersionID: fixture.ref.ContentVersionID,
		RenditionBuildID: fixture.ref.RenditionBuildID, AttachmentID: fixture.ref.AttachmentID,
		ByteStart: fixture.ref.ByteStart, ByteEnd: fixture.ref.ByteEnd}
	created, err := createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.NoError(t, err)
	require.Equal(t, fixture.ref, created.Ref)
	require.Equal(t, "😀 evidence", created.Text)
	require.NotEmpty(t, created.PassageID)
	require.Equal(t, 1, catalog.allocations)
	createdAgain, err := createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.NoError(t, err)
	require.Equal(t, created.Ref, createdAgain.Ref)

	for _, invalid := range []PassageCreateRequest{
		{NodeID: 27, ContentVersionID: request.ContentVersionID,
			RenditionBuildID: request.RenditionBuildID, AttachmentID: request.AttachmentID,
			ByteStart: request.ByteStart + 1, ByteEnd: request.ByteEnd},
		{NodeID: 27, ContentVersionID: request.ContentVersionID,
			RenditionBuildID: request.RenditionBuildID, AttachmentID: request.AttachmentID,
			ByteStart: 0, ByteEnd: strings.Index("# Synthetic heading\r\n\r\n😀 evidence and e\u0301\r\n", "\u0301") + 1},
		{NodeID: 27, ContentVersionID: request.ContentVersionID,
			RenditionBuildID: request.RenditionBuildID, AttachmentID: request.AttachmentID,
			ByteStart: 0, ByteEnd: MaxPassageReadBytes + 1},
	} {
		_, err := createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", invalid)
		require.Error(t, err)
	}
	require.Equal(t, 2, catalog.allocations, "invalid ranges must not allocate an identity")
}

func TestCreatePassageReadsLargeRetainedBodyWithBoundedQuote(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.authority.Node.ID = 27
	fixture.catalog.authority.Version = store.ContentVersion{ID: fixture.ref.ContentVersionID,
		NodeID: 27, BlobHash: fixture.ref.SourceSHA256}
	fixture.catalog.authority.Attachment.ID = fixture.ref.AttachmentID
	body := append(bytes.Repeat([]byte("a"), 2<<20), []byte("😀")...)
	rendered, _, err := document.EnvelopeRenditionV1(document.RenditionV1{
		ContractVersion: document.RenditionContractV1, Completeness: document.EvidenceComplete,
		EvidenceChecksum: passageProcessingHash("large evidence"), Markdown: body,
		MarkdownChecksum: passageProcessingHashBytes(body),
		Units: []document.NormalizedUnitV1{{EvidenceUnitID: "page:000000", Order: 0,
			Text: "a", Locator: document.EvidenceLocatorV1{Kind: document.EvidenceLocatorPage,
				IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: 1}}},
	}, document.RenditionEnvelopeV1{BuildID: fixture.ref.RenditionBuildID,
		SourceSHA256: fixture.ref.SourceSHA256, SourceFormat: "pdf",
		SourceMediaType: "application/pdf", RenditionRequestFingerprint: passageProcessingHash("request"),
		EvidenceLexicalFingerprint: passageProcessingHash("lexical"),
		NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
		UnitKind:                   document.EvidenceUnitPage})
	require.NoError(t, err)
	fixture.blobs.payload = rendered.Markdown
	fixture.catalog.authority.Artifact.BlobHash = passageProcessingHashBytes(rendered.Markdown)
	fixture.catalog.authority.Artifact.Checksum = fixture.catalog.authority.Artifact.BlobHash
	fixture.catalog.authority.Artifact.Size = int64(len(rendered.Markdown))
	catalog := &passageCreateCatalogStub{passageCatalogStub: fixture.catalog}
	created, err := createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", PassageCreateRequest{
		NodeID: 27, ContentVersionID: fixture.ref.ContentVersionID,
		RenditionBuildID: fixture.ref.RenditionBuildID, AttachmentID: fixture.ref.AttachmentID,
		ByteStart: 2 << 20, ByteEnd: len(body),
	})
	require.NoError(t, err)
	require.Equal(t, "😀", created.Text)
	require.Equal(t, passageProcessingHashBytes(body), created.Ref.BodySHA256)
	require.Equal(t, passageProcessingHash("😀"), created.Ref.QuoteSHA256)
}

func TestCreatePassageRejectsRevokedAndChangedAuthorityBeforeAllocation(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.authority.Node.ID = 27
	fixture.catalog.authority.Version = store.ContentVersion{ID: fixture.ref.ContentVersionID,
		NodeID: 27, BlobHash: fixture.ref.SourceSHA256}
	fixture.catalog.authority.Attachment.ID = fixture.ref.AttachmentID
	catalog := &passageCreateCatalogStub{passageCatalogStub: fixture.catalog}
	request := PassageCreateRequest{NodeID: 27, ContentVersionID: fixture.ref.ContentVersionID,
		RenditionBuildID: fixture.ref.RenditionBuildID, AttachmentID: fixture.ref.AttachmentID,
		ByteStart: fixture.ref.ByteStart, ByteEnd: fixture.ref.ByteEnd}
	fixture.catalog.inputBinding = "synthetic-input"
	_, err := createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.ErrorIs(t, err, ErrPassageUnavailable)
	require.Zero(t, fixture.blobs.calls)
	require.Zero(t, catalog.allocations)
	fixture.catalog.inputVisible = true
	fixture.catalog.authority.Build.ID = passageProcessingHash("changed build")
	_, err = createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.ErrorIs(t, err, ErrPassageUnavailable)
	require.Zero(t, catalog.allocations)
	fixture.catalog.authority.Build.ID = request.RenditionBuildID
	fixture.catalog.authority.Artifact.Size = MaxRenditionBytes + 256<<10 + 1
	_, err = createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.ErrorIs(t, err, ErrPassageUnavailable)
	require.Zero(t, fixture.blobs.calls)
	require.Zero(t, catalog.allocations)
	fixture.catalog.authority.Artifact.Size = int64(len(fixture.blobs.payload))
	fixture.blobs.payload = bytes.Clone(fixture.blobs.payload)
	fixture.blobs.payload[len(fixture.blobs.payload)-1] ^= 1
	_, err = createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.ErrorIs(t, err, ErrPassageCorrupt)
	require.Zero(t, catalog.allocations)
	fixture.blobs.payload[len(fixture.blobs.payload)-1] = 0xff
	fixture.catalog.authority.Artifact.BlobHash = passageProcessingHashBytes(fixture.blobs.payload)
	fixture.catalog.authority.Artifact.Checksum = fixture.catalog.authority.Artifact.BlobHash
	_, err = createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.ErrorIs(t, err, ErrPassageCorrupt)
	require.Zero(t, catalog.allocations)
	fixture.catalog.err = store.ErrPassageAuthorityUnavailable
	beforeReads := fixture.blobs.calls
	_, err = createPassage(t.Context(), catalog, fixture.blobs, "embedded:operator", request)
	require.ErrorIs(t, err, ErrPassageUnavailable)
	require.Equal(t, beforeReads, fixture.blobs.calls, "pruned authority must fail before content access")
	require.Zero(t, catalog.allocations)
}

type passageCreateCatalogStub struct {
	*passageCatalogStub

	allocations int
}

func (stub *passageCreateCatalogStub) PassageCreationAuthority(_ context.Context, nodeID int64,
	versionID, buildID, attachmentID string,
) (store.PassageAuthority, error) {
	stub.calls++
	if stub.err != nil {
		return store.PassageAuthority{}, stub.err
	}
	a := stub.authority
	if a.Node.ID != nodeID || a.Version.ID != versionID || a.Build.ID != buildID ||
		a.Attachment.ID != attachmentID {
		return store.PassageAuthority{}, store.ErrPassageAuthorityUnavailable
	}
	return a, nil
}

func (stub *passageCreateCatalogStub) EnsurePassageDocumentIdentity(context.Context, store.PassageIdentityClaim) (store.DocumentIdentity, error) {
	stub.allocations++
	return store.DocumentIdentity{DocumentUID: "22222222-2222-4222-8222-222222222222", NodeID: 27}, nil
}
