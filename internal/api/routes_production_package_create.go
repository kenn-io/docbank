package api

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionPackageCreateRequest pins the idempotent package operation and
// bounded recipient projection before daemon-owned archive construction.
type ProductionPackageCreateRequest struct {
	OperationID        string `json:"operation_id"`
	ProfileID          string `json:"profile_id"`
	MaxVolumeBytes     int64  `json:"max_volume_bytes"`
	MaxVolumeDocuments int    `json:"max_volume_documents"`
}

// ProductionPackageEvidenceReceipt is the public verified package evidence
// returned without internal vault node or version details.
type ProductionPackageEvidenceReceipt production.PackageEvidenceReceipt

func productionPackageCreateError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	var problem *Error
	if errors.As(err, &problem) && problem.Status < 500 {
		return problem
	}
	if errors.Is(err, production.ErrRecipientArchive) || errors.Is(err, production.ErrPackageProjection) ||
		errors.Is(err, production.ErrPackageEvidence) {
		return NewError(http.StatusConflict, "production_package_conflict",
			"recipient package conflicts with the published production or retained evidence")
	}
	return NewError(http.StatusInternalServerError, "production_package_failed",
		"recipient package could not be verified")
}

func registerProductionPackageCreateRoutes(api huma.API, d Deps, g *OperationGate,
	downloads *webDownloadRegistry) {
	huma.Register(api, huma.Operation{
		OperationID: "createProductionPackage", Method: http.MethodPost,
		Path:          "/api/v1/productions/jobs/{job_id}/packages",
		Summary:       "Build and retain a recipient archive from verified published production artifacts",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 1 << 20,
	}, func(ctx context.Context, in *struct {
		JobID string `path:"job_id" format:"uuid"`
		Body  ProductionPackageCreateRequest
	}) (*struct {
		Body ProductionPackageEvidenceReceipt
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		if d.Store == nil || d.Blobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "production_unavailable",
				"production package storage is unavailable")
		}
		operationID, err := uuid.Parse(in.Body.OperationID)
		if err != nil || operationID.Version() != 4 || operationID.String() != in.Body.OperationID ||
			in.Body.ProfileID == "" || in.Body.MaxVolumeBytes < 1 || in.Body.MaxVolumeBytes > 50<<30 ||
			in.Body.MaxVolumeDocuments < 1 || in.Body.MaxVolumeDocuments > 100_000 {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_production_package",
				"package operation, profile or volume limits are invalid")
		}
		limits := production.PackageLimits{MaxVolumeBytes: in.Body.MaxVolumeBytes,
			MaxVolumeDocuments: in.Body.MaxVolumeDocuments}
		inputs, err := d.Store.LoadProductionPackageInputs(ctx, in.JobID)
		if err != nil {
			return nil, productionPackageCreateError(err)
		}
		projection, err := production.PlanPackageProjection(inputs.Job, inputs.Reservation,
			inputs.Members, in.Body.ProfileID, limits)
		if err != nil {
			return nil, productionPackageCreateError(err)
		}
		var selectedBytes int64
		for _, volume := range projection.Manifest.Volumes {
			if volume.Bytes < 1 || volume.Bytes > blob.MaxIngestBytes-selectedBytes {
				return nil, NewError(http.StatusRequestEntityTooLarge, "production_package_too_large",
					"selected production outputs exceed the supported retained object size")
			}
			selectedBytes += volume.Bytes
		}
		if err := downloads.ensureStagingDir(); err != nil {
			return nil, NewError(http.StatusInternalServerError, "production_package_failed",
				"private package staging is unavailable")
		}
		opener := processing.ProductionFinalArtifactAdapter{Catalog: d.Store, Blobs: d.Blobs}
		runtime := production.PackageRuntime{
			Catalog: d.Store, Opener: opener, StagingDir: downloads.dir,
			Retain: func(ctx context.Context, paths production.RecipientPackageRequest,
				_ production.PublishedRecipientPackage) error {
				stat, err := os.Stat(paths.ArchivePath)
				if err != nil {
					return err
				}
				if !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > blob.MaxIngestBytes {
					return NewError(http.StatusRequestEntityTooLarge, "production_package_too_large",
						"recipient archive exceeds the supported retained object size")
				}
				return g.mutate(func() error {
					return d.Blobs.WithMutation(ctx, func() error {
						_, err := d.Store.RetainProductionPackage(ctx, in.JobID, in.Body.OperationID,
							in.Body.ProfileID, limits, paths.ArchivePath, paths.QCPath,
							paths.TransmittalPath, productionPackageBlobWriter(d.Blobs))
						return err
					})
				})
			},
		}
		if _, err := runtime.Run(ctx, in.JobID, in.Body.ProfileID, limits); err != nil {
			return nil, productionPackageCreateError(err)
		}
		retained, err := d.Store.LoadRetainedProductionPackage(ctx, in.JobID, in.Body.OperationID)
		if err != nil {
			return nil, productionPackageCreateError(err)
		}
		return &struct {
			Body ProductionPackageEvidenceReceipt
		}{Body: ProductionPackageEvidenceReceipt(retained.Evidence)}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "getProductionPackage", Method: http.MethodGet,
		Path:    "/api/v1/productions/jobs/{job_id}/packages/{operation_id}",
		Summary: "Read the exact verified evidence for a retained recipient package",
	}, func(ctx context.Context, in *struct {
		JobID       string `path:"job_id" format:"uuid"`
		OperationID string `path:"operation_id" format:"uuid"`
	}) (*struct {
		Body ProductionPackageEvidenceReceipt
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		if d.Store == nil {
			return nil, NewError(http.StatusServiceUnavailable, "production_unavailable",
				"production package storage is unavailable")
		}
		retained, err := d.Store.LoadRetainedProductionPackage(ctx, in.JobID, in.OperationID)
		if err != nil {
			return nil, productionPackageCreateError(err)
		}
		return &struct {
			Body ProductionPackageEvidenceReceipt
		}{Body: ProductionPackageEvidenceReceipt(retained.Evidence)}, nil
	})
}
