package marker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestLocalUnitsReportsCancellationInsteadOfInvalidPDF(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := (&Client{}).localUnits(ctx, document.AuthorizedUploadMetadata{MediaFamily: "pdf"}, testPDF(1))

	require.ErrorIs(t, err, context.Canceled)
	var classified *document.RenditionProviderError
	require.NotErrorAs(t, err, &classified, "a cancelled count must not be recorded as unsupported input")
}
