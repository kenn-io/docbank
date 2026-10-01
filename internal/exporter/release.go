package exporter

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/document/bundle"
)

// Release retires a finished job without interrupting a ticket or download.
func (w *Worker) Release(ctx context.Context, owner, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gate.MutateContext(ctx, func() error {
		return w.retryCatalog(ctx, func() error {
			return w.catalog.ReleaseExportJob(ctx, owner, id, func() error {
				if w.leases[id] > 0 {
					return bundle.ErrRetained
				}
				err := os.Remove(filepath.Join(w.dir, id+".zip"))
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			})
		})
	})
}
