package mistral

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestVerifySourceReportsCancellationInsteadOfInvalidPDF(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	metadata := document.AuthorizedUploadMetadata{MediaFamily: "pdf", MediaType: "application/pdf"}

	_, _, err := (&RenditionClient{}).verifySource(ctx, testPDF("cancelled"), metadata)

	require.ErrorIs(t, err, context.Canceled)
	var classified *document.RenditionProviderError
	require.NotErrorAs(t, err, &classified, "a cancelled count must not be recorded as unsupported input")
}
