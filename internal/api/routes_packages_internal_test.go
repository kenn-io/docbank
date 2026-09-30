package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/loadfile"
)

func TestPackagePathReferenceUsesConfirmedLogicalVolumeRoots(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	budget := packageMemoryBudget{maximum: 10}
	require.NoError(t, budget.add(8))
	require.ErrorIs(t, budget.add(3), loadfile.ErrLoadfileLimit)
	assert.Equal(t, int64(8), budget.used)
}

func TestPackageInventoryDoesNotWaitForVaultMaintenance(t *testing.T) {
	t.Parallel()
	gate := NewOperationGate()
	require.NoError(t, gate.MaintainContext(t.Context(), func() error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := buildPackagePreflight(ctx, Deps{}, gate, "synthetic", PackagePreflightRequest{
			Profile: "dat-concordance-v1", Encoding: "utf-8", SourceKind: "root", SourceRef: t.TempDir(),
		})
		require.ErrorIs(t, err, loadfile.ErrMalformedInput)
		return nil
	}))
}

func TestPackageMappingReservesMemoryBeforeExpandingFields(t *testing.T) {
	t.Parallel()
	for _, multiValue := range []bool{false, true} {
		records := []loadfile.Record{{Fields: []loadfile.Field{{Column: "Children", Raw: strings.Repeat(";", loadfile.MaxFieldValueBytes)}}}}
		mapping := loadfile.Mapping{Columns: []loadfile.MappingColumn{{SourceOrdinal: new(0), Canonical: new("loadfile.family.children"), MultiValue: multiValue}}}
		budget := packageMemoryBudget{maximum: 256 << 10}
		require.NoError(t, budget.add(packageRecordsMemory(records)))
		_, err := loadfile.ApplyMapping(records, mapping, loadfile.Profile{}, budget.add)
		require.ErrorIs(t, err, loadfile.ErrLoadfileLimit)
		assert.Nil(t, records[0].Fields[0].Value.List)
		assert.Nil(t, records[0].Family.AttachmentDocIDs)
	}
	records := []loadfile.Record{{}}
	budget := packageMemoryBudget{maximum: 512}
	require.NoError(t, budget.add(packageRecordsMemory(records)))
	_, err := loadfile.ApplyMapping(records, loadfile.Mapping{DefaultCustodian: strings.Repeat("x", 1024)}, loadfile.Profile{}, budget.add)
	require.ErrorIs(t, err, loadfile.ErrLoadfileLimit)
	assert.Empty(t, records[0].Fields, "the custodian field must not be appended before reserving memory")
}

func TestPackageMappingSharesBudgetAcrossRecords(t *testing.T) {
	t.Parallel()
	records := []loadfile.Record{
		{Fields: []loadfile.Field{{Raw: strings.Repeat(";", 64)}}},
		{Fields: []loadfile.Field{{Raw: strings.Repeat(";", 64)}}},
	}
	mapping := loadfile.Mapping{Columns: []loadfile.MappingColumn{{SourceOrdinal: new(0), Canonical: new("loadfile.custodian"), MultiValue: true}}}
	budget := packageMemoryBudget{maximum: packageRecordsMemory(records) + 1500}
	require.NoError(t, budget.add(packageRecordsMemory(records)))
	_, err := loadfile.ApplyMapping(records, mapping, loadfile.Profile{}, budget.add)
	require.ErrorIs(t, err, loadfile.ErrLoadfileLimit)
	assert.Len(t, records[0].Fields[0].Value.List, 65)
	assert.Nil(t, records[1].Fields[0].Value.List)
}
