package processing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// DocumentEventCatalog is the minimal durable catalog used by the timeline
// derivation worker.
type DocumentEventCatalog interface {
	EnsureDocumentEventRecipe(ctx context.Context, fingerprint string) error
	MissingDocumentEventTargetsAfter(
		ctx context.Context, fingerprint, after string, limit int,
	) ([]store.DocumentEventTarget, error)
	LoadDocumentEventEvidence(
		ctx context.Context, target store.DocumentEventTarget,
	) (store.DocumentEventEvidenceSnapshot, error)
	PublishDocumentEvents(
		ctx context.Context, target store.DocumentEventTarget,
		fingerprint, inputsSHA256 string, canonical []byte,
	) (store.DocumentEventGeneration, error)
	RecordDocumentEventAttempt(
		ctx context.Context, target store.DocumentEventTarget,
		inputsSHA256, state string, diagnostics []byte,
	) error
	RefreshDocumentEventBuilds(ctx context.Context) error
}

// BackfillDocumentEventTargets derives and publishes exact retained versions.
// Deterministic evidence failures are recorded through the same captured
// digest fence as successful publication; operational failures remain
// retryable by Backfill.
func BackfillDocumentEventTargets(
	ctx context.Context,
	catalog DocumentEventCatalog,
	targets []store.DocumentEventTarget,
) (int, error) {
	completed := 0
	var targetErrors error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return completed, errors.Join(targetErrors, err)
		}
		if err := processDocumentEventTarget(ctx, catalog, target); err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf(
				"deriving document events for %s: %w", target.ContentVersionID, err,
			))
			continue
		}
		completed++
	}
	return completed, targetErrors
}

func processDocumentEventTarget(
	ctx context.Context,
	catalog DocumentEventCatalog,
	target store.DocumentEventTarget,
) error {
	snapshot, err := catalog.LoadDocumentEventEvidence(ctx, target)
	if err != nil {
		state, terminal := documentEventTerminalState(err, snapshot.InputsSHA256)
		if !terminal {
			return fmt.Errorf("loading evidence: %w", err)
		}
		return catalog.RecordDocumentEventAttempt(
			ctx, target, snapshot.InputsSHA256, state, []byte("[]"),
		)
	}

	record, _, err := DeriveDocumentEvents(snapshot)
	if err != nil {
		state := "failed"
		if errors.Is(err, store.ErrDocumentEventEvidenceUnavailable) {
			state = "unavailable"
		}
		return catalog.RecordDocumentEventAttempt(
			ctx, target, snapshot.InputsSHA256, state, []byte("[]"),
		)
	}
	canonical, _, err := document.MarshalDocumentEventsV1(record)
	if err != nil {
		return catalog.RecordDocumentEventAttempt(
			ctx, target, snapshot.InputsSHA256, "failed", []byte("[]"),
		)
	}
	_, err = catalog.PublishDocumentEvents(
		ctx, target, DocumentEventsDeriverFingerprint, snapshot.InputsSHA256, canonical,
	)
	return err
}

func documentEventTerminalState(err error, inputsSHA256 string) (string, bool) {
	if inputsSHA256 == "" {
		return "", false
	}
	if errors.Is(err, store.ErrSourceMetadataCorrupt) {
		return "failed", true
	}
	if errors.Is(err, store.ErrDocumentEventEvidenceUnavailable) {
		return "unavailable", true
	}
	return "", false
}

// NewDocumentEventBackfill creates the daemon's single restartable timeline
// derivation worker.
func NewDocumentEventBackfill(
	catalog DocumentEventCatalog,
	mutate func(context.Context, func() error) error,
	logger *slog.Logger,
) *Backfill[store.DocumentEventTarget] {
	underGate := func(ctx context.Context, fn func() error) error {
		if mutate == nil {
			return fn()
		}
		return mutate(ctx, fn)
	}
	backfill := &Backfill[store.DocumentEventTarget]{
		Name: "document-events", Page: 25, IdleDelay: time.Second,
		Mutate: mutate, Logger: logger,
		Key: func(target store.DocumentEventTarget) string {
			return target.ContentVersionID
		},
		Process: func(ctx context.Context, target store.DocumentEventTarget) error {
			_, err := BackfillDocumentEventTargets(
				ctx, catalog, []store.DocumentEventTarget{target},
			)
			return err
		},
	}
	// Progress counts retained versions; limit that scan during large rebuilds.
	var lastBuildRefresh time.Time
	backfill.List = func(ctx context.Context, after string, limit int) ([]store.DocumentEventTarget, error) {
		if err := underGate(ctx, func() error {
			return catalog.EnsureDocumentEventRecipe(ctx, DocumentEventsDeriverFingerprint)
		}); err != nil {
			return nil, err
		}
		targets, err := catalog.MissingDocumentEventTargetsAfter(
			ctx, DocumentEventsDeriverFingerprint, after, limit,
		)
		now := backfill.now()
		if err == nil && (len(targets) == 0 || now.Sub(lastBuildRefresh) >= time.Second) {
			err = underGate(ctx, func() error { return catalog.RefreshDocumentEventBuilds(ctx) })
			if err == nil {
				lastBuildRefresh = now
			}
		}
		return targets, err
	}
	return backfill
}

// RebuildDocumentEvents synchronously invalidates and drains every retained
// version. Restore callers only succeed when no target remains incomplete.
func RebuildDocumentEvents(ctx context.Context, catalog *store.Store) error {
	if _, err := catalog.BumpDocumentEventInputEpoch(
		ctx, DocumentEventsDeriverFingerprint,
	); err != nil {
		return err
	}
	backfill := NewDocumentEventBackfill(catalog, nil, nil)
	backfill.DrainOnce = true
	if err := backfill.Run(ctx); err != nil {
		return err
	}
	coverage, err := catalog.DocumentEventRebuildCoverage(ctx)
	if err != nil {
		return err
	}
	if documentEventRestoreIncomplete(coverage) {
		return errors.New("timeline rebuild is incomplete")
	}
	return nil
}

func documentEventRestoreIncomplete(coverage store.DocumentEventCoverage) bool {
	return coverage.Pending+coverage.Failed != 0
}
