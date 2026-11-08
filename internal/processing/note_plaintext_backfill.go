package processing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.kenn.io/docbank/internal/store"
)

// NewNotePlaintextBackfill recovers eligible exact versions after interruptions.
// Live ordinary consent is checked on each listing and again during admission.
func (service *Service) NewNotePlaintextBackfill(logger *slog.Logger) (*Backfill[store.NoteTextTarget], error) {
	profile, ok := service.profiles[NotePlaintextProfileName]
	if !ok {
		return nil, ErrProfileNotConfigured
	}
	if err := validateNotePlaintextProfile(profile); err != nil {
		return nil, err
	}
	return &Backfill[store.NoteTextTarget]{
		Name: "note-local-text", Page: 64, IdleDelay: time.Second,
		WriteRevision: service.catalog.WriteRevision,
		List: func(ctx context.Context, after string, limit int) ([]store.NoteTextTarget, error) {
			authority, err := service.catalog.AuthorizeProviderOperation(ctx, service.renditionConsentRequest(profile))
			state, stateErr := processingConsentState(err)
			if stateErr != nil {
				return nil, stateErr
			}
			if state != "active" {
				return nil, nil
			}
			return service.catalog.NoteTextTargets(ctx, profile.record.Fingerprint, authority, after, limit)
		},
		Key: func(target store.NoteTextTarget) string { return target.ContentVersionID },
		Process: func(ctx context.Context, target store.NoteTextTarget) error {
			node, err := service.catalog.NodeByID(ctx, target.NodeID)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if node.CurrentVersionID != target.ContentVersionID {
				return nil
			}
			return service.AdmitImportedRendition(ctx, node, NotePlaintextProfileName)
		},
		Logger: logger,
	}, nil
}
