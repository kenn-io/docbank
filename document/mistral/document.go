package mistral

import (
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

// DocumentAdapterContract identifies the daemon's PDF rendition policy.
const DocumentAdapterContract = "docbank-mistral-ocr/v1"

// DocumentDescriptor binds the daemon's PDF rendition to its capability evidence.
func DocumentDescriptor(policy Policy, manifest CapabilityManifest) (document.RenditionDescriptor, error) {
	fingerprint, err := policy.Fingerprint(manifest)
	if err != nil {
		return document.RenditionDescriptor{}, err
	}
	return document.NewRenditionDescriptor(document.RenditionDescriptor{
		ID: renditionProviderID, ContractVersion: document.RenditionProviderContractVersion,
		PolicyFingerprint: fingerprint, TrustBoundary: document.RenditionTrustHostedProvider,
		SupportedFormats: []document.RenditionFormatCapability{{MediaFamily: "pdf", MediaType: "application/pdf", InputKind: document.RenditionInputOriginalFile}},
		ReturnsMarkdown:  true, ReturnsStructured: true,
	})
}

// DisclosureFingerprint binds the PDF descriptor to its exact endpoint and deployment.
func DisclosureFingerprint(descriptor document.RenditionDescriptor, endpoint, deployment string) string {
	return providerutil.RenditionDisclosureFingerprint(DocumentAdapterContract, descriptor, endpoint, deployment)
}
