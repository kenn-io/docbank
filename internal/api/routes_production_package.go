package api

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

type ProductionPackagePublishRequest struct {
	OperationID        string `json:"operation_id"`
	ProfileID          string `json:"profile_id"`
	MaxVolumeBytes     int64  `json:"max_volume_bytes"`
	MaxVolumeDocuments int    `json:"max_volume_documents"`
}

type ProductionPackagePublished struct {
	JobID          string `json:"job_id"`
	OperationID    string `json:"operation_id"`
	ProfileID      string `json:"profile_id"`
	VersionID      string `json:"version_id"`
	ArchiveSHA256  string `json:"archive_sha256"`
	EvidenceSHA256 string `json:"evidence_sha256"`
	Size           int64  `json:"size"`
}

// Valid checks the canonical operation identity, qualified profile, and
// bounded per-volume limits shared by HTTP, daemon, and embedded callers.
func (request ProductionPackagePublishRequest) Valid() bool {
	id, err := uuid.Parse(request.OperationID)
	if err != nil || id.Version() != 4 || id.String() != request.OperationID ||
		request.MaxVolumeBytes < 1 || request.MaxVolumeBytes > 50<<30 ||
		request.MaxVolumeDocuments < 1 || request.MaxVolumeDocuments > 100_000 {
		return false
	}
	switch request.ProfileID {
	case "export-dat-pdf-v1", "export-dat-opt-images-v1", "export-dat-lfp-images-v1":
		return true
	default:
		return false
	}
}

func productionPackagePublishProblem(err error) *Error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return NewError(http.StatusNotFound, "not_found", "production job not found")
	case errors.Is(err, production.ErrJobConflict), errors.Is(err, production.ErrPackageProjection),
		errors.Is(err, production.ErrPackageEvidence):
		return NewError(http.StatusConflict, "production_package_conflict", "production package conflicts with published authority")
	case errors.Is(err, context.Canceled):
		return NewError(http.StatusRequestTimeout, "production_package_canceled", "production package publication canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(http.StatusGatewayTimeout, "production_package_timeout", "production package publication timed out")
	default:
		return NewError(http.StatusInternalServerError, "production_package_failed", "production package could not be verified and retained")
	}
}

type ProductionPackageDownloadTicket struct {
	URL           string `json:"url"`
	ArchiveSHA256 string `json:"archive_sha256"`
	Size          int64  `json:"size"`
	VersionID     string `json:"version_id"`
}

func productionDownloadProblem(err error) *Error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return NewError(http.StatusNotFound, "not_found", "production package not found")
	case errors.Is(err, context.Canceled):
		return NewError(http.StatusRequestTimeout, "production_download_canceled", "production download canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(http.StatusGatewayTimeout, "production_download_timeout", "production download timed out")
	default:
		var problem *Error
		if errors.As(err, &problem) && problem.Status < 500 {
			return problem
		}
		return NewError(http.StatusInternalServerError, "production_download_failed",
			"production package could not be verified for download")
	}
}

func registerProductionPackageRoutes(api huma.API, d Deps, downloads *webDownloadRegistry,
	sessions *webSessionRegistry) {
	huma.Register(api, huma.Operation{
		OperationID: "publishProductionPackage", Method: http.MethodPost,
		Path:          "/api/v1/productions/jobs/{job_id}/packages",
		Summary:       "Publish one verified recipient package from a successful production job",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 4096,
	}, func(ctx context.Context, in *struct {
		JobID string `path:"job_id" format:"uuid"`
		Body  ProductionPackagePublishRequest
	}) (*struct{ Body ProductionPackagePublished }, error) {
		if _, err := exportOwner(ctx); err != nil {
			return nil, err
		}
		if !in.Body.Valid() {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_production_package",
				"production package request is invalid")
		}
		if d.Exports == nil {
			return nil, NewError(http.StatusServiceUnavailable, "production_unavailable",
				"production package publisher is unavailable")
		}
		retained, err := d.Exports.PublishProductionPackage(ctx, in.Body.OperationID, in.JobID,
			in.Body.ProfileID, production.PackageLimits{MaxVolumeBytes: in.Body.MaxVolumeBytes,
				MaxVolumeDocuments: in.Body.MaxVolumeDocuments})
		if err != nil {
			return nil, productionPackagePublishProblem(err)
		}
		return &struct{ Body ProductionPackagePublished }{Body: ProductionPackagePublished{
			JobID: in.JobID, OperationID: in.Body.OperationID, ProfileID: in.Body.ProfileID,
			VersionID: retained.Archive.Version.ID, ArchiveSHA256: retained.Archive.Version.BlobHash,
			EvidenceSHA256: retained.Evidence.SHA256, Size: retained.Archive.Version.Size,
		}}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "downloadProductionPackage", Method: http.MethodPost,
		Path:         "/api/v1/productions/jobs/{job_id}/packages/{operation_id}/download",
		Summary:      "Issue a one-use ticket for a verified retained production package",
		MaxBodyBytes: 1024,
	}, func(ctx context.Context, in *struct {
		JobID       string `path:"job_id" format:"uuid"`
		OperationID string `path:"operation_id" format:"uuid"`
		Body        struct{}
	}) (*struct {
		Body ProductionPackageDownloadTicket
	}, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if d.Store == nil || d.Blobs == nil {
			return nil, NewError(http.StatusServiceUnavailable, "production_unavailable",
				"production package storage is unavailable")
		}
		if err := downloads.ensureStagingDir(); err != nil {
			return nil, NewError(http.StatusInternalServerError, "production_download_failed",
				"private production download staging is unavailable")
		}
		staged, err := processing.PrepareRetainedProductionPackageDownload(ctx, d.Store, d.Blobs,
			downloads.dir, in.JobID, in.OperationID)
		if err != nil {
			return nil, productionDownloadProblem(err)
		}
		file, err := os.Open(staged.ArchivePath)
		if err != nil {
			_ = staged.Close()
			return nil, NewError(http.StatusInternalServerError, "production_download_failed",
				"verified production archive is unavailable")
		}
		release := func() { _ = file.Close(); _ = staged.Close() }
		archive := staged.Retained.Archive.Version
		ticket := webDownloadTicket{
			path: staged.ArchivePath, name: "production.zip", mediaType: "application/zip",
			versionID: archive.ID, blobHash: archive.BlobHash, size: archive.Size,
			owner: owner, archiveFile: file, releaseArchive: release,
		}
		var token string
		if browserSessionRequest(ctx) {
			active, issueErr := sessions.withActiveOwner(owner, func() error {
				var err error
				token, err = downloads.issue(ticket)
				return err
			})
			if !active && issueErr == nil {
				issueErr = NewError(http.StatusGone, "download_session_expired",
					"the browser session expired before download publication")
			}
			err = issueErr
		} else {
			token, err = downloads.issue(ticket)
		}
		if err != nil {
			release()
			return nil, productionDownloadProblem(err)
		}
		return &struct {
			Body ProductionPackageDownloadTicket
		}{Body: ProductionPackageDownloadTicket{
			URL:           webDownloadFilePath + "?ticket=" + token,
			ArchiveSHA256: archive.BlobHash, Size: archive.Size, VersionID: archive.ID,
		}}, nil
	})
}
