package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestOpenAPIDocumentOffline(t *testing.T) {
	// No store, no blobs, no listener: registration must not touch deps.
	out, err := api.OpenAPIYAML()
	require.NoError(t, err)
	doc := string(out)
	for _, op := range []string{"getNode", "resolvePath", "listChildren", "getNodeContent", "verifyNodeContent",
		"listContentVersions", "getContentVersion", "getContentVersionBytes",
		"search", "createNode", "moveNode", "movePath", "trashNode", "trashPath", "restoreNode",
		"storageStatus", "storagePack", "storageRepack", "ingest", "uploadFile", "listTrash", "emptyTrash", "gc", "verify",
		"initBackupRepository", "createBackupSnapshot", "listBackupSnapshots", "listJobs"} {
		assert.Contains(t, doc, op, "operation missing from OpenAPI doc")
	}
	assert.NotContains(t, doc, "/api/daemon/shutdown", "lifecycle plumbing must stay hidden")
	assert.NotContains(t, doc, "/api/daemon/challenge", "lifecycle plumbing must stay hidden")
	assert.Contains(t, doc, "X-Docbank-Blob-Hash")
	assert.Contains(t, doc, api.ContentVersionHeader)
	assert.Contains(t, doc, "Content-Digest")
	assert.Contains(t, doc, "computed_hash")
}

func TestOpenAPIDeclaresSecurity(t *testing.T) {
	// Generated clients must learn from the document alone that every
	// operation needs credentials (X-Api-Key header or bearer token).
	out, err := api.OpenAPIYAML()
	require.NoError(t, err)
	doc := string(out)
	assert.Contains(t, doc, "securitySchemes")
	assert.Contains(t, doc, "X-Api-Key")
	assert.Contains(t, doc, "scheme: bearer")
	assert.Contains(t, doc, "security:", "document-level security requirement missing")
}

func TestOpenAPIDeclaresDigestCheckedUpload(t *testing.T) {
	doc := api.NewOfflineServer().API().OpenAPI()
	op := doc.Paths["/api/v1/uploads"].Post
	require.NotNil(t, op)
	assert.Equal(t, "uploadFile", op.OperationID)
	form := op.RequestBody.Content["multipart/form-data"]
	require.NotNil(t, form)
	assert.Equal(t, "binary", form.Schema.Properties["file"].Format)
	assert.Contains(t, form.Schema.Required, "file")

	required := map[string]bool{}
	for _, param := range op.Parameters {
		required[param.Name] = param.Required
	}
	for _, name := range []string{"parent_id", "name", api.BlobHashHeader, api.BlobSizeHeader} {
		assert.True(t, required[name], "%s must be required", name)
	}
	assert.NotNil(t, op.Responses["200"])
	assert.NotNil(t, op.Responses["201"])

	replace := doc.Paths["/api/v1/nodes/{id}/content"].Put
	require.NotNil(t, replace)
	require.NotNil(t, replace.RequestBody)
	assert.Contains(t, replace.RequestBody.Content, "*/*")
	assert.NotNil(t, replace.Responses["200"])

	revert := doc.Paths["/api/v1/nodes/{id}/revert"].Post
	require.NotNil(t, revert)
	require.NotNil(t, revert.RequestBody)
	assert.Contains(t, revert.RequestBody.Content, "application/json")
	assert.NotNil(t, revert.Responses["200"])
}
