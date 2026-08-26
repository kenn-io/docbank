package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

var errInjectedPublication = errors.New("injected publication failure")

func TestPublishRenditionPublishesVerifiedArtifactsAndHeads(t *testing.T) {
	// Mutation caught: omitting physical membership or either head publication
	// would leave the successful result unreadable through its public stores.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)

	published, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "searchable mercury evidence", "first markdown",
	))
	require.NoError(t, err)
	assert.Equal(t, processingHash("b1"), published.BuildID)
	assert.Equal(t, processingHash("51"), published.AttachmentID)
	assert.Equal(t, processingHash("91"), published.LexicalGeneration.ID)
	require.Len(t, published.Artifacts, 2)
	for _, artifact := range published.Artifacts {
		physical, err := fixture.catalog.PhysicalContent(t.Context(), artifact.Hash)
		require.NoError(t, err)
		assert.Equal(t, artifact.Size, physical.LogicalBytes)
	}

	view, err := fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.NoError(t, err)
	assert.Equal(t, published.BuildID, view.Build.ID)
	active, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, published.LexicalGeneration, active)
	hits, _, err := fixture.catalog.SearchPage(t.Context(), "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, fixture.versionID, hits[0].Node.CurrentVersionID)
	assert.Equal(t, store.SearchMatchContent, hits[0].Match)
}

func TestPublishRenditionAcceptsNilBuildWarnings(t *testing.T) {
	// Mutation caught: exact slice comparison rejects a warning-free build
	// when one representation uses nil and the other uses an empty slice.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "searchable mercury evidence", "first markdown",
	)
	require.Empty(t, staged.Rendition.Warnings)
	staged.Build.Warnings = nil

	_, err = publisher.PublishRendition(t.Context(), staged)
	require.NoError(t, err)
}

