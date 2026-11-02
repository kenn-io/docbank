package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/kit/pack"
)

type cachedPack struct {
	reader  *pack.Reader
	entries map[pack.BlobID]pack.Entry
}

// packReaders owns descriptors for sequential copies. No stream may remain
// open when another blob is opened, so eviction cannot interrupt a copy.
type packReaders struct {
	repo    *Repo
	known   map[pack.BlobID]IndexEntry
	ext     string
	readers map[string]*cachedPack
	order   []string // least recently used first
}

func (p *packReaders) open(ctx context.Context, id pack.BlobID) (*BlobStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	indexed, exists := p.known[id]
	if !exists {
		return nil, fmt.Errorf("backup: blob %s not present in any index", id)
	}
	cached, err := p.reader(indexed.PackID)
	if err != nil {
		return nil, fmt.Errorf("backup: opening pack %s for blob %s: %w", indexed.PackID, id, err)
	}
	entry, exists := cached.entries[id]
	if !exists {
		return nil, fmt.Errorf("backup: blob %s not found in pack %s (index inconsistency)", id, indexed.PackID)
	}
	return openIndexedBlob(ctx, cached.reader, entry, indexed)
}

func (p *packReaders) reader(id string) (*cachedPack, error) {
	if cached, exists := p.readers[id]; exists {
		i := slices.Index(p.order, id)
		p.order = append(slices.Delete(p.order, i, i+1), id)
		return cached, nil
	}
	if len(p.readers) >= maxOpenPackReaders {
		oldest := p.order[0]
		if cached, exists := p.readers[oldest]; exists {
			if err := cached.reader.Close(); err != nil {
				return nil, fmt.Errorf("backup: closing cached pack %s: %w", oldest, err)
			}
		}
		delete(p.readers, oldest)
		p.order = p.order[1:]
	}
	reader, err := pack.OpenReader(p.repo.packPath(id, p.ext), nil)
	if err != nil {
		return nil, err
	}
	cached := &cachedPack{reader: reader, entries: make(map[pack.BlobID]pack.Entry)}
	for _, entry := range reader.Entries() {
		cached.entries[entry.ID] = entry
	}
	p.readers[id] = cached
	p.order = append(p.order, id)
	return cached, nil
}

func (p *packReaders) close() error {
	var err error
	for id, cached := range p.readers {
		if closeErr := cached.reader.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("backup: closing pack %s: %w", id, closeErr))
		}
	}
	p.readers = nil
	p.order = nil
	return err
}
