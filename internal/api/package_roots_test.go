package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/loadfile"
)

func TestPackageRootRegistryBindsOwnerAndExpiresHandle(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "a.txt"), []byte("a"), 0o600))
	resolver, err := loadfile.NewResolver(root, nil)
	require.NoError(t, err)
	registry := newPackageRootRegistry()
	t.Cleanup(func() { require.NoError(t, registry.closeAll()) })
	now := time.Now().UTC()
	require.NoError(t, registry.register("owner-a", "preflight-a", "digest", now.Add(time.Hour).Format(time.RFC3339Nano), resolver))

	got, ok := registry.resolver("owner-a", "preflight-a", "digest", now)
	assert.True(t, ok)
	assert.Same(t, resolver, got)
	_, ok = registry.resolver("owner-b", "preflight-a", "digest", now)
	assert.False(t, ok)
	_, ok = registry.resolver("owner-a", "preflight-b", "digest", now)
	assert.False(t, ok)
	_, ok = registry.resolver("owner-a", "preflight-a", "other-digest", now)
	assert.False(t, ok)
	_, ok = registry.resolver("owner-a", "preflight-a", "digest", now.Add(2*time.Hour))
	assert.False(t, ok)
}
