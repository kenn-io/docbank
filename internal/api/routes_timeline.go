package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

func registerTimelineRoutes(api huma.API, d Deps, g *gate) {
	type buildOutput struct{ Body TimelineBuild }
	huma.Register(api, huma.Operation{
		OperationID: "createTimelineRebuild", Method: http.MethodPost,
		Path: "/api/v1/timeline/rebuilds", Summary: "Start or replay a timeline rebuild",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: 4 << 10,
	}, func(ctx context.Context, in *struct {
		Body TimelineRebuildRequest
	}) (*buildOutput, error) {
		var build store.DocumentEventBuild
		err := g.MutateContext(ctx, func() error {
			var err error
			build, err = d.Store.StartDocumentEventRebuild(
				ctx, in.Body.OperationID, store.DocumentEventRebuildRequestSHA256,
			)
			return err
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &buildOutput{Body: timelineBuildAPI(build)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "readTimelineRebuild", Method: http.MethodGet,
		Path: "/api/v1/timeline/rebuilds/{operation_id}", Summary: "Read timeline rebuild progress",
	}, func(ctx context.Context, in *struct {
		OperationID string `path:"operation_id" format:"uuid" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
	}) (*buildOutput, error) {
		build, err := d.Store.DocumentEventBuild(ctx, in.OperationID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &buildOutput{Body: timelineBuildAPI(build)}, nil
	})

	type coverageOutput struct{ Body DocumentEventCoverage }
	huma.Register(api, huma.Operation{
		OperationID: "readTimelineCoverage", Method: http.MethodGet,
		Path: "/api/v1/timeline/coverage", Summary: "Read current-file timeline coverage",
	}, func(ctx context.Context, _ *struct{}) (*coverageOutput, error) {
		coverage, err := d.Store.DocumentEventCoverageReport(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &coverageOutput{Body: documentEventCoverageAPI(coverage)}, nil
	})
}

func timelineBuildAPI(build store.DocumentEventBuild) TimelineBuild {
	return TimelineBuild{
		OperationID: build.OperationID, State: build.State,
		DeriverFingerprint: build.DeriverFingerprint, TargetEpoch: build.TargetEpoch,
		Scanned: build.Scanned, Published: build.Published, Failed: build.Failed,
		Unavailable: build.Unavailable, StartedAt: build.StartedAt, UpdatedAt: build.UpdatedAt,
		FinishedAt: build.FinishedAt,
	}
}

func documentEventCoverageAPI(coverage store.DocumentEventCoverage) DocumentEventCoverage {
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
