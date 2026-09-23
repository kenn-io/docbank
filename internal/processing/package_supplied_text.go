package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/suppliedtext"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

const packageSuppliedTextMaxBytes = 4 << 20
const packageSuppliedTextCredential = "credential:" + "package-local"

// PublishPackageSuppliedText attaches one sender-provided text rendition to
// the exact native content version. The source and text versions remain
// separate immutable authorities; the native is never extracted or parsed.
func PublishPackageSuppliedText(ctx context.Context, catalog *store.Store, blobs *blob.Store,
	packageID, occurrenceID string, sourceVersion, textVersion store.ContentVersion, encoding string,
) (string, error) {
	if catalog == nil || blobs == nil {
		return "", errors.New("supplied-text publication requires catalog and blob stores")
	}
	if packageID == "" || occurrenceID == "" || sourceVersion.ID == textVersion.ID ||
		sourceVersion.ID == "" || textVersion.ID == "" {
		return "", errors.New("invalid package supplied-text authority")
	}
	sealedSource, err := catalog.ContentVersionByID(ctx, sourceVersion.ID)
	if err != nil {
		return "", err
	}
	sealedText, err := catalog.ContentVersionByID(ctx, textVersion.ID)
	if err != nil {
		return "", err
	}
	if sealedSource.BlobHash != sourceVersion.BlobHash || sealedSource.Size != sourceVersion.Size ||
		sealedText.BlobHash != textVersion.BlobHash || sealedText.Size != textVersion.Size {
		return "", errors.New("package supplied-text versions changed")
	}
	mediaType, _, err := mime.ParseMediaType(sealedSource.MimeType)
	if err != nil {
		return "", fmt.Errorf("invalid native media type: %w", err)
	}
	var family, filename string
	switch mediaType {
	case "application/pdf":
		family, filename = "pdf", "source.pdf"
	case "image/tiff":
		family, filename = "image", "source.tiff"
	case "text/plain":
		family, filename = "text", "source.txt"
	case "application/json":
		family, filename = "structured", "source.json"
	default:
		return "", fmt.Errorf("unsupported native media type %q for supplied text", mediaType)
	}
	decode, err := loadfile.Decoder(encoding)
	if err != nil {
		return "", err
	}
	if sealedText.Size > packageSuppliedTextMaxBytes {
		return "", errors.New("supplied text exceeds publication limit")
	}
	stream, size, err := blobs.OpenStreamContext(ctx, sealedText.BlobHash)
	if err != nil {
		return "", err
	}
	if size != sealedText.Size {
		return "", errors.Join(errors.New("supplied-text bytes differ from sealed version"), stream.Close())
	}
	decoded, readErr := io.ReadAll(io.LimitReader(decode(stream), packageSuppliedTextMaxBytes+1))
	if err := errors.Join(readErr, stream.Close()); err != nil {
		return "", err
	}
	if len(decoded) > packageSuppliedTextMaxBytes || strings.TrimSpace(string(decoded)) == "" {
		return "", errors.New("supplied text is empty or exceeds publication limit")
	}
	evidencePolicy, err := document.NewEvidencePolicy(packageSuppliedTextMaxBytes)
	if err != nil {
		return "", err
	}
	policyJSON, err := canonical.Marshal(evidencePolicy.Identity())
	if err != nil {
		return "", err
	}
	binding, err := suppliedtext.SourceBinding(packageID, occurrenceID, sealedSource.BlobHash,
		sealedText.BlobHash, encoding, renditionBytesSHA256(policyJSON))
	if err != nil {
		return "", err
	}
	provider, err := suppliedtext.New(suppliedtext.Profile{Source: packageSuppliedSource{
		digest: sealedSource.BlobHash, text: string(decoded),
	}, SourceBinding: binding, MaxDocumentChars: packageSuppliedTextMaxBytes})
	if err != nil {
		return "", err
	}
	profileValue := packageSuppliedTextProfile(provider.Descriptor(), binding)
	profile, err := pdfProfileRecord(profileValue)
	if err != nil {
		return "", err
	}
	if _, err := catalog.ActiveRendition(ctx, sealedSource.ID, profile.Fingerprint); err == nil {
		generation, generationErr := catalog.ActiveLexicalGeneration(ctx)
		if generationErr != nil {
			return "", generationErr
		}
		return generation.ID, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	for range 3 {
		generation, publishErr := publishPackageSuppliedText(ctx, catalog, blobs, provider, profileValue, profile,
			sealedSource, family, mediaType, filename, binding)
		if !errors.Is(publishErr, store.ErrLexicalGenerationStale) {
			return generation, publishErr
		}
	}
	return "", store.ErrLexicalGenerationStale
}

type packageSuppliedSource struct{ digest, text string }

func (s packageSuppliedSource) SuppliedText(_ context.Context, digest string) (document.SuppliedText, error) {
	if digest != s.digest {
		return document.SuppliedText{}, errors.New("supplied-text source digest differs from sealed native")
	}
	return document.SuppliedText{Provider: "package", Text: s.text}, nil
}

type packageSuppliedUpload struct {
	io.ReadCloser

	metadata document.AuthorizedUploadMetadata
}

func (u *packageSuppliedUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }

func publishPackageSuppliedText(ctx context.Context, catalog *store.Store, blobs *blob.Store,
	provider *suppliedtext.Provider, profileValue document.ProcessingProfileV1,
	profile store.ProcessingProfileRecord, source store.ContentVersion,
	family, mediaType, filename, binding string,
) (string, error) {
	stream, size, err := blobs.OpenStreamContext(ctx, source.BlobHash)
	if err != nil {
		return "", err
	}
	if size != source.Size {
		return "", errors.Join(errors.New("native bytes differ from sealed version"), stream.Close())
	}
	metadata := document.AuthorizedUploadMetadata{
		Filename: filename, MediaFamily: family, MediaType: mediaType,
		ByteLength: source.Size, SHA256: source.BlobHash,
		CapabilityRecordChecksum: renditionBytesSHA256([]byte("package-supplied-text-capability/v1\x00" + binding)),
		ProviderMetadataChecksum: renditionBytesSHA256([]byte("package-supplied-text-metadata/v1\x00" + binding)),
		InputKind:                document.RenditionInputOriginalFile, InputBinding: binding,
	}
	at := time.Now().UTC()
	descriptor := provider.Descriptor()
	authorization := document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: profile.RenditionRequestFingerprint,
		SourceSHA256:                source.BlobHash, SourceBytes: source.Size,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              family, MediaType: mediaType, InputKind: metadata.InputKind,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		MaxArtifacts:         1, MaxArtifactBytes: packageSuppliedTextMaxBytes + 1024,
		MaxTotalResultBytes: packageSuppliedTextMaxBytes + 1024,
		AuthorizedAt:        at.Format(emailObservationTimeLayout),
		ExpiresAt:           at.Add(5 * time.Minute).Format(emailObservationTimeLayout),
	}
	result, err := document.RenderRendition(ctx, provider,
		&packageSuppliedUpload{ReadCloser: stream, metadata: metadata}, authorization)
	if err != nil {
		return "", err
	}
	evidencePolicy, renditionPolicy, err := document.RenditionExecutionPoliciesForProfileV1(profileValue)
	if err != nil {
		return "", err
	}
	policy := jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"},{"max_count":1,"min_count":1,"role":"structured_evidence"}],"version":1}`)
	staged, err := stagePackageSuppliedText(catalog, profile, source, result,
		authorization, evidencePolicy, renditionPolicy, policy, at)
	if err != nil {
		return "", err
	}
	staged.LexicalGenerationID = renditionBytesSHA256([]byte(uuid.NewString()))
	publisher, err := NewArtifactPublisher(catalog, blobs)
	if err != nil {
		return "", err
	}
	published, err := publisher.PublishRendition(ctx, staged)
	if err != nil {
		return "", err
	}
	return published.LexicalGeneration.ID, nil
}