func TestPublishRenditionRejectsArtifactReceiptMismatchBeforeCatalogAuthority(t *testing.T) {
	// Mutation caught: trusting declared size instead of the verified CAS
	// receipt would grant catalog authority to bytes from a rejected candidate.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	staged := fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "searchable mercury evidence", "first markdown",
	)
	actualHash := staged.Build.Artifacts[0].BlobHash
	staged.Build.Artifacts[0].Size++

	_, err = publisher.PublishRendition(t.Context(), staged)
	require.Error(t, err)
	_, err = fixture.catalog.PhysicalContent(t.Context(), actualHash)
	require.ErrorIs(t, err, store.ErrNotFound,
		"a rejected receipt must not gain catalog-authorized membership")
	_, err = fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestPublishRenditionRejectsForgedProducerGraph(t *testing.T) {
	// Mutation caught: trusting caller-supplied rendition, unit, and segment
	// checksums allows unrelated evidence, Markdown, and searchable text to be
	// staged as one immutable build.
	for _, testCase := range []struct {
		name   string
		want   string
		mutate func(*testing.T, *StagedRendition)
	}{
		{
			name: "noncanonical normalized evidence bytes with forged checksums",
			want: "not exact canonical bytes",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				evidenceBytes, err := io.ReadAll(staged.Artifacts[0].Payload)
				require.NoError(t, err)
				evidenceBytes = append(evidenceBytes, '\n')
				evidenceHash := processingSHA256(evidenceBytes)
				staged.Artifacts[0].Payload = bytes.NewReader(evidenceBytes)
				staged.Build.Artifacts[0].BlobHash = evidenceHash
				staged.Build.Artifacts[0].Checksum = evidenceHash
				staged.Build.Artifacts[0].Size = int64(len(evidenceBytes))
				staged.Rendition.EvidenceChecksum = evidenceHash
				staged.Build.EvidenceChecksum = evidenceHash
				staged.Rendition.Checksum = processingRenditionChecksum(staged.Rendition)
				staged.Build.RenditionChecksum = staged.Rendition.Checksum
			},
		},
		{
			name: "search text unrelated to rendition unit with forged checksums",
			want: "does not match deterministic producer output",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				segment := &staged.Rendition.LexicalSegments[0]
				segment.Text = "forged searchable text"
				segment.CharEnd = len([]rune(segment.Text))
				segment.Checksum = processingChecksumStrings(
					document.RenditionContractV1, segment.UnitID, strconv.Itoa(segment.Order),
					strconv.Itoa(segment.CharStart), strconv.Itoa(segment.CharEnd), segment.Text,
				)
				segment.ID = "lexical_segment_" + segment.Checksum
				staged.Build.LexicalSegments[0] = store.RenditionLexicalSegmentRecord{
					ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
					CharStart: segment.CharStart, CharEnd: segment.CharEnd,
					Checksum: segment.Checksum, Text: segment.Text,
				}
				staged.Rendition.Checksum = processingRenditionChecksum(staged.Rendition)
				staged.Build.RenditionChecksum = staged.Rendition.Checksum
			},
		},
		{
			name: "self consistent rendition unrelated to producer output",
			want: "does not match deterministic producer output",
			mutate: func(t *testing.T, staged *StagedRendition) {
				t.Helper()
				unit := &staged.Rendition.Units[0]
				unit.Text = "fully forged but internally consistent text"
				unit.Checksum = processingChecksumStrings(
					document.RenditionContractV1, unit.EvidenceUnitID, strconv.Itoa(unit.Order),
					unit.Text, strings.Join(unit.HeadingPath, "\x00"),
				)
				unit.ID = "rendition_unit_" + unit.Checksum
				staged.Build.Units[0] = store.RenditionUnitRecord{
					ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
					Checksum: unit.Checksum, HeadingPath: append([]string(nil), unit.HeadingPath...),
					Locator: unit.Locator,
				}
				staged.Rendition.Markdown = []byte(unit.Text + "\n")
				staged.Rendition.MarkdownChecksum = processingSHA256(staged.Rendition.Markdown)
				staged.Build.MarkdownChecksum = staged.Rendition.MarkdownChecksum
				staged.Artifacts[1].Payload = bytes.NewReader(staged.Rendition.Markdown)
				staged.Build.Artifacts[1].BlobHash = staged.Rendition.MarkdownChecksum
				staged.Build.Artifacts[1].Checksum = staged.Rendition.MarkdownChecksum
				staged.Build.Artifacts[1].Size = int64(len(staged.Rendition.Markdown))

				segment := &staged.Rendition.LexicalSegments[0]
				segment.UnitID = unit.ID
				segment.Text = unit.Text
				segment.CharEnd = len([]rune(segment.Text))
				segment.Checksum = processingChecksumStrings(
					document.RenditionContractV1, segment.UnitID, strconv.Itoa(segment.Order),
					strconv.Itoa(segment.CharStart), strconv.Itoa(segment.CharEnd), segment.Text,
				)
				segment.ID = "lexical_segment_" + segment.Checksum
				staged.Build.LexicalSegments[0] = store.RenditionLexicalSegmentRecord{
					ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
					CharStart: segment.CharStart, CharEnd: segment.CharEnd,
					Checksum: segment.Checksum, Text: segment.Text,
				}
				staged.Rendition.Checksum = processingRenditionChecksum(staged.Rendition)
				staged.Build.RenditionChecksum = staged.Rendition.Checksum
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
			require.NoError(t, err)
			staged := fixture.stage(t,
				publicationIDs{"bf", "5f", "9f"}, "canonical searchable text", "canonical heading",
			)
			testCase.mutate(t, &staged)

			_, err = publisher.PublishRendition(t.Context(), staged)
			require.ErrorContains(t, err, testCase.want)
			_, err = fixture.catalog.ActiveRendition(
				t.Context(), fixture.versionID, fixture.profile.Fingerprint,
			)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestPublishRenditionFailureAfterBlobClosePreservesPriorHeads(t *testing.T) {
	// Mutation caught: continuing after a blob writer reports a terminal-close
	// failure would make an unverified retained payload reachable.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	first, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "prior mercury evidence", "first markdown",
	))
	require.NoError(t, err)

	failingWriter := &failAfterClosedBlobWriter{Store: fixture.blobs}
	failingPublisher, err := NewArtifactPublisher(fixture.catalog, failingWriter)
	require.NoError(t, err)
	_, err = failingPublisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b2", "52", "92"}, "replacement venus evidence", "second markdown",
	))
	require.ErrorIs(t, err, errInjectedPublication)
	assertPriorPublicationServes(t, fixture, first)
}

