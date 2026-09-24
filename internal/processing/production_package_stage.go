package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/safefileio"
)

type RetainedProductionPackageCatalog interface {
	LoadRetainedProductionPackage(ctx context.Context, jobID, operationID string) (store.RetainedProductionPackage, error)
}

// StagedRetainedProductionPackage owns one private copy of each retained
// version. Close removes the entire directory, including the archive.
type StagedRetainedProductionPackage struct {
	Retained        store.RetainedProductionPackage
	ArchivePath     string
	QCPath          string
	TransmittalPath string
	dir             string
}

func (p StagedRetainedProductionPackage) Close() error {
	if p.dir == "" {
		return nil
	}
	return os.RemoveAll(p.dir)
}

// StageRetainedProductionPackageBlobs copies only Store-authorized version
// hashes, verifies each complete blob stream and its exact hash/size, and
// leaves the handoff files in a private directory for final ZIP verification.
func StageRetainedProductionPackageBlobs(ctx context.Context, catalog RetainedProductionPackageCatalog,
	blobs verifiedBlobReader, stagingRoot, jobID, operationID string) (
	staged StagedRetainedProductionPackage, err error,
) {
	if ctx == nil || catalog == nil || blobs == nil || !filepath.IsAbs(stagingRoot) {
		return StagedRetainedProductionPackage{}, production.ErrPackageEvidence
	}
	retained, err := catalog.LoadRetainedProductionPackage(ctx, jobID, operationID)
	if err != nil {
		return StagedRetainedProductionPackage{}, err
	}
	if err := safefileio.EnsurePrivateDir(stagingRoot); err != nil {
		return StagedRetainedProductionPackage{}, fmt.Errorf("securing package staging: %w", err)
	}
	dir, err := os.MkdirTemp(stagingRoot, ".production-download-")
	if err != nil {
		return StagedRetainedProductionPackage{}, err
	}
	defer func() {
		if err != nil {
			removeErr := os.RemoveAll(dir)
			err = errors.Join(err, removeErr)
		}
	}()
	staged = StagedRetainedProductionPackage{
		Retained: retained, dir: dir,
		ArchivePath:     filepath.Join(dir, "recipient.zip"),
		QCPath:          filepath.Join(dir, "qc.json"),
		TransmittalPath: filepath.Join(dir, "transmittal.json"),
	}
	for _, part := range []struct {
		path    string
		version store.ContentVersion
	}{
		{staged.ArchivePath, retained.Archive.Version},
		{staged.QCPath, retained.QC.Version},
		{staged.TransmittalPath, retained.Transmittal.Version},
	} {
		if err = stageRetainedProductionPart(ctx, blobs, part.path, part.version); err != nil {
			return StagedRetainedProductionPackage{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return StagedRetainedProductionPackage{}, err
	}
	return staged, nil
}

func stageRetainedProductionPart(ctx context.Context, blobs verifiedBlobReader,
	path string, version store.ContentVersion) error {
	if !canonical.IsSHA256Hex(version.BlobHash) || version.Size < 1 {
		return production.ErrPackageEvidence
	}
	stream, size, err := blobs.OpenStreamContext(ctx, version.BlobHash)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Join(err, stream.Close())
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), packageStageContextReader{ctx: ctx, reader: stream})
	if copyErr == nil {
		copyErr = stream.Verify()
	}
	verified := stream.Verified()
	err = errors.Join(copyErr, stream.Close(), file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if !verified || size != version.Size || written != version.Size ||
		hex.EncodeToString(hasher.Sum(nil)) != version.BlobHash {
		return production.ErrPackageEvidence
	}
	return ctx.Err()
}

type packageStageContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r packageStageContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
