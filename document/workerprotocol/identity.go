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
		{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "text", MediaType: "text/markdown", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
		{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
	}})
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
	authorization.AuthorizedAt = ""
	authorization.ExpiresAt = ""
	encoded, err := json.Marshal(struct {
		Contract      string                            `json:"contract"`
		Metadata      document.AuthorizedUploadMetadata `json:"metadata"`
		Authorization document.RenditionAuthorization   `json:"authorization"`
		Fields        [][2]string                       `json:"fields"`
	}{Contract, metadata, authorization, fields})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ReplaySafeBinding recognizes the exact durable worker protocol, never a
// provider's ID alone. This only permits re-preparation: current consent,
// runtime disclosure and request allowlists are still checked before egress.
func ReplaySafeBinding(canonicalProfile []byte, identity document.RenditionExecutionIdentityV1) bool {
	var profile document.ProcessingProfileV1
	if json.Unmarshal(canonicalProfile, &profile, json.RejectUnknownMembers(true)) != nil || profile.Rendition == nil {
		return false
	}
	binding := profile.Rendition
	auth := identity.Authorization
	if binding.TrustBoundary != string(document.RenditionTrustOperatorNetwork) || auth.ProviderID != ProviderID || auth.InputKind != document.RenditionInputOriginalFile || identity.Upload.InputKind != auth.InputKind {
		return false
	}
	var descriptor document.RenditionDescriptor
	var err error
	switch binding.AdapterContract {
	case DocumentAdapter:
		descriptor, err = DocumentDescriptor()
	case ASRAdapter:
		descriptor, err = ASRDescriptor(auth.PolicyFingerprint)
	default:
		return false
	}
	if err != nil || binding.Descriptor.ID != descriptor.ID || binding.Descriptor.Fingerprint != descriptor.Fingerprint || auth.DescriptorFingerprint != descriptor.Fingerprint || auth.PolicyFingerprint != descriptor.PolicyFingerprint {
		return false
	}
	_, fingerprints, err := document.CanonicalProfile(profile)
	return err == nil && fingerprints.RenditionRequest == auth.RenditionRequestFingerprint
}
