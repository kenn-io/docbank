package vectorindex

import (
	"bytes"
	"sync"
)

// GenerationCache retains one immutable opened generation. Replacing the entry
// bounds retained memory to the existing single-generation size limit.
type GenerationCache struct {
	mu         sync.Mutex
	id         string
	generation *Generation
}

// Get opens an immutable generation once. A failed load keeps the prior entry.
func (cache *GenerationCache) Get(id string, load func() ([]byte, error)) (*Generation, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.generation != nil && cache.id == id {
		return cache.generation, nil
	}
	encoded, err := load()
	if err != nil {
		return nil, err
	}
	generation, err := OpenGeneration(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		return nil, err
	}
	cache.id, cache.generation = id, generation
	return generation, nil
}

// Clear releases the cache's retained generation.
func (cache *GenerationCache) Clear() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.id, cache.generation = "", nil
}
