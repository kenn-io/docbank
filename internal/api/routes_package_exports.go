package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
)

func registerPackageExportRoute(api huma.API, d Deps, downloads *webDownloadRegistry, sessions *webSessionRegistry) {
	huma.Register(api, huma.Operation{OperationID: "createPackageExport", Method: "POST",
		Path: "/api/v1/packages/exports", Summary: "Build and verify a load-file export package",
		DefaultStatus: 201, MaxBodyBytes: 16 << 10}, func(ctx context.Context, in *struct {
		Body PackageExportRequest
	}) (*struct{ Body PackageExportTicket }, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		file, stagedPath, err := downloads.createStagingFile()
		if err != nil {
			return nil, FromStoreError(err)
		}
		keep := false
		defer func() {
			if !keep {
				_ = file.Close()
				_ = os.Remove(stagedPath)
			}
		}()
		built, err := processing.WriteLoadFileExport(ctx, d.Store, d.Blobs, processing.LoadFileExportRequest{
			SnapshotID: in.Body.SnapshotID, ProfileID: in.Body.ProfileID,
			SourcePackageID: in.Body.SourcePackageID, BatesAllocationID: in.Body.BatesAllocationID,
		}, file)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if err = file.Sync(); err != nil {
			return nil, FromStoreError(err)
		}
		verified, err := processing.VerifyLoadFileExport(ctx, file, built.Receipt.Size)
		if err != nil || !samePackageExportReceipt(verified.Receipt, built.Receipt) {
			return nil, FromStoreError(errors.Join(err, errors.New("load-file export failed independent verification")))
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, FromStoreError(err)
		}
		name := fmt.Sprintf("loadfile-%s.zip", in.Body.SnapshotID)
		ticket := webDownloadTicket{path: stagedPath, name: name, mediaType: "application/zip",
			blobHash: verified.Receipt.ArchiveSHA256, size: verified.Receipt.Size, owner: owner, archiveFile: file,
			releaseArchive: func() { _ = file.Close(); _ = os.Remove(stagedPath) }}
		var token string
		if browserSessionRequest(ctx) {
			active, issueErr := sessions.withActiveOwner(owner, func() error {
				var issueErr error
				token, issueErr = downloads.issue(ticket)
				return issueErr
			})
			if !active && issueErr == nil {
				issueErr = errors.New("browser session was revoked before package export publication")
			}
			err = issueErr
		} else {
			token, err = downloads.issue(ticket)
		}
		if err != nil {
			return nil, FromStoreError(err)
		}
		keep = true
		return &struct{ Body PackageExportTicket }{Body: PackageExportTicket{
			URL: webDownloadFilePath + "?ticket=" + token, Name: name,
			SnapshotID: verified.Receipt.SnapshotID, SourcePackageID: verified.Receipt.SourcePackageID,
			ProfileID:         verified.Receipt.ProfileID,
			BatesAllocationID: verified.Receipt.BatesAllocationID, ArchiveSHA256: verified.Receipt.ArchiveSHA256,
			ManifestSHA256: verified.Receipt.ManifestSHA256, CrosswalkSHA256: verified.Receipt.CrosswalkSHA256,
			Size: verified.Receipt.Size, Records: verified.Receipt.RecordCount, Pages: verified.Receipt.PageCount,
		}}, nil
	})
}

func samePackageExportReceipt(left, right processing.LoadFileExportReceipt) bool {
	return left.SnapshotID == right.SnapshotID && left.SourcePackageID == right.SourcePackageID &&
		left.ProfileID == right.ProfileID && left.BatesAllocationID == right.BatesAllocationID &&
		left.ArchiveSHA256 == right.ArchiveSHA256 && left.ManifestSHA256 == right.ManifestSHA256 &&
		left.CrosswalkSHA256 == right.CrosswalkSHA256 && left.Size == right.Size &&
		left.RecordCount == right.RecordCount && left.PageCount == right.PageCount
}
