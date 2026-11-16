package blob

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestExistingReaderDoesNotRecoverHandoff(t *testing.T) {
	metadata, blobs, _, _ := placementTestVault(t)
	prior, err := blobs.readBackend.Ownership(t.Context())
	require.NoError(t, err)
	wrong := prior
	wrong.Epoch = "50000000-0000-4000-8000-000000000001"
	digest := strings.Repeat("a", 64)
	handoffRecord, err := NewPrimaryRestoreHandoff(blobs.dir, wrong, &digest)
	require.NoError(t, err)
	require.NoError(t, handoffRecord.Prepare(t.Context()))
	handoff := filepath.Join(blobs.dir, "tmp", primaryRestoreHandoffName)
	before, err := os.ReadFile(handoff)
	require.NoError(t, err)
	registry := NewRegistry(t.Context(), metadata.VaultID(), nil, nil)
	defer func() { require.NoError(t, registry.Close()) }()
	_, err = NewExistingReader(store.NewPackCatalog(metadata), blobs.dir, registry)
	require.ErrorIs(t, err, packstore.ErrStoreFenced)
	data, err := os.ReadFile(handoff)
	require.NoError(t, err)
	require.Equal(t, before, data)
}
