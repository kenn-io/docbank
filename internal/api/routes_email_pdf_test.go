package api_test

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestEmailPDFUnavailableDoesNotQueueOrFabricateOutput(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	response, body := do(t, ts, http.MethodPost, "/api/v1/email-pdfs", nil, map[string]any{"version_id": "00000000-0000-4000-8000-000000000001", "generation_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "paper": "A4", "consent": true})
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, body)
	require.Contains(t, body, "email_pdf_unavailable")
}
