package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAttachmentNavigationBrowserReadsAreExactAndBounded(t *testing.T) {
	const version = "11111111-1111-4111-8111-111111111111"
	for _, test := range []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodGet, "/api/v1/versions/" + version, true},
		{http.MethodGet, "/api/v1/email-document-publications/synthetic-1", true},
		{http.MethodGet, "/api/v1/email-document-relations?parent_version_id=" + version + "&limit=50", true},
		{http.MethodGet, "/api/v1/email-document-relations?child_version_id=" + version + "&limit=50&after_operation_id=synthetic-1&after_order=50", true},
		{http.MethodPost, "/api/v1/email-document-publications", false},
		{http.MethodDelete, "/api/v1/email-document-publications/synthetic-1", false},
		{http.MethodPost, "/api/v1/email-document-processing", false},
		{http.MethodPost, "/api/v1/processing/consents", false},
		{http.MethodGet, "/api/v1/email-document-publications/synthetic-1?extra=1", false},
		{http.MethodGet, "/api/v1/email-document-publications/synthetic-1/extra", false},
		{http.MethodGet, "/api/v1/email-document-relations", false},
		{http.MethodGet, "/api/v1/email-document-relations?parent_version_id=" + version + "&child_version_id=" + version, false},
		{http.MethodGet, "/api/v1/email-document-relations?parent_version_id=" + version + "&limit=251", false},
		{http.MethodGet, "/api/v1/email-document-relations?parent_version_id=" + version + "&limit=50&limit=50", false},
		{http.MethodGet, "/api/v1/email-document-relations?parent_version_id=" + version + "&unknown=1", false},
		{http.MethodGet, "/api/v1/email-document-relations?parent_version_id=" + version + "&after_order=2", false},
		{http.MethodGet, "/api/v1/versions/" + version + "/content", false},
		{http.MethodGet, "/api/v1/versions/" + version + "?extra=1", false},
		{http.MethodGet, "/api/v1/versions/not-a-version", false},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			assert.Equal(t, test.allowed, webSessionRequestAllowed(httptest.NewRequest(test.method, test.path, nil)))
		})
	}
}
