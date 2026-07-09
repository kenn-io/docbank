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
	for _, op := range []string{"getNode", "resolvePath", "listChildren", "getNodeContent",
		"search", "createNode", "moveNode", "movePath", "trashNode", "trashPath", "restoreNode",
		"ingest", "listTrash", "emptyTrash", "gc", "verify"} {
		assert.Contains(t, doc, op, "operation missing from OpenAPI doc")
	}
	assert.NotContains(t, doc, "/api/daemon/shutdown", "lifecycle plumbing must stay hidden")
}