func stagePackageSuppliedText(catalog *store.Store, profile store.ProcessingProfileRecord,
	source store.ContentVersion, result document.RenditionResult,
	authorization document.RenditionAuthorization, evidencePolicy document.EvidencePolicy,
	renditionPolicy document.RenditionPolicy, policy jsontext.Value, at time.Time,
) (StagedRendition, error) {
	normalized, err := document.NormalizeEvidenceV1(result.Evidence, evidencePolicy)
	if err != nil {
		return StagedRendition{}, err
	}
	evidenceBytes, evidenceHash, err := document.MarshalNormalizedEvidenceV1(normalized)
	if err != nil {
		return StagedRendition{}, err
	}
	rendition, err := document.BuildRenditionV1(normalized, renditionPolicy)
	if err != nil {
		return StagedRendition{}, err
	}
	authorizationBytes, err := json.Marshal(authorization, json.Deterministic(true))
	if err != nil {
		return StagedRendition{}, err
	}
	receiptBytes, err := json.Marshal(result.Receipt, json.Deterministic(true))
	if err != nil {
		return StagedRendition{}, err
	}
	buildID := renditionBytesSHA256([]byte(uuid.NewString()))
	build := store.RenditionBuildRecord{
		ID: buildID, VaultID: catalog.VaultID(), SourceSHA256: source.BlobHash,
		RenditionRequestFingerprint:       profile.RenditionRequestFingerprint,
		EvidenceLexicalFingerprint:        profile.EvidenceLexicalFingerprint,
		CapturedArtifactPolicyFingerprint: renditionBytesSHA256(policy),
		CapturedArtifactPolicy:            policy,
		AuthorizationChecksum:             renditionBytesSHA256(authorizationBytes),
		ProviderOperationID:               result.Receipt.OperationID,
		ProviderReceipt:                   receiptBytes,
		EvidenceChecksum:                  evidenceHash, RenditionChecksum: rendition.Checksum,
		MarkdownChecksum: rendition.MarkdownChecksum, Completeness: rendition.Completeness,
		CompletedAt: result.Receipt.CompletedAt,
	}
	artifacts := []struct {
		role string
		data []byte
	}{
		{normalizedEvidenceRole, evidenceBytes},
		{sanitizedMarkdownRole, rendition.Markdown},
		{string(document.EvidenceArtifactStructured), result.Artifacts[0].Payload},
	}
	payloads := make([]StagedArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		hash := renditionBytesSHA256(artifact.data)
		id := renditionArtifactID(buildID, artifact.role, 0, hash)
		build.Artifacts = append(build.Artifacts, store.RenditionArtifactRecord{
			ID: id, Role: artifact.role, BlobHash: hash, Checksum: hash,
			Size: int64(len(artifact.data)), State: store.RenditionArtifactVerified,
		})
		payloads = append(payloads, StagedArtifact{ID: id, Payload: bytes.NewReader(artifact.data)})
	}
	build.DeclaredArtifactCount = len(build.Artifacts)
	for _, unit := range rendition.Units {
		build.Units = append(build.Units, store.RenditionUnitRecord{
			ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
			Checksum: unit.Checksum, HeadingPath: unit.HeadingPath, Locator: unit.Locator,
		})
	}
	for _, segment := range rendition.LexicalSegments {
		build.LexicalSegments = append(build.LexicalSegments, store.RenditionLexicalSegmentRecord{
			ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
			CharStart: segment.CharStart, CharEnd: segment.CharEnd,
			Checksum: segment.Checksum, Text: segment.Text,
		})
	}
	for _, warning := range rendition.Warnings {
		build.Warnings = append(build.Warnings, warning.Code)
	}
	attachmentID := store.RenditionAttachmentID(buildID, source.ID, profile.Fingerprint)
	attachedAt := at.Format(emailObservationTimeLayout)
	attachment := store.RenditionAttachmentRecord{
		ID: attachmentID, VaultID: catalog.VaultID(), ContentVersionID: source.ID,
		BuildID: buildID, Profile: profile, AttachedAt: attachedAt,
	}
	return StagedRendition{
		Rendition: rendition, RenditionPolicy: renditionPolicy, Build: build,
		Attachment: attachment,
		Head: store.RenditionHeadRecord{
			ContentVersionID: source.ID, ProcessingProfileFingerprint: profile.Fingerprint,
			AttachmentID: attachmentID, PublishedAt: attachedAt,
		}, Artifacts: payloads,
	}, nil
}

