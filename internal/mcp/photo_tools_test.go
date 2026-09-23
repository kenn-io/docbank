package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPhotoMCPWorkflowAndWriteOptIn(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false, false, false))
	assert.Contains(t, readOnly, "get_photo_asset")
	for _, name := range []string{
		"create_photo_asset", "attach_photo_file", "detach_photo_file",
		"exclude_photo_asset", "promote_photo_asset",
	} {
		assert.NotContains(t, readOnly, name)
	}

	withWrites := catalogMap(toolCatalog(false, false, true))
	for _, name := range []string{
		"create_photo_asset", "attach_photo_file", "detach_photo_file",
		"exclude_photo_asset", "promote_photo_asset",
	} {
		tool := withWrites[name]
		require.NotNil(t, tool)
		require.NotNil(t, tool.Annotations)
		assert.False(t, tool.Annotations.ReadOnlyHint)
		assertSchemaContract(t, tool.InputSchema, true)
		assertSchemaContract(t, tool.OutputSchema, true)
	}
	assert.NotContains(t, withWrites, "set_photo_display")
	assert.NotContains(t, withWrites, "set_photo_settings")

	server := newServerWithOptions(testImplementation(), ServerOptions{AllowPhotoEdits: true})
	discovery := decodeResult(t, exchangeRaw(t, server, requestFor("server/discover", nil)))
	assert.Equal(t, catalogInstructions(false, false, true), discovery["instructions"])
}

func TestPhotoWriteNoReplay(t *testing.T) {
	var requests atomic.Int32
	disconnected := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			return
		}
		connection, _, err := hijacker.Hijack()
		if err == nil {
			_ = connection.Close()
		}
	}))
	t.Cleanup(disconnected.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(disconnected.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	validator := mustResolveSchema(catalogMap(toolCatalog(false, false, true))["create_photo_asset"].OutputSchema)

	_, err := executePhotoWriteTool(t.Context(), lease, "create_photo_asset", validator,
		[]byte(`{"node_id":7}`))
	require.Error(t, err)
	require.ErrorIs(t, err, errProcessingOutcomeUnknown)
	assert.Equal(t, int32(1), requests.Load())
}
