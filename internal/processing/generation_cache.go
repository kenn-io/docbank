package processing

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

const generationCacheIdle = 5 * time.Minute

// generationCache shares immutable decoded data, never query authority or a
// database lease. Loaders run synchronously in the initiating query: the owner
// drains searches before closing storage, with no detached SQL to join.
type generationCache struct {
	mu     sync.Mutex
	load   func(context.Context, string) (store.VectorIndexGenerationRecord, error)
	idle   time.Duration
	closed bool
	spaces map[string]*generationCacheSpace
}
type generationCacheSpace struct {
	head    string
	entry   *generationCacheEntry
	flights map[string]*generationCacheFlight
}
type generationCacheEntry struct {
	record     store.VectorIndexGenerationRecord // metadata only; Bytes is always nil
	generation *vectorindex.Generation
	timer      *time.Timer
}
type generationCacheFlight struct {
	done       chan struct{}
	record     store.VectorIndexGenerationRecord
	generation *vectorindex.Generation
	err        error
}

func newGenerationCache(load func(context.Context, string) (store.VectorIndexGenerationRecord, error), idle time.Duration) *generationCache {
	return &generationCache{load: load, idle: idle, spaces: make(map[string]*generationCacheSpace)}
}

// Build timestamps describe publication history, not content identity. An
// identical generation can be reclaimed and rebuilt with a new valid time;
// the store still validates that time on every metadata/full read.
func sameGenerationRecord(a, b store.VectorIndexGenerationRecord) bool {
	return a.ID == b.ID && a.VectorSpaceID == b.VectorSpaceID && a.SourceManifestChecksum == b.SourceManifestChecksum &&
		a.IndexManifestChecksum == b.IndexManifestChecksum && a.ByteSize == b.ByteSize && a.RowCount == b.RowCount
}

func (cache *generationCache) Get(ctx context.Context, record store.VectorIndexGenerationRecord) (*vectorindex.Generation, error) {
	observeHead := true
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cache.mu.Lock()
		if cache.closed {
			cache.mu.Unlock()
			// Stop clears retention immediately, but the owner keeps storage open
			// while admitted reads drain. Load synchronously without cache state.
			_, generation, err := cache.loadGeneration(ctx, record)
			return generation, err
		}
		space := cache.spaces[record.VectorSpaceID]
		if space == nil {
			space = &generationCacheSpace{head: record.ID, flights: make(map[string]*generationCacheFlight)}
			cache.spaces[record.VectorSpaceID] = space
		}
		if flight := space.flights[record.ID]; flight != nil {
			// Joining an existing flight inherits its observation order. Neither
			// this join nor a later cancellation retry may re-observe an older head.
			observeHead = false
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-flight.done:
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// A canceled leader does not poison another query's independent lease.
			if errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded) {
				continue
			}
			if flight.err != nil {
				return nil, flight.err
			}
			if !sameGenerationRecord(flight.record, record) {
				return nil, errors.New("loaded vector generation does not match leased metadata")
			}
			return flight.generation, nil
		}
		if observeHead && space.head != record.ID {
			if space.entry != nil {
				space.entry.timer.Stop()
				space.entry = nil
			}
			space.head = record.ID
		}
		if entry := space.entry; entry != nil && entry.record.ID == record.ID {
			if !sameGenerationRecord(entry.record, record) {
				cache.mu.Unlock()
				return nil, errors.New("cached vector generation does not match leased metadata")
			}
			cache.touch(record.VectorSpaceID, space, entry)
			generation := entry.generation
			cache.mu.Unlock()
			return generation, nil
		}
		flight := &generationCacheFlight{done: make(chan struct{})}
		space.flights[record.ID] = flight
		cache.mu.Unlock()

		loaded, generation, err := cache.loadGeneration(ctx, record)
		cache.mu.Lock()
		flight.record, flight.generation, flight.err = loaded, generation, err
		delete(space.flights, record.ID)
		// Observing another head or stopping invalidates publication by this load.
		if err == nil && !cache.closed && cache.spaces[record.VectorSpaceID] == space && space.head == record.ID {
			entry := &generationCacheEntry{record: loaded, generation: generation}
			space.entry = entry
			cache.touch(record.VectorSpaceID, space, entry)
		}
		if space.entry == nil && len(space.flights) == 0 && cache.spaces[record.VectorSpaceID] == space {
			delete(cache.spaces, record.VectorSpaceID)
		}
		close(flight.done)
		cache.mu.Unlock()
		return generation, err
	}
}

// loadGeneration is shared by ordinary misses and reads draining after Stop.
// It returns metadata without serialized bytes and never changes cache state.
func (cache *generationCache) loadGeneration(ctx context.Context, record store.VectorIndexGenerationRecord) (store.VectorIndexGenerationRecord, *vectorindex.Generation, error) {
	loaded, err := cache.load(ctx, record.ID)
	var generation *vectorindex.Generation
	if err == nil && !sameGenerationRecord(loaded, record) {
		err = errors.New("loaded vector generation does not match leased metadata")
	}
	if err == nil {
		generation, err = vectorindex.OpenGeneration(bytes.NewReader(loaded.Bytes), int64(len(loaded.Bytes)))
	}
	if err == nil {
		err = ctx.Err()
	}
	loaded.Bytes = nil
	return loaded, generation, err
}

// touch holds mu. Replacing the timer and checking its identity prevents a
// queued callback from expiring an entry that a newer query just touched.
func (cache *generationCache) touch(spaceID string, space *generationCacheSpace, entry *generationCacheEntry) {
	if entry.timer != nil {
		entry.timer.Stop()
	}
	var timer *time.Timer
	timer = time.AfterFunc(cache.idle, func() {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.spaces[spaceID] != space || space.entry != entry || entry.timer != timer {
			return
		}
		space.entry = nil
		if len(space.flights) == 0 {
			delete(cache.spaces, spaceID)
		}
	})
	entry.timer = timer
}

func (cache *generationCache) Close() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.closed = true
	for _, space := range cache.spaces {
		if space.entry != nil {
			space.entry.timer.Stop()
			space.entry = nil
		}
	}
	clear(cache.spaces)
}

func (service *Service) generationSource() *generationCache {
	service.generationCacheOnce.Do(func() {
		service.generations = newGenerationCache(service.catalog.LoadVectorIndexGeneration, generationCacheIdle)
	})
	return service.generations
}
