package production

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// PublishPackageQCReceipt writes the actual-archive QC beside the archive,
// never inside it. A byte-identical retry is accepted; a changed receipt is
// refused. The archive is rehashed and reopened before publication.
func PublishPackageQCReceipt(archivePath, receiptPath string, receipt PackageQC) error {
	if archivePath == "" || receiptPath == "" ||
		VerifyRecipientArchiveWithQC(archivePath, receipt) != nil {
		return ErrRecipientArchive
	}
	data, err := packageJSON(receipt)
	if err != nil {
		return err
	}
	if existing, readErr := ReadPackageQCReceipt(receiptPath); readErr == nil {
		current, encodeErr := packageJSON(existing)
		if encodeErr == nil && bytes.Equal(current, data) {
			return nil
		}
		return ErrRecipientArchive
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return ErrRecipientArchive
	}
	staged, err := os.CreateTemp(filepath.Dir(receiptPath), ".production-qc-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if _, err := staged.Write(data); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	if err := os.Link(staged.Name(), receiptPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := ReadPackageQCReceipt(receiptPath)
			if readErr == nil {
				current, encodeErr := packageJSON(existing)
				if encodeErr == nil && bytes.Equal(current, data) {
					return nil
				}
			}
			return ErrRecipientArchive
		}
		return err
	}
	return nil
}

// ReadPackageQCReceipt accepts only the canonical typed receipt. It does not
// establish that the archive still matches; call VerifyRecipientArchiveWithQC
// at each handoff.
func ReadPackageQCReceipt(path string) (PackageQC, error) {
	file, err := os.Open(path)
	if err != nil {
		return PackageQC{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPackageMetadataBytes+1))
	if err != nil || len(data) > maxPackageMetadataBytes {
		return PackageQC{}, ErrRecipientArchive
	}
	var receipt PackageQC
	if err := json.Unmarshal(data, &receipt, json.RejectUnknownMembers(true)); err != nil ||
		receipt.Contract != PackageQCContractV1 || receipt.ArchiveSHA256 == "" ||
		receipt.ManifestSHA256 == "" || len(receipt.Entries) == 0 || len(receipt.PageNumbers) == 0 {
		return PackageQC{}, ErrRecipientArchive
	}
	canonical, err := packageJSON(receipt)
	if err != nil || !bytes.Equal(canonical, data) {
		return PackageQC{}, ErrRecipientArchive
	}
	return receipt, nil
}
