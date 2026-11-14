package workerprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

// DocumentDescriptor declares only the gateway pilot's bounded document types.
func DocumentDescriptor() (document.RenditionDescriptor, error) {
	sum := sha256.Sum256([]byte(DocumentAdapter))
	return document.NewRenditionDescriptor(document.RenditionDescriptor{ID: ProviderID, ContractVersion: document.RenditionProviderContractVersion, PolicyFingerprint: hex.EncodeToString(sum[:]), TrustBoundary: document.RenditionTrustOperatorNetwork, ReturnsMarkdown: true, ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, SupportedFormats: []document.RenditionFormatCapability{
		{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
	}})
}

type submissionKeyV1 struct {
	Filename, MediaFamily, MediaType, SHA256, CapabilityRecordChecksum, ProviderMetadataChecksum, InputBinding                     string
	ByteLength                                                                                                                     int64
	InputKind                                                                                                                      document.RenditionInputKind
	ProviderID, DescriptorFingerprint, PolicyFingerprint, RenditionRequestFingerprint                                              string
	SourceSHA256                                                                                                                   string
	SourceBytes                                                                                                                    int64
	AuthorizationCapabilityRecordChecksum, AuthorizationProviderMetadataChecksum, AuthorizationMediaFamily, AuthorizationMediaType string
	AuthorizationInputKind                                                                                                         document.RenditionInputKind
	DiscloseFilename                                                                                                               bool
	AllowedArtifactRoles                                                                                                           []document.EvidenceArtifactRole
	MaxProviderMarkdownBytes, MaxArtifactBytes, MaxArtifacts, MaxTotalResultBytes                                                  int
	Fields                                                                                                                         [][2]string
}

// ASRDescriptor binds a qualified, bounded transcript policy to original WAV/MP3.
func ASRDescriptor(policyFingerprint string) (document.RenditionDescriptor, error) {
	return document.NewRenditionDescriptor(document.RenditionDescriptor{ID: ProviderID, ContractVersion: document.RenditionProviderContractVersion, PolicyFingerprint: policyFingerprint, TrustBoundary: document.RenditionTrustOperatorNetwork, ReturnsStructured: true, ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript}, SupportedFormats: []document.RenditionFormatCapability{
		{MediaFamily: "audio", MediaType: "audio/wav", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "audio", MediaType: "audio/mpeg", InputKind: document.RenditionInputOriginalFile},
	}})
}

// SubmissionKey identifies the exact provider-visible request, excluding only
// the transient authorization interval. It is never a provider-issued handle.
func SubmissionKey(metadata document.AuthorizedUploadMetadata, authorization document.RenditionAuthorization, fields [][2]string) (string, error) {
	if _, err := authorization.Fingerprint(); err != nil {
		return "", err
	}
	if authorization.ProviderID != ProviderID || metadata.InputKind != document.RenditionInputOriginalFile || metadata.InputKind != authorization.InputKind || metadata.SHA256 != authorization.SourceSHA256 || metadata.ByteLength != authorization.SourceBytes || metadata.CapabilityRecordChecksum != authorization.CapabilityRecordChecksum || metadata.ProviderMetadataChecksum != authorization.ProviderMetadataChecksum || metadata.MediaType != authorization.MediaType || metadata.MediaFamily != authorization.MediaFamily || !utf8.ValidString(metadata.Filename) || len(metadata.Filename) > 255 || strings.ContainsAny(metadata.Filename, "/\\\x00\r\n") {
		return "", errors.New("worker submission metadata differs from authority")
	}
	if !authorization.DiscloseFilename {
		metadata.Filename = ""
	}
	encoded, err := json.Marshal(submissionKeyV1{
		Filename: metadata.Filename, MediaFamily: metadata.MediaFamily, MediaType: metadata.MediaType, ByteLength: metadata.ByteLength,
		SHA256: metadata.SHA256, CapabilityRecordChecksum: metadata.CapabilityRecordChecksum, ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		InputKind: metadata.InputKind, InputBinding: metadata.InputBinding, ProviderID: authorization.ProviderID,
		DescriptorFingerprint: authorization.DescriptorFingerprint, PolicyFingerprint: authorization.PolicyFingerprint,
		RenditionRequestFingerprint: authorization.RenditionRequestFingerprint, SourceSHA256: authorization.SourceSHA256, SourceBytes: authorization.SourceBytes,
		AuthorizationCapabilityRecordChecksum: authorization.CapabilityRecordChecksum, AuthorizationProviderMetadataChecksum: authorization.ProviderMetadataChecksum,
		AuthorizationMediaFamily: authorization.MediaFamily, AuthorizationMediaType: authorization.MediaType, AuthorizationInputKind: authorization.InputKind,
		DiscloseFilename: authorization.DiscloseFilename, AllowedArtifactRoles: authorization.AllowedArtifactRoles,
		MaxProviderMarkdownBytes: authorization.MaxProviderMarkdownBytes, MaxArtifactBytes: authorization.MaxArtifactBytes,
		MaxArtifacts: authorization.MaxArtifacts, MaxTotalResultBytes: authorization.MaxTotalResultBytes, Fields: fields,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("docbank-worker-submission-v1\x00"), encoded...))
	return hex.EncodeToString(digest[:]), nil
}