func TestPublishRenditionFailureAfterCatalogStagePreservesPriorHeads(t *testing.T) {
	// Mutation caught: treating a staged immutable build as published would let
	// catalog membership bypass version-scoped attachment and head authority.
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	first, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"b1", "51", "91"}, "prior mercury evidence", "first markdown",
	))
	require.NoError(t, err)

	failingCatalog := &failAfterCatalogStage{renditionPublicationCatalog: fixture.catalog}
	failingPublisher, err := NewArtifactPublisher(failingCatalog, fixture.blobs)
	require.NoError(t, err)
	second := fixture.replacementStage(t,
		publicationIDs{"b2", "52", "92"}, "replacement venus evidence", "second markdown",
	)
	_, err = failingPublisher.PublishRendition(t.Context(), second)
	require.ErrorIs(t, err, errInjectedPublication)
	assertPriorPublicationServes(t, fixture, first)

	assert.True(t, failingCatalog.staged,
		"the injected failure occurs after the immutable build is staged")
}

type publicationFixture struct {
	catalog         *store.Store
	blobs           *blob.Store
	profile         store.ProcessingProfileRecord
	evidencePolicy  document.EvidencePolicy
	renditionPolicy document.RenditionPolicy
	versionID       string
}

type publicationIDs struct {
	build, attachment, generation string
}

func newPublicationFixture(t *testing.T) publicationFixture {
	t.Helper()
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })

	source := []byte("synthetic private-free source")
	receipt, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	physical := processingBlobPhysical(t, receipt)
	node, err := catalog.CreateFile(
		t.Context(), catalog.RootID(), "source.pdf", receipt.Hash, receipt.Size,
		"application/pdf", physical,
	)
	require.NoError(t, err)
	evidencePolicy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	normalizationPolicy, err := document.NewNormalizePolicy(100_000)
	require.NoError(t, err)
	renditionPolicy, err := document.NewRenditionPolicy(normalizationPolicy)
	require.NoError(t, err)
	return publicationFixture{
		catalog: catalog, blobs: blobs, profile: processingProfile(t),
		evidencePolicy: evidencePolicy, renditionPolicy: renditionPolicy,
		versionID: node.CurrentVersionID,
	}
}

func (f publicationFixture) stage(
	t *testing.T,
	ids publicationIDs, lexicalText, markdown string,
) StagedRendition {
	t.Helper()
	return f.stageForSource(t, ids, lexicalText, markdown, f.versionID, f.mustSourceHash())
}

func (f publicationFixture) replacementStage(
	t *testing.T, ids publicationIDs, lexicalText, markdown string,
) StagedRendition {
	t.Helper()
	source := []byte("replacement source " + ids.build)
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(
		t.Context(), f.catalog.RootID(), "replacement-"+ids.build+".pdf",
		receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt),
	)
	require.NoError(t, err)
	return f.stageForSource(t, ids, lexicalText, markdown, node.CurrentVersionID, receipt.Hash)
}

