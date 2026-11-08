package notebook

import (
	"context"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// NewBackfill recovers pending note projections through the shared worker loop.
func NewBackfill(projector *Projector, mutate func(context.Context, func() error) error, logger *slog.Logger) *processing.Backfill[store.NoteIndexTarget] {
	return &processing.Backfill[store.NoteIndexTarget]{
		Name: "notes", Page: sourcePage, IdleDelay: time.Second,
		WriteRevision: projector.catalog.WriteRevision,
		List:          projector.catalog.NoteIndexTargets,
		Key:           func(target store.NoteIndexTarget) string { return target.Name },
		Process:       projector.Process,
		Mutate:        mutate,
		Logger:        logger,
	}
}
