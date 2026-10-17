package docbank

import (
	"context"
	"errors"

	internalprocessing "go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// RebuildDocumentEvents starts or replays a durable rebuild and synchronously
// drains the provider-free timeline worker for embedded callers.
func (v *Vault) RebuildDocumentEvents(
	ctx context.Context, operationID string,
) (DocumentEventBuild, error) {
	if err := v.begin(); err != nil {
		return DocumentEventBuild{}, err
	}
	defer v.lifecycle.RUnlock()
	gate := embeddedMutationGate{vault: v}
	var build store.DocumentEventBuild
	err := gate.MutateContext(ctx, func() error {
		var err error
		build, err = v.metadata.StartDocumentEventRebuild(
			ctx, operationID, store.DocumentEventRebuildRequestSHA256,
		)
		return err
	})
	if err != nil {
		return DocumentEventBuild{}, err
	}
	if build.State == "completed" {
		return fromStoreDocumentEventBuild(build), nil
	}
	if build.State != "running" {
		return fromStoreDocumentEventBuild(build), errors.New("timeline rebuild is incomplete")
	}

	backfill := internalprocessing.NewDocumentEventBackfill(v.metadata, gate.MutateContext, nil)
	backfill.DrainOnce = true
	if err := backfill.Run(ctx); err != nil {
		return fromStoreDocumentEventBuild(build), err
	}
	build, err = v.metadata.DocumentEventBuild(ctx, operationID)
	if err != nil {
		return DocumentEventBuild{}, err
	}
	coverage, err := v.metadata.DocumentEventRebuildCoverage(ctx)
	if err != nil {
		return fromStoreDocumentEventBuild(build), err
	}
	if build.State != "completed" || coverage.Pending+coverage.Failed+coverage.Unavailable != 0 {
		return fromStoreDocumentEventBuild(build), errors.New("timeline rebuild is incomplete")
	}
	return fromStoreDocumentEventBuild(build), nil
}

// DocumentEventCoverage reports timeline derivation for current file versions.
func (v *Vault) DocumentEventCoverage(ctx context.Context) (DocumentEventCoverage, error) {
	if err := v.begin(); err != nil {
		return DocumentEventCoverage{}, err
	}
	defer v.lifecycle.RUnlock()
	coverage, err := v.metadata.DocumentEventCoverageReport(ctx)
	if err != nil {
		return DocumentEventCoverage{}, err
	}
	return fromStoreDocumentEventCoverage(coverage), nil
}

func fromStoreDocumentEventBuild(build store.DocumentEventBuild) DocumentEventBuild {
	return DocumentEventBuild{
		OperationID: build.OperationID, State: build.State,
		DeriverFingerprint: build.DeriverFingerprint, TargetEpoch: build.TargetEpoch,
		Scanned: build.Scanned, Published: build.Published, Failed: build.Failed,
		Unavailable: build.Unavailable, StartedAt: build.StartedAt, UpdatedAt: build.UpdatedAt,
		FinishedAt: build.FinishedAt,
	}
}

func fromStoreDocumentEventCoverage(coverage store.DocumentEventCoverage) DocumentEventCoverage {
	return DocumentEventCoverage{
		Selected: coverage.Selected, Indexed: coverage.Indexed, Pending: coverage.Pending,
		Failed: coverage.Failed, Unavailable: coverage.Unavailable,
		MissingMetadata: coverage.MissingMetadata, InvalidDates: coverage.InvalidDates,
		UnboundProvenance:    coverage.UnboundProvenance,
		OperationalFallbacks: coverage.OperationalFallbacks,
		ContractVersion:      coverage.ContractVersion,
		DeriverFingerprint:   coverage.DeriverFingerprint, InputEpoch: coverage.InputEpoch,
		PublicationEpoch: coverage.PublicationEpoch,
	}
}
