package client_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"testing"
)

func TestClientEmailPDFRequiresExactIdentityAndAvailableRuntime(t *testing.T) {
	c, _ := newClient(t, serverKey)
	_, err := c.RequestEmailPDF(t.Context(), document.EmailPDFRequest{VersionID: "not-a-version", Consent: true})
	require.ErrorContains(t, err, "version")
	_, err = c.RequestEmailPDF(t.Context(), document.EmailPDFRequest{VersionID: "12345678-1234-4234-8234-123456789abc", Consent: true})
	require.ErrorContains(t, err, "email_pdf_unavailable")
}
