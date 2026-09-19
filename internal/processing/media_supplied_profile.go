package processing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/suppliedtranscript"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

const (
	SuppliedMediaProfileName   = "supplied-transcript"
	SuppliedCaptionProfileName = "supplied-captions"
)

func suppliedInputKind(profile string) (string, bool) {
	switch profile {
	case SuppliedMediaProfileName:
		return store.MediaInputTranscript, true
	case SuppliedCaptionProfileName:
		return store.MediaInputCaption, true
	default:
		return "", false
	}
}

type retainedTranscriptSource struct {
	catalog   *store.Store
	blobs     *blob.Store
	principal string
}

var errSuppliedInputInvalid = errors.New("supplied transcript bytes are invalid")

func (source retainedTranscriptSource) Transcript(
	ctx context.Context, sealedAudioSHA256 string,
) (document.SuppliedTranscript, error) {
	input, err := source.catalog.SuppliedTranscriptForSource(ctx, source.principal, sealedAudioSHA256)
	if errors.Is(err, store.ErrNotFound) {
		return document.SuppliedTranscript{}, nil
	}
	if err != nil {
		return document.SuppliedTranscript{}, err
	}
	return source.readTranscript(ctx, input)
}

func (source retainedTranscriptSource) TranscriptForBinding(
	ctx context.Context, sealedAudioSHA256, binding string,
) (document.SuppliedTranscript, error) {
	input, err := source.catalog.SuppliedTranscriptBindingForSource(
		ctx, source.principal, store.MediaInputTranscript, sealedAudioSHA256, binding)
	if errors.Is(err, store.ErrNotFound) {
		classified, classifyErr := document.NewRenditionProviderError(
			document.RenditionErrorPolicyRejected, 0, store.ErrNotFound)
		if classifyErr != nil {
			return document.SuppliedTranscript{}, classifyErr
		}
		return document.SuppliedTranscript{}, classified
	}
	if err != nil {
		return document.SuppliedTranscript{}, err
	}
	return source.readTranscript(ctx, input)
}

func (source retainedTranscriptSource) readTranscript(
	ctx context.Context, input store.SuppliedTranscriptInput,
) (document.SuppliedTranscript, error) {
	raw, _, err := source.readInput(ctx, input)
	if err != nil {
		return document.SuppliedTranscript{}, err
	}
	provider := input.Provider
	if provider == "" {
		provider = "supplied"
	}
	return document.SuppliedTranscript{Provider: provider, Text: string(raw)}, nil
}

func (source retainedTranscriptSource) readInput(
	ctx context.Context, input store.SuppliedTranscriptInput,
) ([]byte, store.ContentVersion, error) {
	version, err := source.catalog.ContentVersionByID(ctx, input.ContentVersionID)
	if err != nil {
		return nil, store.ContentVersion{}, err
	}
	reader, size, err := source.blobs.OpenStreamContext(ctx, version.BlobHash)
	if err != nil {
		return nil, store.ContentVersion{}, err
	}
	defer func() { _ = reader.Close() }()
	if size != version.Size || size < 1 || size > 16<<20 {
		return nil, store.ContentVersion{}, errors.New("supplied transcript bytes are outside bounds")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, 16<<20+1))
	if err != nil {
		return nil, store.ContentVersion{}, err
	}
	if int64(len(raw)) != size || !utf8.Valid(raw) || strings.TrimSpace(string(raw)) == "" {
		return nil, store.ContentVersion{}, errSuppliedInputInvalid
	}
	return raw, version, nil
}

