package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func (service *Service) capturePreparationSource(ctx context.Context, selection store.PageBinding) ([]byte, error) {
	view, err := service.catalog.ContentVersionViewByID(ctx, selection.NodeID, selection.Source.VersionID)
	if err != nil {
		return nil, err
	}
	if view.Node.IsDir() || view.Node.TrashedAt != nil {
		return nil, store.ErrNotFound
	}
	if view.Node.Revision != selection.Revision {
		return nil, store.ErrStaleRevision
	}
	if view.Version.BlobHash != selection.Source.SHA256 || view.Version.Size != selection.Source.Size {
		return nil, ErrRedactionPreparationInvalid
	}
	if view.Version.MimeType != "application/pdf" {
		return nil, ErrRedactionPreparationUnsupported
	}
	stream, size, err := service.blobs.OpenStreamContext(ctx, selection.Source.SHA256)
	if err != nil {
		return nil, preparationSourceError(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: stream}, selection.Source.Size+1))
	if readErr == nil && (size != selection.Source.Size || int64(len(data)) != size || !stream.Verified()) {
		readErr = ErrRedactionPreparationIntegrity
	}
	err = errors.Join(readErr, stream.Close(), ctx.Err())
	if err == nil {
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != selection.Source.SHA256 {
			err = ErrRedactionPreparationIntegrity
		}
	}
	if err != nil {
		clear(data)
		return nil, preparationSourceError(err)
	}
	return data, nil
}

func preparationSourceError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, packstore.ErrPhysicalMissing),
		errors.Is(err, packstore.ErrContentMismatch), errors.Is(err, packstore.ErrPhysicalCorrupt),
		errors.Is(err, pack.ErrChecksum), errors.Is(err, pack.ErrCorrupt),
		errors.Is(err, pack.ErrTruncated), errors.Is(err, pack.ErrBlobMismatch),
		errors.Is(err, pack.ErrVerificationIncomplete):
		return fmt.Errorf("%w: %w", ErrRedactionPreparationIntegrity, err)
	default:
		return err
	}
}
