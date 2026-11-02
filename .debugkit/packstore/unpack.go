package packstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"go.kenn.io/kit/pack"
)

// UnpackStats summarizes loose materialization and physical retirement.
type UnpackStats struct {
	PacksUnpacked  int
	BlobsRestored  int
	BytesRestored  int64
	MappingsPruned int64
}

type unpackPlan struct {
	packID  string
	entries []IndexEntry
}

// Unpack durably materializes every live indexed blob before atomically
// clearing packed catalog authority. Every pack and footer is preflighted
// before the first loose write.
func (m *Maintainer) Unpack(ctx context.Context) (UnpackStats, error) {
	var stats UnpackStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	lease, err := m.coordinator.AcquireMaintenance(ctx)
	if err != nil {
		return stats, err
	}
	defer func() { _ = lease.Release() }()
	pruned, err := m.catalog.PruneUnreferenced(ctx)
	if err != nil {
		return stats, err
	}
	stats.MappingsPruned = pruned
	records, err := m.catalog.ListPackRecords(ctx)
	if err != nil {
		return stats, err
	}
	indexed, err := m.catalog.ListIndexed(ctx)
	if err != nil {
		return stats, err
	}
	byPack := make(map[string][]IndexEntry)
	for _, entry := range indexed {
		byPack[entry.PackID] = append(byPack[entry.PackID], entry)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].PackID < records[j].PackID })
	plans := make([]unpackPlan, 0, len(records))
	for _, record := range records {
		entries := byPack[record.PackID]
		if len(entries) == 0 {
			plans = append(plans, unpackPlan{packID: record.PackID})
			continue
		}
		reader, err := OpenMaintenancePack(m.layout.PackPath(record.PackID), m.limits)
		if errors.Is(err, fs.ErrNotExist) {
			return stats, fmt.Errorf("packstore: pack %s is missing with %d live mappings", record.PackID, len(entries))
		}
		if err != nil {
			return stats, err
		}
		plan := unpackPlan{packID: record.PackID, entries: entries}
		if err := validateUnpackPlan(reader, plan, m.limits); err != nil {
			return stats, errors.Join(err, reader.Close())
		}
		if err := reader.Close(); err != nil {
			return stats, err
		}
		plans = append(plans, plan)
	}
	if len(byPack) != countNonEmptyPlans(plans) {
		return stats, errors.New("packstore: indexed mappings reference a missing pack record")
	}

	loose, err := NewLooseStore(m.layout)
	if err != nil {
		return stats, err
	}
	for i := range plans {
		if len(plans[i].entries) == 0 {
			continue
		}
		reader, err := OpenMaintenancePack(m.layout.PackPath(plans[i].packID), m.limits)
		if err != nil {
			return stats, err
		}
		if err := validateUnpackPlan(reader, plans[i], m.limits); err != nil {
			return stats, errors.Join(err, reader.Close())
		}
		for _, entry := range plans[i].entries {
			if err := ctx.Err(); err != nil {
				return stats, errors.Join(err, reader.Close())
			}
			stream, err := reader.OpenBlob(ctx, entry.Hash)
			if err != nil {
				return stats, errors.Join(err, reader.Close())
			}
			_, writeErr := loose.Write(ctx, stream, WriteOptions{
				Durability: DurablePublication, Dedup: VerifyFullHash,
				ExpectedHash: entry.Hash, MaxBytes: entry.RawLen,
			})
			if err := errors.Join(writeErr, stream.Close()); err != nil {
				return stats, errors.Join(err, reader.Close())
			}
			stats.BlobsRestored++
			stats.BytesRestored += entry.RawLen
		}
		if err := reader.Close(); err != nil {
			return stats, err
		}
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if err := m.catalog.ClearPackMetadata(ctx); err != nil {
		return stats, err
	}
	var retireErr error
	for _, plan := range plans {
		if err := m.filesystem.Retire(ctx, ObjectRef{PackID: plan.packID}); err != nil {
			retireErr = errors.Join(retireErr, err)
			continue
		}
		stats.PacksUnpacked++
	}
	return stats, retireErr
}

func validateUnpackPlan(reader *MaintenancePackReader, plan unpackPlan, limits Limits) error {
	footer := make(map[Hash]pack.Entry)
	for _, entry := range reader.Entries() {
		hash, err := ParseHash(entry.ID.String())
		if err != nil {
			return err
		}
		if _, duplicate := footer[hash]; duplicate {
			return fmt.Errorf("%w: duplicate blob %s", pack.ErrCorrupt, hash)
		}
		footer[hash] = entry
	}
	for _, indexed := range plan.entries {
		authoritative, ok := footer[indexed.Hash]
		if !ok {
			return fmt.Errorf("%w: indexed blob %s absent from %s", pack.ErrCorrupt, indexed.Hash, plan.packID)
		}
		if !packIndexMatchesFooter(&indexed, authoritative) {
			return fmt.Errorf("%w: metadata mismatch for %s", pack.ErrCorrupt, indexed.Hash)
		}
		if authoritative.RawLen > uint64(limits.BlobBytes) {
			return newLimitError(LimitBlobRawBytes, authoritative.RawLen, uint64(limits.BlobBytes))
		}
		if authoritative.StoredLen > uint64(limits.BlobBytes) {
			return newLimitError(LimitBlobStoredBytes, authoritative.StoredLen, uint64(limits.BlobBytes))
		}
	}
	return nil
}

func countNonEmptyPlans(plans []unpackPlan) int {
	count := 0
	for _, plan := range plans {
		if len(plan.entries) != 0 {
			count++
		}
	}
	return count
}
