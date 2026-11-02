package packstore

import (
	"errors"
	"sort"
	"sync"
)

// Health holds process-local physical observations used to prefer candidates
// that have not failed recently. Durable authority remains application-owned.
type Health struct {
	mu        sync.RWMutex
	locations map[locationHealthKey]struct{}
	stores    map[StoreID]struct{}
}

// NewHealth returns an empty process-local health observer.
func NewHealth() *Health {
	return &Health{
		locations: make(map[locationHealthKey]struct{}),
		stores:    make(map[StoreID]struct{}),
	}
}

// Order returns a detached candidate list in current read preference order.
func (h *Health) Order(hash Hash, candidates []ReadLocation) []ReadLocation {
	ordered := append([]ReadLocation(nil), candidates...)
	h.mu.RLock()
	defer h.mu.RUnlock()
	sort.SliceStable(ordered, func(i, j int) bool {
		return h.score(hash, ordered[i]) < h.score(hash, ordered[j])
	})
	return ordered
}

// Observe records a typed physical failure without changing durable authority.
func (h *Health) Observe(hash Hash, location ReadLocation, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case errors.Is(err, ErrPhysicalMissing), errors.Is(err, ErrPhysicalCorrupt):
		h.locations[healthKey(hash, location)] = struct{}{}
	case errors.Is(err, ErrStoreUnavailable), errors.Is(err, ErrStoreFenced):
		h.stores[location.StoreID] = struct{}{}
	}
}

// Clear removes process-local failure observations after verified success.
func (h *Health) Clear(hash Hash, location ReadLocation) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.locations, healthKey(hash, location))
	delete(h.stores, location.StoreID)
}

func (h *Health) score(hash Hash, location ReadLocation) int {
	score := 0
	if _, failed := h.stores[location.StoreID]; failed {
		score++
	}
	if _, failed := h.locations[healthKey(hash, location)]; failed {
		score += 2
	}
	return score
}

type locationHealthKey struct {
	store       StoreID
	generation  LocationGeneration
	kind        uint8
	encoding    LooseEncoding
	logicalSize int64
	storedSize  int64
	hash        Hash
	packID      string
	offset      int64
	storedLen   int64
	rawLen      int64
	flags       uint8
	crc32c      uint32
}

func healthKey(hash Hash, location ReadLocation) locationHealthKey {
	key := locationHealthKey{
		store:      location.StoreID,
		generation: location.Generation,
		hash:       hash,
	}
	if location.Loose != nil {
		key.kind = 1
		key.encoding = location.Loose.Encoding
		key.logicalSize = location.Loose.LogicalSize
		key.storedSize = location.Loose.StoredSize
		return key
	}
	key.kind = 2
	if location.Pack != nil {
		key.packID = location.Pack.PackID
		key.offset = location.Pack.Offset
		key.storedLen = location.Pack.StoredLen
		key.rawLen = location.Pack.RawLen
		key.flags = location.Pack.Flags
		key.crc32c = location.Pack.CRC32C
	}
	return key
}
