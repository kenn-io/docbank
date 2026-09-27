package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionReproductionCreateRequest pins a historical selection and its
// selected delivery policy before daemon-owned archive construction.
type ProductionReproductionCreateRequest struct {
	Request            documentproduction.ReproductionRequest `json:"request"`
	DeliveryPolicy     ProductionReproductionDeliveryPolicy   `json:"delivery_policy"`
	ProfileID          string                                 `json:"profile_id"`
	MaxVolumeBytes     int64                                  `json:"max_volume_bytes"`
	MaxVolumeDocuments int                                    `json:"max_volume_documents"`
}

// ProductionReproductionDeliveryPolicy is the caller-selected frozen policy
// for later delivery evidence; this route performs no external transfer.
type ProductionReproductionDeliveryPolicy production.PackageDeliveryPolicy

func productionReproductionError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	if errors.Is(err, production.ErrReproductionConflict) {
		return NewError(http.StatusConflict, "production_reproduction_conflict",
			"reproduction receipt disagrees with the original production or retained package")
	}
	if errors.Is(err, production.ErrRecipientArchive) || errors.Is(err, production.ErrPackageProjection) ||
		errors.Is(err, production.ErrPackageEvidence) {
		return NewError(http.StatusConflict, "production_reproduction_conflict",
			"reproduction package could not be built from the published original")
	}
	var problem *Error
	if errors.As(err, &problem) && problem.Status < 500 {
		return problem
	}
	return NewError(http.StatusInternalServerError, "production_reproduction_failed",
		"reproduction receipt could not be verified")
}

func productionReproductionBlobWriter(blobs *blob.Store) store.ProductionPackageBlobWriter {
	return func(ctx context.Context, reader io.Reader) (string, int64, store.BlobPhysical, error) {
		written, err := blobs.WriteDetailedContext(ctx, reader)
		if err != nil {
			return "", 0, store.BlobPhysical{}, err
		}
		encoding, err := written.EncodingName()
		if err != nil {
			return "", 0, store.BlobPhysical{}, err
		}
		return written.Hash, written.Size, store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			MD5: written.MD5, Created: written.Created,
		}, nil
	}
}

func registerProductionReproductionRoutes(api huma.API, d Deps, g *OperationGate,
	downloads *webDownloadRegistry) {
	huma.Register(api, huma.Operation{
		OperationID: "createProductionReproduction", Method: http.MethodPost,
		Path:          "/api/v1/productions/jobs/{job_id}/reproductions",
		Summary:       "Build and retain a new verified package using only published production artifacts",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 1 << 20,
	}, func(ctx context.Context, in *struct {
		JobID string `path:"job_id" format:"uuid"`
		Body  ProductionReproductionCreateRequest
	}) (*struct {
		Body documentproduction.ReproductionReceipt
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		if d.Store == nil || d.Blobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "production_unavailable",
				"production package storage is unavailable")
		}
		policy := production.PackageDeliveryPolicy(in.Body.DeliveryPolicy)
		policySHA256, policyErr := production.PackageDeliveryPolicySHA256(policy)
		_, _, requestErr := documentproduction.CanonicalReproductionRequest(in.Body.Request)
		if policyErr != nil || requestErr != nil || policySHA256 != in.Body.Request.DeliveryPolicySHA256 ||
			in.Body.ProfileID == "" || in.Body.MaxVolumeBytes < 1 || in.Body.MaxVolumeBytes > 50<<30 ||
			in.Body.MaxVolumeDocuments < 1 || in.Body.MaxVolumeDocuments > 100_000 {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_production_reproduction",
				"reproduction selection, delivery policy, profile or package limits are invalid")
		}
		limits := production.PackageLimits{MaxVolumeBytes: in.Body.MaxVolumeBytes,
			MaxVolumeDocuments: in.Body.MaxVolumeDocuments}
		inputs, err := d.Store.LoadProductionPackageInputs(ctx, in.JobID)
		if err != nil {
			return nil, productionReproductionError(err)
		}
		projection, err := production.PlanPackageProjection(inputs.Job, inputs.Reservation,
			inputs.Members, in.Body.ProfileID, limits)
		if err != nil {
			return nil, productionReproductionError(err)
		}
		var selectedBytes int64
		for _, volume := range projection.Manifest.Volumes {
			if volume.Bytes < 1 || volume.Bytes > blob.MaxIngestBytes-selectedBytes {
				return nil, NewError(http.StatusRequestEntityTooLarge, "production_reproduction_too_large",
					"selected production outputs exceed the supported retained object size")
			}
			selectedBytes += volume.Bytes
		}
		if err := downloads.ensureStagingDir(); err != nil {
			return nil, NewError(http.StatusInternalServerError, "production_reproduction_failed",
				"private reproduction staging is unavailable")
		}
		opener := processing.ProductionFinalArtifactAdapter{Catalog: d.Store, Blobs: d.Blobs}
		runtime := production.ReproductionRuntime{
			Catalog: d.Store, Opener: opener, StagingDir: downloads.dir,
			Retain: func(ctx context.Context, request documentproduction.ReproductionRequest,
				_ production.ReproductionSelection, paths production.RecipientPackageRequest,
				_ production.PublishedRecipientPackage) error {
				stat, err := os.Stat(paths.ArchivePath)
				if err != nil {
					return err
				}
				if !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > blob.MaxIngestBytes {
					return NewError(http.StatusRequestEntityTooLarge, "production_reproduction_too_large",
						"reproduction archive exceeds the supported retained object size")
				}
				return g.mutate(func() error {
					return d.Blobs.WithMutation(ctx, func() error {
						_, err := d.Store.RetainProductionReproduction(ctx, in.JobID, request, policy,
							paths, opener, productionReproductionBlobWriter(d.Blobs))
						return err
					})
				})
			},
		}
		_, err = runtime.Run(ctx, in.JobID, in.Body.Request, policy, in.Body.ProfileID, limits)
		if err != nil {
			return nil, productionReproductionError(err)
		}
		receipt, err := d.Store.LoadProductionReproduction(ctx, in.JobID, in.Body.Request.OperationID)
		if err != nil {
			return nil, productionReproductionError(err)
		}
		return &struct {
			Body documentproduction.ReproductionReceipt
		}{Body: receipt}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "getProductionReproduction", Method: http.MethodGet,
		Path:    "/api/v1/productions/jobs/{job_id}/reproductions/{operation_id}",
		Summary: "Read an exact verified reproduction receipt and original production link",
	}, func(ctx context.Context, in *struct {
		JobID       string `path:"job_id" format:"uuid"`
		OperationID string `path:"operation_id" format:"uuid"`
	}) (*struct {
		Body documentproduction.ReproductionReceipt
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		receipt, err := d.Store.LoadProductionReproduction(ctx, in.JobID, in.OperationID)
		if err != nil {
			return nil, productionReproductionError(err)
		}
		return &struct {
			Body documentproduction.ReproductionReceipt
		}{Body: receipt}, nil
	})
}
