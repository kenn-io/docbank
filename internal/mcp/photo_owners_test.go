package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestPhotoOwnerAdministrationStaysReadOnly(t *testing.T) {
	readOnly := catalogMap(toolCatalog(false, false, false))
	owners := readOnly["list_photo_owners"]
	require.NotNil(t, owners)
	require.NotNil(t, owners.Annotations)
	assert.True(t, owners.Annotations.ReadOnlyHint)

	all := catalogMap(toolCatalog(true, true, true))
	for _, name := range []string{"add_photo_owner", "rename_photo_owner", "remove_photo_owner"} {
		assert.NotContains(t, all, name)
	}
}

func TestListPhotoOwnersReadsDaemonOwners(t *testing.T) {
	owner := api.PhotoOwner{ID: "00000000-0000-4000-8000-000000000001", Name: "Ada", EnrolledAt: "2026-09-22T00:00:00Z"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/photos/owners" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, []api.PhotoOwner{owner}))
	}))
	t.Cleanup(server.Close)
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(server.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	validator := mustResolveSchema(catalogMap(toolCatalog(false, false, false))["list_photo_owners"].OutputSchema)

	result, err := executeReadTool(t.Context(), lease, nil, "list_photo_owners", validator, []byte(`{}`))
	require.NoError(t, err)
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok)
	owners, ok := structured["owners"].([]any)
	require.True(t, ok)
	require.Len(t, owners, 1)
	assert.Equal(t, map[string]any{"id": owner.ID, "name": "Ada", "enrolled_at": owner.EnrolledAt}, owners[0])
}
