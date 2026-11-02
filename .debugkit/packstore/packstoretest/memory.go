package packstoretest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"

	"go.kenn.io/kit/packstore"
)

// MemoryCatalog is a concurrency-safe in-memory packstore catalog for real
// lifecycle tests. It is not intended for production authority: applications
// should run RunCatalogContract against their durable catalog adapter.
type MemoryCatalog struct {
	mu         sync.Mutex
	members    map[packstore.Hash]packstore.Reference
	candidates map[packstore.Hash]packstore.Candidate
	entries    map[packstore.Hash]packstore.IndexEntry
	packs      map[string]packstore.PackRecord
}

// NewMemoryCatalog returns an empty catalog with complete reference inventory.
func NewMemoryCatalog() *MemoryCatalog {
	return &MemoryCatalog{
		members:    make(map[packstore.Hash]packstore.Reference),
		candidates: make(map[packstore.Hash]packstore.Candidate),
		entries:    make(map[packstore.Hash]packstore.IndexEntry),
		packs:      make(map[string]packstore.PackRecord),
	}
}

// Catalog implements CatalogHarness.
func (c *MemoryCatalog) Catalog() packstore.Catalog { return c }

// SetMember grants or removes logical membership for hash.
func (c *MemoryCatalog) SetMember(hash packstore.Hash, member bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !member {
		delete(c.members, hash)
		return
	}
	c.members[hash] = packstore.Reference{Hash: hash, OriginalHashes: []string{hash.String()}}
}

// SetCandidate records an application's loose candidate metadata.
func (c *MemoryCatalog) SetCandidate(candidate packstore.Candidate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.candidates[candidate.Hash] = candidate
}

// PutPack seeds a pack record and its index entries.
func (c *MemoryCatalog) PutPack(record packstore.PackRecord, entries []packstore.IndexEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.packs[record.PackID] = record
	for _, entry := range entries {
		c.entries[entry.Hash] = entry
	}
}

// Snapshot returns a detached view of catalog authority.
func (c *MemoryCatalog) Snapshot() CatalogState {
	c.mu.Lock()
	defer c.mu.Unlock()
	members := make(map[packstore.Hash]bool, len(c.members))
	for hash := range c.members {
		members[hash] = true
	}
	entries := make(map[packstore.Hash]packstore.IndexEntry, len(c.entries))
	packs := make(map[string]packstore.PackRecord, len(c.packs))
	maps.Copy(entries, c.entries)
	maps.Copy(packs, c.packs)
	return CatalogState{Members: members, Entries: entries, Packs: packs}
}

func (c *MemoryCatalog) Resolve(_ context.Context, hash packstore.Hash) (packstore.Location, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.members[hash]; !ok {
		return packstore.Location{}, nil
	}
	entry, packed := c.entries[hash]
	if !packed {
		return packstore.Location{Member: true}, nil
	}
	return packstore.Location{Member: true, Pack: &entry}, nil
}

func (c *MemoryCatalog) ListReferences(context.Context) (packstore.ReferenceInventory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	refs := make([]packstore.Reference, 0, len(c.members))
	for _, ref := range c.members {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Hash < refs[j].Hash })
	return packstore.ReferenceInventory{References: refs, Complete: true}, nil
}

func (c *MemoryCatalog) ListUnpacked(context.Context) ([]packstore.Candidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var candidates []packstore.Candidate
	for hash, candidate := range c.candidates {
		if _, member := c.members[hash]; !member {
			continue
		}
		if _, packed := c.entries[hash]; !packed {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Hash < candidates[j].Hash })
	return candidates, nil
}

func (c *MemoryCatalog) ListIndexed(context.Context) ([]packstore.IndexEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sortedEntries(c.entries, ""), nil
}

func (c *MemoryCatalog) ListPackRecords(context.Context) ([]packstore.PackRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	records := make([]packstore.PackRecord, 0, len(c.packs))
	for _, record := range c.packs {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].PackID < records[j].PackID })
	return records, nil
}

func (c *MemoryCatalog) ListPackEntries(_ context.Context, packID string) ([]packstore.IndexEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return sortedEntries(c.entries, packID), nil
}

func (c *MemoryCatalog) HasPackRecord(_ context.Context, packID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.packs[packID]
	return ok, nil
}

func (c *MemoryCatalog) PruneUnreferenced(context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var pruned int64
	for hash := range c.entries {
		if _, member := c.members[hash]; !member {
			delete(c.entries, hash)
			pruned++
		}
	}
	return pruned, nil
}

