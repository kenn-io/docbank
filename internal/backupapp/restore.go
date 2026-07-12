package backupapp

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/store"
)

// PackedRestoreTarget supplies docbank's storage policy and catalog adapter to
// Kit while it restores into an unpublished target database.
type PackedRestoreTarget struct {
	coordinator *packstore.Coordinator
	limits      packstore.Limits
}

var _ backup.PackedContentTarget = (*PackedRestoreTarget)(nil)

func newPackedRestoreTarget() *PackedRestoreTarget {
	return &PackedRestoreTarget{
		coordinator: packstore.NewCoordinator(),
		limits:      packstore.DefaultLimits(),
	}
}

func (t *PackedRestoreTarget) Limits() packstore.Limits { return t.limits }

func (t *PackedRestoreTarget) AcquireRestoreLease(ctx context.Context) (*packstore.Lease, error) {
	lease, err := t.coordinator.AcquireMutation(ctx)
	if err != nil {
		return nil, fmt.Errorf("backupapp: acquiring packed restore lease: %w", err)
	}
	return lease, nil
}

func (t *PackedRestoreTarget) OpenRestoreCatalog(
	_ context.Context, db *sql.DB,
) (packstore.RestoreCatalog, error) {
	return store.NewPackRestoreCatalog(db), nil
}
