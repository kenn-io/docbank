package docbank_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/localface"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/config"
)

func TestEmbeddedFaceProfilesRequireDedicatedSetup(t *testing.T) {
	t.Parallel()
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "renamed", true: "mixed"}[mixed], func(t *testing.T) {
			t.Parallel()
			profile, descriptor, err := config.LocalFaces()
			require.NoError(t, err)
			provider, err := localface.New(descriptor, func(context.Context) (string, error) {
				t.Error("refused file requests must not start the provider")
				return "", errors.New("unexpected provider start")
			})
			require.NoError(t, err)
			supplied := docbank.ProcessingProfileConfig{Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{config.LocalFacesProfile: provider}}
			if mixed {
				renderer, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
				require.NoError(t, err)
				supplied.Profile = embeddedProcessingProfile(t, renderer.Descriptor())
				supplied.Profile.Embeddings = profile.Embeddings
				supplied.RenditionProvider = renderer
			}
			vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{"custom-faces": supplied}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			receipt, err := vault.Put(t.Context(), "/photo.jpg", strings.NewReader("synthetic photo"), docbank.PutOptions{MediaType: "image/jpeg"})
			require.NoError(t, err)
			request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "custom-faces"}}
			_, err = vault.PlanProcessing(t.Context(), request)
			require.ErrorIs(t, err, docbank.ErrProcessingProfileUnavailable)
			_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, Consent: true})
			require.ErrorIs(t, err, docbank.ErrProcessingProfileUnavailable)
			_, err = vault.GrantProcessingPlanConsent(t.Context(), docbank.ProcessingConsentGrantRequest{PlanRequest: request})
			require.ErrorIs(t, err, docbank.ErrProcessingProfileUnavailable)
			fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
			_, err = vault.DocumentCoverage(t.Context(), docbank.CoverageRequest{Profile: "custom-faces", Fence: fence})
			require.ErrorIs(t, err, docbank.ErrProcessingProfileUnavailable)
			_, err = vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "Ada", Mode: docbank.DocumentSearchLexical, Profile: "custom-faces", Fence: fence})
			require.ErrorIs(t, err, docbank.ErrProcessingProfileUnavailable)
		})
	}
}
