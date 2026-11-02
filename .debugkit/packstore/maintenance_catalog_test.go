package packstore

import (
	"context"
	"errors"
	"maps"
	"os"
	"sort"
	"sync"
)

type maintenanceCatalog struct {
	mu                 sync.Mutex
	members            map[Hash]Reference
	candidates         map[Hash]Candidate
	candidateOrder     []Hash
	entries            map[Hash]IndexEntry
	packs              map[string]PackRecord
	commitHook         func()
	recordErr          error
	adoptErr           error
	repackErr          error
	referencesComplete bool
}

func newMaintenanceCatalog() *maintenanceCatalog {
	return &maintenanceCatalog{
		members: make(map[Hash]Reference), candidates: make(map[Hash]Candidate),
		entries: make(map[Hash]IndexEntry), packs: make(map[string]PackRecord), referencesComplete: true,
	}
}

func (c *maintenanceCatalog) Resolve(_ context.Context, hash Hash) (Location, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, member := c.members[hash]; !member {
		return Location{}, nil
	}
	entry, packed := c.entries[hash]
	if !packed {
		return Location{Member: true}, nil
	}
	return Location{Member: true, Pack: &entry}, nil
}

func (c *maintenanceCatalog) ListReferences(context.Context) (ReferenceInventory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]Reference, 0, len(c.members))
	for _, ref := range c.members {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hash < result[j].Hash })
	return ReferenceInventory{References: result, Complete: c.referencesComplete}, nil
}

func (c *maintenanceCatalog) ListUnpacked(context.Context) ([]Candidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []Candidate
	if len(c.candidateOrder) != 0 {
		for _, hash := range c.candidateOrder {
			candidate, exists := c.candidates[hash]
			if !exists {
				continue
			}
			if _, member := c.members[hash]; member {
				if _, packed := c.entries[hash]; !packed {
					result = append(result, candidate)
				}
			}
		}
		return result, nil
	}
	for hash, candidate := range c.candidates {
		if _, member := c.members[hash]; member {
			if _, packed := c.entries[hash]; !packed {
				result = append(result, candidate)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hash < result[j].Hash })
	return result, nil
}

func (c *maintenanceCatalog) setCandidateOrder(hashes []Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.candidateOrder = append([]Hash(nil), hashes...)
}

func (c *maintenanceCatalog) ListIndexed(context.Context) ([]IndexEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]IndexEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		result = append(result, entry)
	}
	return result, nil
}

func (c *maintenanceCatalog) ListPackRecords(context.Context) ([]PackRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]PackRecord, 0, len(c.packs))
	for _, record := range c.packs {
		result = append(result, record)
	}
	return result, nil
}

func (c *maintenanceCatalog) ListPackEntries(_ context.Context, packID string) ([]IndexEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []IndexEntry
	for _, entry := range c.entries {
		if entry.PackID == packID {
			result = append(result, entry)
		}
	}
	return result, nil
}

func (c *maintenanceCatalog) HasPackRecord(_ context.Context, packID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.packs[packID]
	return exists, nil
}

func (c *maintenanceCatalog) PruneUnreferenced(context.Context) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var count int64
	for hash := range c.entries {
		if _, member := c.members[hash]; !member {
			delete(c.entries, hash)
			count++
		}
	}
	return count, nil
}

func (c *maintenanceCatalog) RecordPack(_ context.Context, record PackRecord, adoptions []Adoption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recordErr != nil {
		return c.recordErr
	}
	if _, exists := c.packs[record.PackID]; exists {
		return errors.New("pack exists")
	}
	c.recordLocked(record, adoptions, false)
	return nil
}

func (c *maintenanceCatalog) AdoptPack(_ context.Context, record PackRecord, adoptions []Adoption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.adoptErr != nil {
		return c.adoptErr
	}
	c.recordLocked(record, adoptions, true)
	return nil
}

func (c *maintenanceCatalog) recordLocked(record PackRecord, adoptions []Adoption, replace bool) {
	c.packs[record.PackID] = record
	for _, adoption := range adoptions {
		if _, member := c.members[adoption.Entry.Hash]; !member {
			continue
		}
		if _, exists := c.entries[adoption.Entry.Hash]; !exists || replace {
			c.entries[adoption.Entry.Hash] = adoption.Entry
		}
	}
	if c.commitHook != nil {
		c.commitHook()
	}
}

func (c *maintenanceCatalog) DeletePackRecord(_ context.Context, packID string) error {
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

func (c *maintenanceCatalog) DeleteIndexEntry(_ context.Context, hash Hash) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, hash)
	return nil
}

func (c *maintenanceCatalog) ListPackUsage(context.Context) ([]PackUsage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]PackUsage, 0, len(c.packs))
	for _, record := range c.packs {
		usage := PackUsage{PackRecord: record}
		for hash, entry := range c.entries {
			if entry.PackID == record.PackID {
				if _, member := c.members[hash]; member {
					usage.LiveEntries++
					usage.LiveStoredBytes += entry.StoredLen
					usage.LiveRawBytes += entry.RawLen
					usage.MaxLiveStoredLen = max(usage.MaxLiveStoredLen, entry.StoredLen)
					usage.MaxLiveRawLen = max(usage.MaxLiveRawLen, entry.RawLen)
				}
			}
		}
		result = append(result, usage)
	}
	return result, nil
}

func (c *maintenanceCatalog) ListLivePackEntries(_ context.Context, packID string) ([]IndexEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []IndexEntry
	for hash, entry := range c.entries {
		if entry.PackID == packID {
			if _, member := c.members[hash]; member {
				result = append(result, entry)
			}
		}
	}
	return result, nil
}

func (c *maintenanceCatalog) CommitRepack(_ context.Context, sourceIDs []string, records []PackRecord, moves []RepackMove) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repackErr != nil {
		return c.repackErr
	}
	expected := make(map[Hash]string)
	for _, sourceID := range sourceIDs {
		for hash, entry := range c.entries {
			if entry.PackID == sourceID {
				if _, member := c.members[hash]; member {
					expected[hash] = sourceID
				}
			}
		}
	}
	if len(expected) != len(moves) {
		return errors.New("repack set changed")
	}
	for _, move := range moves {
		if expected[move.NewEntry.Hash] != move.OldPackID {
			return errors.New("repack set changed")
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

func (c *maintenanceCatalog) DeleteEmptyPackRecord(_ context.Context, packID string) (bool, error) {
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

func (c *maintenanceCatalog) ClearPackMetadata(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	clear(c.packs)
	return nil
}

func (c *maintenanceCatalog) addLoose(hash Hash, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.members[hash] = Reference{Hash: hash, OriginalHashes: []string{hash.String()}}
	info, _ := osStat(path)
	c.candidates[hash] = Candidate{Hash: hash, OriginalHashes: []string{hash.String()}, Paths: []string{path}, Size: info}
}

var osStat = func(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (c *maintenanceCatalog) snapshot() (map[Hash]IndexEntry, map[string]PackRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := make(map[Hash]IndexEntry, len(c.entries))
	packs := make(map[string]PackRecord, len(c.packs))
	maps.Copy(entries, c.entries)
	maps.Copy(packs, c.packs)
	return entries, packs
}
