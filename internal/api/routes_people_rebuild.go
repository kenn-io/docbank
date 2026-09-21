package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type PeopleRebuildRequest struct {
	OperationID string `json:"operation_id" format:"uuid" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
}

type PeopleBuild struct {
	OperationID         string `json:"operation_id" format:"uuid"`
	RequestSHA256       string `json:"request_sha256" pattern:"^[0-9a-f]{64}$"`
	ResolverFingerprint string `json:"resolver_fingerprint" pattern:"^[0-9a-f]{64}$"`
	State               string `json:"state" enum:"running,completed,failed"`
	TargetEpoch         int64  `json:"target_epoch"`
	Scanned             int64  `json:"scanned"`
	Published           int64  `json:"published"`
	Failed              int64  `json:"failed"`
	StartedAt           string `json:"started_at" format:"date-time"`
	UpdatedAt           string `json:"updated_at" format:"date-time"`
	FinishedAt          string `json:"finished_at,omitempty" format:"date-time"`
}

type PeopleCoverage struct {
	ContractVersion      string `json:"contract_version"`
	ResolverFingerprint  string `json:"resolver_fingerprint" pattern:"^[0-9a-f]{64}$"`
	BindingEpoch         int64  `json:"binding_epoch"`
	PublicationEpoch     int64  `json:"publication_epoch"`
	Published            int64  `json:"published"`
	Pending              int64  `json:"pending"`
	Failed               int64  `json:"failed"`
	Unavailable          int64  `json:"unavailable"`
	UnresolvedActors     int64  `json:"unresolved_actors"`
	SuppressedActors     int64  `json:"suppressed_actors"`
	UnresolvedCustodians int64  `json:"unresolved_custodians"`
	OpenCandidates       int64  `json:"open_candidates"`
	CandidateQueueFull   bool   `json:"candidate_queue_full"`
}

func registerPeopleRebuildRoutes(api huma.API, d Deps, g *gate) {
	type buildOutput struct{ Body PeopleBuild }
	huma.Register(api, huma.Operation{
		OperationID: "rebuildDocumentPeople", Method: http.MethodPost,
		Path: "/api/v1/people/rebuilds", Summary: "Start or replay person attribution rebuild",
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: 4 << 10,
	}, func(ctx context.Context, in *struct{ Body PeopleRebuildRequest }) (*buildOutput, error) {
		var build store.DocumentPeopleBuild
		err := g.MutateContext(ctx, func() error {
			var err error
			build, err = d.Store.RebuildDocumentPeople(ctx, in.Body.OperationID)
			return err
		})
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &buildOutput{Body: peopleBuildAPI(build)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getPeopleRebuild", Method: http.MethodGet,
		Path: "/api/v1/people/rebuilds/{operation_id}", Summary: "Read person attribution rebuild progress",
	}, func(ctx context.Context, in *struct {
		OperationID string `path:"operation_id" format:"uuid" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
	}) (*buildOutput, error) {
		build, err := d.Store.DocumentPeopleBuild(ctx, in.OperationID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &buildOutput{Body: peopleBuildAPI(build)}, nil
	})

	type coverageOutput struct{ Body PeopleCoverage }
	huma.Register(api, huma.Operation{
		OperationID: "getPeopleCoverage", Method: http.MethodGet,
		Path: "/api/v1/people/coverage", Summary: "Read current-file person attribution coverage",
	}, func(ctx context.Context, _ *struct{}) (*coverageOutput, error) {
		coverage, err := d.Store.DocumentPeopleCoverage(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &coverageOutput{Body: peopleCoverageAPI(coverage)}, nil
	})
}

func peopleBuildAPI(build store.DocumentPeopleBuild) PeopleBuild {
	return PeopleBuild{
		OperationID: build.OperationID, RequestSHA256: build.RequestSHA256,
		ResolverFingerprint: build.ResolverFingerprint, State: build.State,
		TargetEpoch: build.TargetEpoch, Scanned: build.Scanned, Published: build.Published,
		Failed: build.Failed, StartedAt: build.StartedAt, UpdatedAt: build.UpdatedAt,
		FinishedAt: build.FinishedAt,
	}
}

func peopleCoverageAPI(coverage store.DocumentPeopleCoverage) PeopleCoverage {
	return PeopleCoverage{
		ContractVersion: coverage.ContractVersion, ResolverFingerprint: coverage.ResolverFingerprint,
		BindingEpoch: coverage.BindingEpoch, PublicationEpoch: coverage.PublicationEpoch,
		Published: coverage.Published, Pending: coverage.Pending, Failed: coverage.Failed,
		Unavailable: coverage.Unavailable, UnresolvedActors: coverage.UnresolvedActors,
		SuppressedActors: coverage.SuppressedActors, UnresolvedCustodians: coverage.UnresolvedCustodians,
		OpenCandidates: coverage.OpenCandidates, CandidateQueueFull: coverage.CandidateQueueFull,
	}
}
