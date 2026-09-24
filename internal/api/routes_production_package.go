package api

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

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