func (source retainedTranscriptSource) CaptionForBinding(
	ctx context.Context, sealedSourceSHA256, binding string,
) (suppliedtranscript.Caption, error) {
	input, err := source.catalog.SuppliedTranscriptBindingForSource(
		ctx, source.principal, store.MediaInputCaption, sealedSourceSHA256, binding)
	if errors.Is(err, store.ErrNotFound) {
		classified, classifyErr := document.NewRenditionProviderError(
			document.RenditionErrorPolicyRejected, 0, store.ErrNotFound)
		if classifyErr != nil {
			return suppliedtranscript.Caption{}, classifyErr
		}
		return suppliedtranscript.Caption{}, classified
	}
	if err != nil {
		return suppliedtranscript.Caption{}, err
	}
	if input.Origin != "supplied" {
		classified, classifyErr := document.NewRenditionProviderError(
			document.RenditionErrorPolicyRejected, 0, errors.New("caption input is not supplied"))
		if classifyErr != nil {
			return suppliedtranscript.Caption{}, classifyErr
		}
		return suppliedtranscript.Caption{}, classified
	}
	raw, version, err := source.readInput(ctx, input)
	if err != nil {
		if errors.Is(err, errSuppliedInputInvalid) {
			classified, classifyErr := document.NewRenditionProviderError(
				document.RenditionErrorMalformedEvidence, 0, err)
			if classifyErr != nil {
				return suppliedtranscript.Caption{}, classifyErr
			}
			return suppliedtranscript.Caption{}, classified
		}
		return suppliedtranscript.Caption{}, err
	}
	mediaType, _, parseErr := mime.ParseMediaType(version.MimeType)
	if parseErr != nil || mediaType != "application/x-subrip" {
		classified, classifyErr := document.NewRenditionProviderError(
			document.RenditionErrorUnsupportedInput, 0, errors.New("caption input is not SubRip"))
		if classifyErr != nil {
			return suppliedtranscript.Caption{}, classifyErr
		}
		return suppliedtranscript.Caption{}, classified
	}
	provider := input.Provider
	if provider == "" {
		provider = "supplied"
	}
	return suppliedtranscript.Caption{Provider: provider, Language: input.Language, SRT: raw}, nil
}

// NewSuppliedMediaProfile constructs the daemon's executable local transcript
// profile over occurrence-authorized retained inputs.
func NewSuppliedMediaProfile(
	catalog *store.Store, blobs *blob.Store, principal string,
) (string, ProfileConfig, error) {
	if catalog == nil || blobs == nil || principal == "" {
		return "", ProfileConfig{}, errors.New("supplied media profile requires catalog, blobs, and principal")
	}
	sourceBinding := stableHash("docbank/supplied-media-source/v1", catalog.VaultID(), principal)
	provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
		Source:        retainedTranscriptSource{catalog: catalog, blobs: blobs, principal: principal},
		SourceBinding: sourceBinding, MaxDocumentChars: 16 << 20,
	})
	if err != nil {
		return "", ProfileConfig{}, err
	}
	descriptor := provider.Descriptor()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{ //nolint:gosec // Contains a stable non-secret local binding identity.
			Name: "supplied-transcript", AdapterContract: "rendition-adapter/v1",
			Descriptor:               document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			AuthorizationFingerprint: stableHash("docbank/supplied-media-authorization/v1", sourceBinding),
			CredentialBinding:        "credential:local-supplied-transcript",
			DeploymentFingerprint:    sourceBinding, DisclosureFingerprint: stableHash(
				"docbank/supplied-media-disclosure/v1", sourceBinding),
			TrustBoundary: string(descriptor.TrustBoundary), MaxDocumentBytes: 1 << 30,
			MaxResponseBytes: 16 << 20, MaxUnits: 1,
			RequestedArtifacts:       []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
			UploadOptionsFingerprint: stableHash("docbank/supplied-media-upload/v1", sourceBinding),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     stableHash("docbank/supplied-media-completeness/v1"),
			LexicalSegmenterFingerprint: stableHash("docbank/supplied-media-segmenter/v1"),
			MaxDocumentChars:            16 << 20, MaxSegmentRunes: 1 << 20, MaxUnitRunes: 1 << 20,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      stableHash("docbank/supplied-media-normalizer/v1"),
			RenditionContract:          document.RenditionContractV1,
			SanitizerFingerprint:       stableHash("docbank/supplied-media-sanitizer/v1"),
			SourceEvidenceContract:     document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: stableHash("docbank/supplied-media-attachments/v1"),
			ConsentFingerprint:          stableHash("docbank/supplied-media-consent/v1", sourceBinding),
			RetainSanitizedMarkdown:     true, RetainTypedArtifacts: true,
			TrustBoundary: string(descriptor.TrustBoundary),
		},
	}
	if _, _, err := document.CanonicalProfile(profile); err != nil {
		return "", ProfileConfig{}, fmt.Errorf("constructing supplied media processing profile: %w", err)
	}
	return SuppliedMediaProfileName, ProfileConfig{Profile: profile, RenditionProvider: provider}, nil
}

