package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBrowserSessionsCreateFoldersForFolderUploads(t *testing.T) {
	t.Parallel()
	assert.True(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/nodes", nil)))
	assert.False(t, webSessionRequestAllowed(httptest.NewRequest(http.MethodPost, "/api/v1/nodes?parent_id=1", nil)))
}
