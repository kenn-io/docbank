package docbank

import (
	"context"
	"errors"
	"io"
	"sync"

	"go.kenn.io/docbank/internal/store"
)

const (
	// DefaultWalkPageSize is the finite page size used when WalkOptions.PageSize is zero.
	DefaultWalkPageSize = 500
	// MaxWalkPageSize is the largest page one Walker may materialize.
	MaxWalkPageSize = store.MaxWalkPageSize
)

// WalkOptions controls one stable snapshot traversal.
type WalkOptions struct {
	PageSize       int
	IncludeTrashed bool
}

// WalkEntry is one node and its canonical path in the traversal snapshot.
type WalkEntry struct {
	Path string
	Node Node
}

// Walker returns bounded pages from one stable tree snapshot until closed.
type Walker struct {
	inner   *store.Walker
	release func()

	closeOnce sync.Once
	closeErr  error
}

// Walk begins a stable snapshot traversal rooted at rootPath.
func (v *Vault) Walk(ctx context.Context, rootPath string, opts WalkOptions) (*Walker, error) {
	if err := v.begin(); err != nil {
		return nil, err
	}
	pageSize := opts.PageSize
	if pageSize == 0 {
		pageSize = DefaultWalkPageSize
	}
	if pageSize < 1 || pageSize > MaxWalkPageSize {
		v.lifecycle.RUnlock()
		return nil, errors.New("docbank walk page size must be between 1 and 5000")
	}
	walker, err := v.metadata.BeginWalk(ctx, rootPath, pageSize, opts.IncludeTrashed)
	if err != nil {
		v.lifecycle.RUnlock()
		return nil, err
	}
	return &Walker{inner: walker, release: v.lifecycle.RUnlock}, nil
}

// Next returns the next bounded snapshot page. io.EOF follows the last page.
func (w *Walker) Next(ctx context.Context) ([]WalkEntry, error) {
	if w == nil || w.inner == nil {
		return nil, io.EOF
	}
	entries, err := w.inner.Next(ctx)
	if err != nil {
		return nil, err
	}
	page := make([]WalkEntry, 0, len(entries))
	for _, entry := range entries {
		page = append(page, WalkEntry{Path: entry.Path, Node: fromStoreNode(entry.Node)})
	}
	return page, nil
}

// Close releases the snapshot and vault lifecycle lease. It is idempotent.
func (w *Walker) Close() error {
	if w == nil {
		return nil
	}
	w.closeOnce.Do(func() {
		w.closeErr = w.inner.Close()
		w.release()
	})
	return w.closeErr
}