// NewSuppliedCaptionProfile constructs the executable local caption profile
// over occurrence-authorized retained inputs.
func NewSuppliedCaptionProfile(
	catalog *store.Store, blobs *blob.Store, principal string,
) (string, ProfileConfig, error) {
	if catalog == nil || blobs == nil || principal == "" {
		return "", ProfileConfig{}, errors.New("supplied caption profile requires catalog, blobs, and principal")
	}
	sourceBinding := stableHash("docbank/supplied-captions-source/v1", catalog.VaultID(), principal)
	provider, err := suppliedtranscript.NewCaptions(suppliedtranscript.CaptionProfile{
		Source:        retainedTranscriptSource{catalog: catalog, blobs: blobs, principal: principal},
		SourceBinding: sourceBinding, MaxDocumentChars: 16 << 20,
	})
	if err != nil {
		return "", ProfileConfig{}, err
	}
	descriptor := provider.Descriptor()
	profile := document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{ //nolint:gosec // Contains a stable non-secret local binding identity.
			Name: "supplied-captions", AdapterContract: "rendition-adapter/v1",
			Descriptor:               document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			AuthorizationFingerprint: stableHash("docbank/supplied-captions-authorization/v1", sourceBinding),
			CredentialBinding:        "credential:local-supplied-captions",
			DeploymentFingerprint:    sourceBinding, DisclosureFingerprint: stableHash(
				"docbank/supplied-captions-disclosure/v1", sourceBinding),
			TrustBoundary: string(descriptor.TrustBoundary), MaxDocumentBytes: 1 << 30,
			MaxResponseBytes: 16 << 20, MaxUnits: 25_000,
			RequestedArtifacts:       []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript},
			UploadOptionsFingerprint: stableHash("docbank/supplied-captions-upload/v1", sourceBinding),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint:     stableHash("docbank/supplied-captions-completeness/v1"),
			LexicalSegmenterFingerprint: stableHash("docbank/supplied-captions-segmenter/v1"),
			MaxDocumentChars:            16 << 20, MaxSegmentRunes: 1 << 20, MaxUnitRunes: 1 << 20,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      stableHash("docbank/supplied-captions-normalizer/v1"),
			RenditionContract:          document.RenditionContractV1,
			SanitizerFingerprint:       stableHash("docbank/supplied-captions-sanitizer/v1"),
			SourceEvidenceContract:     document.SourceEvidenceContractV1,
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 100, VectorLimit: 100},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: stableHash("docbank/supplied-captions-attachments/v1"),
			ConsentFingerprint:          stableHash("docbank/supplied-captions-consent/v1", sourceBinding),
			RetainSanitizedMarkdown:     true, RetainTypedArtifacts: true,
			TrustBoundary: string(descriptor.TrustBoundary),
		},
	}
	if _, _, err := document.CanonicalProfile(profile); err != nil {
		return "", ProfileConfig{}, fmt.Errorf("constructing supplied caption processing profile: %w", err)
	}
	return SuppliedCaptionProfileName, ProfileConfig{Profile: profile, RenditionProvider: provider}, nil
}