func (c *MemoryCatalog) RecordPack(_ context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.packs[record.PackID]; exists {
		return fmt.Errorf("packstoretest: pack %s already exists", record.PackID)
	}
	c.recordLocked(record, adoptions, false)
	return nil
}

func (c *MemoryCatalog) AdoptPack(_ context.Context, record packstore.PackRecord, adoptions []packstore.Adoption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordLocked(record, adoptions, true)
	return nil
}

func (c *MemoryCatalog) recordLocked(record packstore.PackRecord, adoptions []packstore.Adoption, replace bool) {
	c.packs[record.PackID] = record
	for _, adoption := range adoptions {
		if _, member := c.members[adoption.Entry.Hash]; !member {
			continue
		}
		if _, exists := c.entries[adoption.Entry.Hash]; !exists || replace {
			c.entries[adoption.Entry.Hash] = adoption.Entry
		}
	}
}

func (c *MemoryCatalog) DeletePackRecord(_ context.Context, packID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for hash, entry := range c.entries {
		if entry.PackID == packID {
			delete(c.entries, hash)
		}
	}
	delete(c.packs, packID)
	return nil
}

func (c *MemoryCatalog) DeleteIndexEntry(_ context.Context, hash packstore.Hash) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, hash)
	return nil
}

func (c *MemoryCatalog) ListPackUsage(context.Context) ([]packstore.PackUsage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	usage := make([]packstore.PackUsage, 0, len(c.packs))
	for _, record := range c.packs {
		item := packstore.PackUsage{PackRecord: record}
		for hash, entry := range c.entries {
			if entry.PackID != record.PackID {
				continue
			}
			if _, member := c.members[hash]; !member {
				continue
			}
			item.LiveEntries++
			item.LiveStoredBytes += entry.StoredLen
			item.LiveRawBytes += entry.RawLen
			item.MaxLiveStoredLen = max(item.MaxLiveStoredLen, entry.StoredLen)
			item.MaxLiveRawLen = max(item.MaxLiveRawLen, entry.RawLen)
		}
		usage = append(usage, item)
	}
	sort.Slice(usage, func(i, j int) bool { return usage[i].PackID < usage[j].PackID })
	return usage, nil
}

func (c *MemoryCatalog) ListLivePackEntries(_ context.Context, packID string) ([]packstore.IndexEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := make(map[packstore.Hash]packstore.IndexEntry)
	for hash, entry := range c.entries {
		if entry.PackID == packID {
			if _, member := c.members[hash]; member {
				entries[hash] = entry
			}
		}
	}
	return sortedEntries(entries, packID), nil
}

func (c *MemoryCatalog) CommitRepack(
	_ context.Context, sourceIDs []string, records []packstore.PackRecord, moves []packstore.RepackMove,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	sources := make(map[string]bool, len(sourceIDs))
	for _, packID := range sourceIDs {
		sources[packID] = true
	}
	expected := make(map[packstore.Hash]string)
	for hash, entry := range c.entries {
		if sources[entry.PackID] {
			if _, member := c.members[hash]; member {
				expected[hash] = entry.PackID
			}
		}
	}
	if len(expected) != len(moves) {
		return errors.New("packstoretest: repack set changed")
	}
	for _, move := range moves {
		if expected[move.NewEntry.Hash] != move.OldPackID {
			return errors.New("packstoretest: repack set changed")
		}
	}
	for _, record := range records {
		c.packs[record.PackID] = record
	}
	for _, move := range moves {
		c.entries[move.NewEntry.Hash] = move.NewEntry
	}
	return nil
}

func (c *MemoryCatalog) DeleteEmptyPackRecord(_ context.Context, packID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.PackID == packID {
			return false, nil
		}
	}
	if _, exists := c.packs[packID]; !exists {
		return false, nil
	}
	delete(c.packs, packID)
	return true, nil
}

func (c *MemoryCatalog) ClearPackMetadata(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	clear(c.packs)
	return nil
}

func sortedEntries(entries map[packstore.Hash]packstore.IndexEntry, packID string) []packstore.IndexEntry {
	result := make([]packstore.IndexEntry, 0, len(entries))
	for _, entry := range entries {
		if packID == "" || entry.PackID == packID {
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hash < result[j].Hash })
	return result
}

var (
	_ CatalogHarness    = (*MemoryCatalog)(nil)
	_ packstore.Catalog = (*MemoryCatalog)(nil)
)
