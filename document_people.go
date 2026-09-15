package docbank

import (
	"context"
	"errors"

	internalprocessing "go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

// RebuildDocumentPeople starts or replays a durable rebuild and synchronously
// drains the provider-free attribution worker for embedded callers.
func (v *Vault) RebuildDocumentPeople(ctx context.Context, operationID string) (DocumentPeopleBuild, error) {
	if err := v.begin(); err != nil {
		return DocumentPeopleBuild{}, err
	}
	defer v.lifecycle.RUnlock()
	gate := embeddedMutationGate{vault: v}
	var build store.DocumentPeopleBuild
	err := gate.MutateContext(ctx, func() error {
		var err error
		build, err = v.metadata.RebuildDocumentPeople(ctx, operationID)
		return err
	})
	if err != nil {
		return DocumentPeopleBuild{}, err
	}
	if build.State == "completed" {
		return fromStoreDocumentPeopleBuild(build), nil
	}
	if build.State != "running" {
		return fromStoreDocumentPeopleBuild(build), errors.New("document people rebuild is incomplete")
	}
	backfill := internalprocessing.NewDocumentPeopleBackfill(v.metadata, gate.MutateContext, nil)
	backfill.DrainOnce = true
	if err := backfill.Run(ctx); err != nil {
		return fromStoreDocumentPeopleBuild(build), err
	}
	build, err = v.metadata.DocumentPeopleBuild(ctx, operationID)
	if err != nil {
		return DocumentPeopleBuild{}, err
	}
	if build.State != "completed" {
		return fromStoreDocumentPeopleBuild(build), errors.New("document people rebuild is incomplete")
	}
	return fromStoreDocumentPeopleBuild(build), nil
}

// DocumentPeopleCoverage reports attribution derivation for current files.
func (v *Vault) DocumentPeopleCoverage(ctx context.Context) (DocumentPeopleCoverage, error) {
	if err := v.begin(); err != nil {
		return DocumentPeopleCoverage{}, err
	}
	defer v.lifecycle.RUnlock()
	coverage, err := v.metadata.DocumentPeopleCoverage(ctx)
	if err != nil {
		return DocumentPeopleCoverage{}, err
	}
	return fromStoreDocumentPeopleCoverage(coverage), nil
}

func fromStoreDocumentPeopleBuild(build store.DocumentPeopleBuild) DocumentPeopleBuild {
	return DocumentPeopleBuild{
		OperationID: build.OperationID, RequestSHA256: build.RequestSHA256,
		ResolverFingerprint: build.ResolverFingerprint, State: build.State,
		TargetEpoch: build.TargetEpoch, Scanned: build.Scanned, Published: build.Published,
		Failed: build.Failed, StartedAt: build.StartedAt, UpdatedAt: build.UpdatedAt,
		FinishedAt: build.FinishedAt,
	}
}

func fromStoreDocumentPeopleCoverage(coverage store.DocumentPeopleCoverage) DocumentPeopleCoverage {
	return DocumentPeopleCoverage{
		ContractVersion: coverage.ContractVersion, ResolverFingerprint: coverage.ResolverFingerprint,
		BindingEpoch: coverage.BindingEpoch, PublicationEpoch: coverage.PublicationEpoch,
		Published: coverage.Published, Pending: coverage.Pending, Failed: coverage.Failed,
		Unavailable: coverage.Unavailable, UnresolvedActors: coverage.UnresolvedActors,
		SuppressedActors: coverage.SuppressedActors, UnresolvedCustodians: coverage.UnresolvedCustodians,
		OpenCandidates: coverage.OpenCandidates, CandidateQueueFull: coverage.CandidateQueueFull,
	}
}