func (f publicationFixture) stageForSource(
	t *testing.T,
	ids publicationIDs, lexicalText, markdown, versionID, sourceHash string,
) StagedRendition {
	t.Helper()
	normalizedEvidence, err := document.NormalizeEvidenceV1(document.SourceEvidenceV1{
		ContractVersion: document.SourceEvidenceContractV1,
		Completeness:    document.EvidenceComplete,
		Family:          "pdf",
		UnitKind:        document.EvidenceUnitPage,
		Units: []document.SourceEvidenceUnitV1{{
			Order: 0, Text: lexicalText, HeadingPath: []string{markdown},
			Locator: document.SourceEvidenceLocatorV1{
				Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne,
				Start: 1, End: 1,
			},
		}},
	}, f.evidencePolicy)
	require.NoError(t, err)
	evidence, evidenceHash, err := document.MarshalNormalizedEvidenceV1(normalizedEvidence)
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalizedEvidence, f.renditionPolicy)
	require.NoError(t, err)
	markdownBytes := rendition.Markdown
	markdownHash := rendition.MarkdownChecksum
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	warnings := make([]string, len(rendition.Warnings))
	for index, warning := range rendition.Warnings {
		warnings[index] = warning.Code
	}
	units := make([]store.RenditionUnitRecord, len(rendition.Units))
	for index, unit := range rendition.Units {
		units[index] = store.RenditionUnitRecord{
			ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
			Checksum: unit.Checksum, HeadingPath: append([]string(nil), unit.HeadingPath...),
			Locator: unit.Locator,
		}
	}
	segments := make([]store.RenditionLexicalSegmentRecord, len(rendition.LexicalSegments))
	for index, segment := range rendition.LexicalSegments {
		segments[index] = store.RenditionLexicalSegmentRecord{
			ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
			CharStart: segment.CharStart, CharEnd: segment.CharEnd,
			Checksum: segment.Checksum, Text: segment.Text,
		}
	}
	build := store.RenditionBuildRecord{
		ID: processingHash(ids.build), VaultID: f.catalog.VaultID(),
		SourceSHA256:                      sourceHash,
		RenditionRequestFingerprint:       f.profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        f.profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: processingSHA256(policy),
		CapturedArtifactPolicy:            policy, AuthorizationChecksum: processingHash(ids.build + "a6"),
		ProviderOperationID: "synthetic-operation-" + ids.build,
		ProviderReceipt:     jsontext.Value(`{"provider":"synthetic","request_id":"` + ids.build + `"}`),
		EvidenceChecksum:    evidenceHash, RenditionChecksum: rendition.Checksum,
		MarkdownChecksum: markdownHash, Completeness: document.EvidenceComplete,
		Warnings: warnings, CompletedAt: "2026-08-22T09:00:00.000000000Z",
		DeclaredArtifactCount: 2,
		Artifacts: []store.RenditionArtifactRecord{
			{ID: "artifact_" + processingHash(ids.build+"21"), Role: "normalized_evidence", BlobHash: evidenceHash,
				Size: int64(len(evidence)), Checksum: evidenceHash, State: store.RenditionArtifactVerified},
			{ID: "artifact_" + processingHash(ids.build+"22"), Role: "sanitized_markdown", BlobHash: markdownHash,
				Size: int64(len(markdownBytes)), Checksum: markdownHash, State: store.RenditionArtifactVerified},
		},
		Units: units, LexicalSegments: segments,
	}
	attachment := store.RenditionAttachmentRecord{
		ID: processingHash(ids.attachment), VaultID: f.catalog.VaultID(),
		ContentVersionID: versionID, BuildID: build.ID, Profile: f.profile,
		AttachedAt: "2026-08-22T10:00:00.000000000Z",
	}
	return StagedRendition{
		Rendition: rendition, RenditionPolicy: f.renditionPolicy,
		Build: build, Attachment: attachment,
		Head: store.RenditionHeadRecord{
			ContentVersionID: versionID, ProcessingProfileFingerprint: f.profile.Fingerprint,
			AttachmentID: attachment.ID, PublishedAt: "2026-08-22T10:01:00.000000000Z",
		},
		LexicalGenerationID: processingHash(ids.generation),
		Artifacts: []StagedArtifact{
			{ID: build.Artifacts[0].ID, Payload: bytes.NewReader(evidence)},
			{ID: build.Artifacts[1].ID, Payload: bytes.NewReader(markdownBytes)},
		},
	}
}

func (f publicationFixture) mustSourceHash() string {
	node, err := f.catalog.NodeByPath(context.Background(), "/source.pdf")
	if err != nil {
		panic(err)
	}
	return node.BlobHash
}