func packageSuppliedTextProfile(descriptor document.RenditionDescriptor, binding string) document.ProcessingProfileV1 {
	fp := func(field string) string {
		sum := sha256.Sum256([]byte("docbank-package-supplied-text-profile/v1\x00" + binding + "\x00" + field))
		return hex.EncodeToString(sum[:])
	}
	return document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Embeddings:      []document.EmbeddingBindingV1{},
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "docbank-package-supplied-text/v1", AuthorizationFingerprint: fp("local-authority"),
			CredentialBinding: packageSuppliedTextCredential, DeploymentFingerprint: fp("builtin-v1"),
			Descriptor:            document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			DisclosureFingerprint: fp("local-only"), MaxDocumentBytes: 4 << 30,
			MaxResponseBytes: 8 << 20, MaxUnits: 1, Name: "package-supplied-text-v1",
			RequestedArtifacts:       []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:            string(document.RenditionTrustLocalProcess),
			UploadOptionsFingerprint: fp("sealed-text-version"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: fp("sender-supplied-generic"), LexicalSegmenterFingerprint: fp("rune4096-v1"),
			MaxDocumentChars: packageSuppliedTextMaxBytes, MaxSegmentRunes: 4096,
			MaxUnitRunes:               packageSuppliedTextMaxBytes,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      fp("generic-text-v1"),
			RenditionContract:          document.RenditionContractV1, SanitizerFingerprint: fp("rendition-v1"),
			SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: fp("exact-native-version"), ConsentFingerprint: fp("builtin-local"),
			RetainSanitizedMarkdown: true, RetainTypedArtifacts: true,
			TrustBoundary: string(document.RenditionTrustLocalProcess),
		},
	}
}
