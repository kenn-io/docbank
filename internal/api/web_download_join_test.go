package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmailPDFCannotUseOriginalPreviewPurpose(t *testing.T) {
	request := `{"node_id":1,"revision":1,"version_id":"selected","blob_hash":"` + strings.Repeat("a", 64) + `","size":10,"email_pdf_profile":"profile","email_pdf_attachment":"attachment","purpose":"preview"}`
	_, problem := decodeWebDownloadRequest(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, webDownloadPreparePath, strings.NewReader(request)))
	require.NotNil(t, problem, "a PDF receipt must not inherit the original email's preview eligibility")
	require.Equal(t, 422, problem.Status)
}