func processingProfile(t *testing.T) store.ProcessingProfileRecord {
	t.Helper()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "rendition-adapter/v1", AuthorizationFingerprint: processingHash("a1"),
			CredentialBinding: "credential:synthetic", DeploymentFingerprint: processingHash("a2"),
			Descriptor:            document.ProviderDescriptorV1{ID: "synthetic-rendition", Fingerprint: processingHash("a3")},
			DisclosureFingerprint: processingHash("a4"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 100, Name: "primary",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      "synthetic-vault", UploadOptionsFingerprint: processingHash("a5"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: processingHash("b1"), LexicalSegmenterFingerprint: processingHash("b2"),
			MaxSegmentRunes: 100, MaxUnitRunes: 1000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      processingHash("b3"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: processingHash("b4"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: processingHash("c1"), ConsentFingerprint: processingHash("c2"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true, TrustBoundary: "synthetic-vault",
		},
	}
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	return store.ProcessingProfileRecord{
		Fingerprint: fingerprints.Profile, CanonicalProfile: jsontext.Value(canonical),
		RenditionRequestFingerprint:    fingerprints.RenditionRequest,
		EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
		RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
		AttachmentPolicyFingerprint:    profile.RetentionDisclosure.AttachmentPolicyFingerprint,
		ConsentFingerprint:             profile.RetentionDisclosure.ConsentFingerprint,
		RenditionDisclosureFingerprint: profile.Rendition.DisclosureFingerprint,
		TrustBoundary:                  profile.RetentionDisclosure.TrustBoundary,
	}
}

func processingBlobPhysical(t *testing.T, receipt blob.WriteReceipt) store.BlobPhysical {
	t.Helper()
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	return store.BlobPhysical{
		Encoding: encoding, StoredBytes: receipt.StoredSize,
		PackEligible: receipt.PackEligible, Created: receipt.Created,
	}
}

type failAfterClosedBlobWriter struct {
	*blob.Store

	failed bool
}

func (w *failAfterClosedBlobWriter) WriteDetailedContext(
	ctx context.Context, reader io.Reader,
) (blob.WriteReceipt, error) {
	receipt, err := w.Store.WriteDetailedContext(ctx, reader)
	if err == nil && !w.failed {
		w.failed = true
		return receipt, errInjectedPublication
	}
	return receipt, err
}

type failAfterCatalogStage struct {
	renditionPublicationCatalog

	staged bool
}

func (c *failAfterCatalogStage) StageRenditionBuild(
	ctx context.Context, record store.RenditionBuildRecord,
) error {
	if err := c.renditionPublicationCatalog.StageRenditionBuild(ctx, record); err != nil {
		return err
	}
	c.staged = true
	return errInjectedPublication
}

func assertPriorPublicationServes(
	t *testing.T, fixture publicationFixture, prior PublishedRendition,
) {
	t.Helper()
	view, err := fixture.catalog.ActiveRendition(
		t.Context(), fixture.versionID, fixture.profile.Fingerprint,
	)
	require.NoError(t, err)
	assert.Equal(t, prior.BuildID, view.Build.ID)
	active, err := fixture.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, prior.LexicalGeneration, active)
	hits, _, err := fixture.catalog.SearchPage(t.Context(), "mercury", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	hits, _, err = fixture.catalog.SearchPage(t.Context(), "venus", 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
}

func processingHash(seed string) string { return processingSHA256([]byte(seed)) }

func processingSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func processingChecksumStrings(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(hash, strconv.Itoa(len(value)))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func processingRenditionChecksum(rendition document.RenditionV1) string {
	parts := []string{
		document.RenditionContractV1, rendition.EvidenceChecksum,
		string(rendition.Completeness), rendition.MarkdownChecksum,
	}
	for _, unit := range rendition.Units {
		parts = append(parts, unit.Checksum)
	}
	for _, segment := range rendition.LexicalSegments {
		parts = append(parts, segment.Checksum)
	}
	for _, warning := range rendition.Warnings {
		parts = append(parts, warning.Code)
	}
	return processingChecksumStrings(parts...)
}
