package docling

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestDocumentDescriptorAndDisclosure(t *testing.T) {
	descriptor, err := DocumentDescriptor(document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	require.True(t, descriptor.ReturnsMarkdown)
	require.True(t, descriptor.ReturnsStructured)
	require.Equal(t, []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}, descriptor.ArtifactRoles)
	require.Len(t, descriptor.SupportedFormats, 8)
	hosted, err := DocumentDescriptor(document.RenditionTrustHostedProvider)
	require.NoError(t, err)
	require.NotEqual(t, descriptor.Fingerprint, hosted.Fingerprint)
	_, err = DocumentDescriptor(document.RenditionTrustLocalProcess)
	require.Error(t, err)
	deployment := strings.Repeat("a", 64)
	endpoint := "http://127.0.0.1:5001"
	fingerprint := DocumentDisclosureFingerprint(descriptor, endpoint, deployment)
	require.NotEqual(t, fingerprint, ASRDisclosureFingerprint(descriptor, endpoint, deployment))
	require.NotEqual(t, fingerprint, DocumentDisclosureFingerprint(descriptor, endpoint+"/", deployment))
	require.NotEqual(t, fingerprint, DocumentDisclosureFingerprint(descriptor, endpoint, strings.Repeat("b", 64)))
	require.NotEqual(t, fingerprint, DocumentDisclosureFingerprint(hosted, endpoint, deployment))
	// Callers receive independent descriptors; mutating one cannot change later identity.
	descriptor.SupportedFormats[0].MediaType = "synthetic/invalid"
	again, err := DocumentDescriptor(document.RenditionTrustOperatorNetwork)
	require.NoError(t, err)
	require.Equal(t, fingerprint, DocumentDisclosureFingerprint(again, endpoint, deployment))
}
