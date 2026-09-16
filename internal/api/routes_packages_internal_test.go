package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/loadfile"
)

func TestPackagePathReferenceUsesConfirmedLogicalVolumeRoots(t *testing.T) {
	root := t.TempDir()
	mapping := loadfile.Mapping{VolumeRoots: map[string]string{"VOL001": "DELIVERY/VOL001"}}
	volumes, err := logicalPackageVolumes([]loadfile.Volume{{Name: "DELIVERY", DeclaredRoot: "DELIVERY", Ordinal: 1}}, mapping)
	require.NoError(t, err)
	require.Equal(t, []loadfile.Volume{{Name: "VOL001", DeclaredRoot: "VOL001", Ordinal: 1}}, volumes)
	volume, relPath, err := packagePathReference(root, filepath.Join(root, "DELIVERY", "VOL001", "DATA", "a.dat"), volumes, mapping.VolumeRoots)
	require.NoError(t, err)
	assert.Equal(t, "VOL001", volume.Name)
	assert.Equal(t, "DATA/a.dat", relPath)
}

func TestPackageMemoryBudgetRejectsAggregateGrowth(t *testing.T) {
	budget := packageMemoryBudget{maximum: 10}
	require.NoError(t, budget.add(8))
	require.ErrorIs(t, budget.add(3), loadfile.ErrLoadfileLimit)
	assert.Equal(t, int64(8), budget.used)
}

func TestPackageHashingRejectsCancellationAndSourceGrowth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "VOL001"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "source.txt"), []byte("synthetic content"), 0o600))
	resolver, err := loadfile.NewResolver(t.Context(), root, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, resolver.Close()) }()
	volume := loadfile.Volume{Name: "VOL001", DeclaredRoot: "VOL001"}
	files := []packageLoadFile{{volume: volume, relPath: "source.txt"}}
	hashed, err := hashPackageFiles(t.Context(), resolver, []loadfile.Volume{volume}, nil, nil, files)
	require.NoError(t, err)
	require.Len(t, hashed, 1)
	assert.Equal(t, int64(len("synthetic content")), hashed[0].Size)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = hashPackageFiles(ctx, resolver, []loadfile.Volume{volume}, nil, nil, files)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, os.WriteFile(filepath.Join(root, "VOL001", "source.txt"), []byte("synthetic content with appended bytes"), 0o600))
	_, err = hashPackageFiles(t.Context(), resolver, []loadfile.Volume{volume}, nil, nil, files)
	require.ErrorIs(t, err, loadfile.ErrMalformedInput)
}

func TestPackageInventoryDoesNotWaitForVaultMaintenance(t *testing.T) {
	gate := NewOperationGate()
	require.NoError(t, gate.Maintain(func() error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := buildPackagePreflight(ctx, Deps{}, gate, "synthetic", PackagePreflightRequest{
			Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: t.TempDir(),
		})
		require.ErrorIs(t, err, loadfile.ErrMalformedInput)
		return nil
	}))
}
