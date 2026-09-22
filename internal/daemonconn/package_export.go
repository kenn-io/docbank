package daemonconn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/processing"
)

const maxPackageFileExportBytes int64 = 512 << 20

type packageExportStagingFile interface {
	io.ReadWriteSeeker
	io.ReaderAt
}

func (c *Connection) CreatePackageExport(ctx context.Context, request api.PackageExportRequest, destination io.Writer) (api.PackageExportTicket, error) {
	if destination == nil {
		return api.PackageExportTicket{}, errors.New("load-file export destination is required")
	}
	receipt, err := c.API().CreatePackageExport(ctx, &apiclient.CreatePackageExportRequestOptions{Body: &request})
	if err != nil {
		return api.PackageExportTicket{}, err
	}
	if receipt.URL == "" || receipt.Size < 1 || receipt.Records < 1 || !validSHA256Hex(receipt.ArchiveSHA256) ||
		!validSHA256Hex(receipt.ManifestSHA256) || !validSHA256Hex(receipt.CrosswalkSHA256) ||
		receipt.SnapshotID != request.SnapshotID || receipt.SourcePackageID != request.SourcePackageID ||
		receipt.ProfileID != request.ProfileID ||
		receipt.BatesAllocationID != request.BatesAllocationID {
		return api.PackageExportTicket{}, integrityErrorf("load-file export receipt is inconsistent")
	}
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+receipt.URL, nil)
	if err != nil {
		return api.PackageExportTicket{}, fmt.Errorf("building load-file export download: %w", err)
	}
	download.Header.Set("X-Api-Key", c.key)
	response, err := c.hc.Do(download)
	if err != nil {
		return api.PackageExportTicket{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return api.PackageExportTicket{}, integrityErrorf("load-file export download failed with HTTP %d", response.StatusCode)
	}
	digest := sha256.New()
	written, copyErr := io.CopyBuffer(io.MultiWriter(destination, digest), io.LimitReader(response.Body, receipt.Size+1), make([]byte, 256<<10))
	if copyErr != nil {
		return api.PackageExportTicket{}, fmt.Errorf("downloading load-file export: %w", copyErr)
	}
	if written != receipt.Size || hex.EncodeToString(digest.Sum(nil)) != receipt.ArchiveSHA256 {
		return api.PackageExportTicket{}, integrityErrorf("load-file export bytes disagree with verified receipt")
	}
	return *receipt, nil
}

// CreatePackageExportTo streams a package into a seekable staging file and
// independently reopens the ZIP before returning its bound receipt.
func (c *Connection) CreatePackageExportTo(
	ctx context.Context, request api.PackageExportRequest, destination packageExportStagingFile,
) (api.PackageExportTicket, error) {
	if destination == nil {
		return api.PackageExportTicket{}, errors.New("load-file export destination is required")
	}
	receipt, err := c.API().CreatePackageExport(ctx, &apiclient.CreatePackageExportRequestOptions{Body: &request})
	if err != nil {
		return api.PackageExportTicket{}, err
	}
	if receipt.URL == "" || receipt.Size < 1 || receipt.Size > maxPackageFileExportBytes || receipt.Records < 1 ||
		!validSHA256Hex(receipt.ArchiveSHA256) || !validSHA256Hex(receipt.ManifestSHA256) ||
		!validSHA256Hex(receipt.CrosswalkSHA256) || receipt.SnapshotID != request.SnapshotID ||
		receipt.SourcePackageID != request.SourcePackageID || receipt.ProfileID != request.ProfileID ||
		receipt.BatesAllocationID != request.BatesAllocationID {
		return api.PackageExportTicket{}, integrityErrorf("load-file export receipt is inconsistent")
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return api.PackageExportTicket{}, err
	}
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+receipt.URL, nil)
	if err != nil {
		return api.PackageExportTicket{}, fmt.Errorf("building load-file export download: %w", err)
	}
	download.Header.Set("X-Api-Key", c.key)
	response, err := c.hc.Do(download)
	if err != nil {
		return api.PackageExportTicket{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return api.PackageExportTicket{}, integrityErrorf("load-file export download failed with HTTP %d", response.StatusCode)
	}
	digest := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(destination, digest), io.LimitReader(response.Body, receipt.Size+1), make([]byte, 256<<10))
	if err != nil || written != receipt.Size || hex.EncodeToString(digest.Sum(nil)) != receipt.ArchiveSHA256 {
		return api.PackageExportTicket{}, integrityErrorf("load-file export bytes disagree with verified receipt")
	}
	verified, err := processing.VerifyLoadFileExport(ctx, destination, receipt.Size)
	if err != nil || verified.Receipt.SnapshotID != receipt.SnapshotID ||
		verified.Receipt.SourcePackageID != receipt.SourcePackageID || verified.Receipt.ProfileID != receipt.ProfileID ||
		verified.Receipt.BatesAllocationID != receipt.BatesAllocationID || verified.Receipt.ArchiveSHA256 != receipt.ArchiveSHA256 ||
		verified.Receipt.ManifestSHA256 != receipt.ManifestSHA256 || verified.Receipt.CrosswalkSHA256 != receipt.CrosswalkSHA256 ||
		verified.Receipt.Size != receipt.Size || verified.Receipt.RecordCount != receipt.Records ||
		verified.Receipt.PageCount != receipt.Pages {
		return api.PackageExportTicket{}, integrityErrorf("load-file export archive disagrees with verified receipt")
	}
	_, err = destination.Seek(0, io.SeekStart)
	return *receipt, err
}
