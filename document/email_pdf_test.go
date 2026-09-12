package document

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmailPDFProfilePinsDecodedGenerationAndPaper(t *testing.T) {
	b := EmailPDFBindingV1{GenerationID: strings.Repeat("a", 64), GenerationChecksum: strings.Repeat("b", 64), Recipe: EmailPDFRecipeV1{Contract: "email-pdf-v1", RendererVersion: "151.0.7922.34", RendererSHA256: strings.Repeat("c", 64), FontsSHA256: strings.Repeat("d", 64), Paper: "A4"}}
	b.Recipe.WorkerSHA256 = strings.Repeat("a", 64)
	p, err := EmailPDFProfile(b)
	require.NoError(t, err)
	_, first, err := CanonicalProfile(p)
	require.NoError(t, err)
	p.Rendition.EmailPDF.Recipe.WorkerSHA256 = strings.Repeat("f", 64)
	_, workerChanged, err := CanonicalProfile(p)
	require.NoError(t, err)
	require.NotEqual(t, first.RenditionRequest, workerChanged.RenditionRequest)
	p.Rendition.EmailPDF.Recipe.Paper = "Letter"
	_, second, err := CanonicalProfile(p)
	require.NoError(t, err)
	require.NotEqual(t, first.RenditionRequest, second.RenditionRequest)
	p.Rendition.EmailPDF.GenerationID = strings.Repeat("e", 64)
	_, third, err := CanonicalProfile(p)
	require.NoError(t, err)
	require.NotEqual(t, second.RenditionRequest, third.RenditionRequest)
	p.Rendition.EmailPDF.Recipe.Paper = "A3"
	_, _, err = CanonicalProfile(p)
	require.Error(t, err)
}
