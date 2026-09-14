package api

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/loadfile"
)

func TestPackagePathReferenceUsesConfirmedLogicalVolumeRoots(t *testing.T) {
	root := t.TempDir()
	mapping := loadfile.Mapping{VolumeRoots: map[string]string{"VOL001": "DELIVERY/VOL001"}}
	volumes := logicalPackageVolumes([]loadfile.Volume{{Name: "DELIVERY", DeclaredRoot: "DELIVERY", Ordinal: 1}}, mapping)
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
