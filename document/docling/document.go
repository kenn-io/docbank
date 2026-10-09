package docling

import (
	"errors"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

// DocumentAdapterContract identifies the daemon's fixed document rendition policy.
const DocumentAdapterContract = "docbank-docling-document/v1"

// DocumentDescriptor returns the fixed document-format descriptor for the daemon.
// The existing client maps PDF page evidence and explicitly degrades other formats.
// Deployment addresses, credentials, and execution bounds are not portable identity.
func DocumentDescriptor(boundary document.RenditionTrustBoundary) (document.RenditionDescriptor, error) {
	if boundary != document.RenditionTrustOperatorNetwork && boundary != document.RenditionTrustHostedProvider {
		return document.RenditionDescriptor{}, errors.New("docling: document trust boundary must be operator_network or hosted_provider")
	}
	return document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: providerID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: providerutil.SHA256Hex([]byte(DocumentAdapterContract)), TrustBoundary: boundary,
		SupportedFormats: []document.RenditionFormatCapability{
			{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "word", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "presentation", MediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "spreadsheet", MediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "text", MediaType: "text/markdown", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "image", MediaType: "image/png", InputKind: document.RenditionInputOriginalFile},
			{MediaFamily: "image", MediaType: "image/jpeg", InputKind: document.RenditionInputOriginalFile},
		},
		ReturnsMarkdown: true, ReturnsStructured: true,
		ArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
	})
}

// DocumentDisclosureFingerprint binds the descriptor to its exact endpoint and
// deployment, using the same NUL-separated identity as the ASR runtime.
func DocumentDisclosureFingerprint(descriptor document.RenditionDescriptor, endpoint, deployment string) string {
	return providerutil.SHA256Hex([]byte(strings.Join([]string{
		DocumentAdapterContract, descriptor.ID, descriptor.Fingerprint, endpoint, deployment,
	}, "\x00")))
}
