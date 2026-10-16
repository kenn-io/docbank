package docbank

import (
	"context"
	"errors"
	"io"
	"os"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

type PackageExportRequest = api.PackageExportRequest

type PackageExportReceipt struct {
	SnapshotID        string `json:"snapshot_id"`
	SourcePackageID   string `json:"source_package_id,omitempty"`
	ProfileID         string `json:"profile_id"`
	BatesAllocationID string `json:"bates_allocation_id,omitempty"`
	LoadFile          string `json:"load_file"`
	PageMap           string `json:"page_map,omitempty"`
	ProfileSHA256     string `json:"profile_sha256"`
	MappingSHA256     string `json:"mapping_sha256"`
	ManifestSHA256    string `json:"manifest_sha256"`
	CrosswalkSHA256   string `json:"crosswalk_sha256"`
	ArchiveSHA256     string `json:"archive_sha256"`
	Size              int64  `json:"size"`
	RecordCount       int    `json:"record_count"`
	PageCount         int    `json:"page_count"`
}

// ExportPackage writes a verified load-file ZIP to destination and returns
// the hashes and counts bound to those exact bytes. No bytes reach destination
// until the staged archive has passed the same verifier used by daemon exports.
func (v *Vault) ExportPackage(ctx context.Context, request PackageExportRequest, destination io.Writer) (_ PackageExportReceipt, retErr error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return PackageExportReceipt{}, ErrClosed
	}
	if destination == nil {
		return PackageExportReceipt{}, errors.New("package export destination is required")
	}
	staged, err := os.CreateTemp(v.vaultRoot, ".docbank-package-export-*")
	if err != nil {
		return PackageExportReceipt{}, err
	}
	stagedPath := staged.Name()
	defer func() {
		retErr = errors.Join(retErr, staged.Close(), os.Remove(stagedPath))
	}()
	built, err := processing.WriteLoadFileExport(ctx, v.metadata, v.blobs, processing.LoadFileExportRequest{
		SnapshotID: request.SnapshotID, SourcePackageID: request.SourcePackageID,
		ProfileID: request.ProfileID, BatesAllocationID: request.BatesAllocationID,
	}, staged)
	if err != nil {
		return PackageExportReceipt{}, err
	}
	if err := staged.Sync(); err != nil {
		return PackageExportReceipt{}, err
	}
	verified, err := processing.VerifyLoadFileExport(ctx, staged, built.Receipt.Size)
	if err != nil || verified.Receipt.ArchiveSHA256 != built.Receipt.ArchiveSHA256 {
		return PackageExportReceipt{}, errors.Join(err, errors.New("package export failed verification"))
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return PackageExportReceipt{}, err
	}
	written, err := io.Copy(destination, staged)
	if err != nil || written != verified.Receipt.Size {
		return PackageExportReceipt{}, errors.Join(err, io.ErrShortWrite)
	}
	return packageExportReceipt(verified.Receipt), nil
}

func packageExportReceipt(value processing.LoadFileExportReceipt) PackageExportReceipt {
	return PackageExportReceipt{SnapshotID: value.SnapshotID, SourcePackageID: value.SourcePackageID,
		ProfileID: value.ProfileID, BatesAllocationID: value.BatesAllocationID,
		LoadFile: value.LoadFile, PageMap: value.PageMap, ProfileSHA256: value.ProfileSHA256,
		MappingSHA256: value.MappingSHA256, ManifestSHA256: value.ManifestSHA256,
		CrosswalkSHA256: value.CrosswalkSHA256, ArchiveSHA256: value.ArchiveSHA256,
		Size: value.Size, RecordCount: value.RecordCount, PageCount: value.PageCount}
}
