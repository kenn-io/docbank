package processing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// NoteCatalog is the existing store boundary used by the original-byte
// projection worker. Deterministic bad content is retained as a terminal state.
type NoteCatalog interface {
	MissingNoteTargetsAfter(context.Context, string, string, int) ([]store.NoteTarget, error)
	PruneNoteParses(context.Context, int) (int64, error)
	PublishNoteParse(context.Context, store.NoteTarget, string, string, notemarkdown.Note) error
}

// NewNoteBackfill shares the daemon's bounded, cancellable derivation loop.
// WriteRevision is supplied by its owner to wake after imports, moves, trash,
// restore and metadata restore without another watcher or polling authority.
func NewNoteBackfill(catalog NoteCatalog, blobs *blob.Store,
	mutate func(context.Context, func() error) error, logger *slog.Logger,
) *Backfill[store.NoteTarget] {
	return &Backfill[store.NoteTarget]{
		Name: "notes", Page: 25, IdleDelay: time.Second, Mutate: mutate, Logger: logger,
		Key: func(target store.NoteTarget) string { return target.VersionID },
		List: func(ctx context.Context, after string, limit int) ([]store.NoteTarget, error) {
			cleanup := func() error { _, err := catalog.PruneNoteParses(ctx, 100); return err }
			var err error
			if mutate == nil {
				err = cleanup()
			} else {
				err = mutate(ctx, cleanup)
			}
			if err != nil {
				return nil, err
			}
			return catalog.MissingNoteTargetsAfter(ctx, notemarkdown.Recipe, after, limit)
		},
		Process: func(ctx context.Context, target store.NoteTarget) error {
			return processNoteTarget(ctx, catalog, blobs, target)
		},
	}
}

func processNoteTarget(ctx context.Context, catalog NoteCatalog, blobs *blob.Store, target store.NoteTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target.Size > store.MaxNoteBytes {
		return catalog.PublishNoteParse(ctx, target, notemarkdown.Recipe, "too_large", notemarkdown.Note{})
	}
	reader, size, err := blobs.OpenStreamContext(ctx, target.BlobHash)
	if err != nil {
		return err
	}
	// The extra byte detects an incorrect catalog size without allocating an
	// unbounded blob. Successful input must reach the verified reader's EOF.
	src, readErr := io.ReadAll(io.LimitReader(reader, store.MaxNoteBytes+1))
	verified := reader.Verified()
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if size != target.Size || int64(len(src)) != target.Size || !verified {
		return fmt.Errorf("note original does not match version %s", target.VersionID)
	}
	note, state, err := parseNoteOriginal(ctx, src)
	if err != nil {
		return err
	}
	return catalog.PublishNoteParse(ctx, target, notemarkdown.Recipe, state, note)
}

func parseNoteOriginal(ctx context.Context, src []byte) (notemarkdown.Note, string, error) {
	if err := ctx.Err(); err != nil {
		return notemarkdown.Note{}, "", err
	}
	if int64(len(src)) > store.MaxNoteBytes {
		return notemarkdown.Note{}, "too_large", nil
	}
	note, err := notemarkdown.Parse(ctx, src, notemarkdown.Limits{})
	if errors.Is(err, notemarkdown.ErrLimit) {
		return notemarkdown.Note{}, "limit", nil
	}
	// With fixed valid defaults, ErrInvalidInput identifies UTF-8/NUL input.
	if errors.Is(err, notemarkdown.ErrInvalidInput) {
		return notemarkdown.Note{}, "not_utf8", nil
	}
	if err != nil {
		return notemarkdown.Note{}, "", err
	}
	return note, "ok", nil
}

// RebuildNotes drains original-byte projections before a restored vault is
// published. Bad note content is a completed classification, not restore loss.
func RebuildNotes(ctx context.Context, catalog NoteCatalog, blobs *blob.Store) error {
	for {
		removed, err := catalog.PruneNoteParses(ctx, 100)
		if err != nil {
			return err
		}
		if removed == 0 {
			break
		}
	}
	worker := NewNoteBackfill(catalog, blobs, nil, nil)
	worker.DrainOnce = true
	return worker.Run(ctx)
}
