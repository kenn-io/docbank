package docbank_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/vision"
)

func TestEmbeddedVaultRejectsAVisionProfile(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	provider, err := vision.New(vision.Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, server.Client())
	require.NoError(t, err)
	profile := embeddedProcessingProfile(t, provider.Descriptor())
	profile.Rendition.AdapterContract = vision.AdapterContract
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	_, err = docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
		"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: docbank.ProcessingRuntimeDisclosure{Endpoint: server.URL}},
	}}})
	require.ErrorContains(t, err, "photo description needs the daemon")
}
