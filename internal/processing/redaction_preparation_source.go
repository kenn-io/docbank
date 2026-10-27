package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func (service *Service) capturePreparationSource(ctx context.Context, selection store.PageBinding) ([]byte, error) {
	selected := NativePDFSelection{NodeID: selection.NodeID, Source: selection.Source}
	view, err := service.loadNativePDFVersion(ctx, selected)
	if err != nil {
		return nil, err
	}
	if view.Node.Revision != selection.Revision {
		return nil, store.ErrStaleRevision
	}
	return service.readNativePDFSource(ctx, selected, view)
}

func (service *Service) loadNativePDFVersion(ctx context.Context, selection NativePDFSelection) (store.ContentVersionView, error) {
	view, err := service.catalog.ContentVersionViewByID(ctx, selection.NodeID, selection.Source.VersionID)
	if err != nil {
		return view, err
	}
	if view.Node.IsDir() || view.Node.TrashedAt != nil {
		return view, store.ErrNotFound
	}
	return view, nil
}

func (service *Service) readNativePDFSource(ctx context.Context, selection NativePDFSelection, view store.ContentVersionView) ([]byte, error) {
	if view.Version.BlobHash != selection.Source.SHA256 || view.Version.Size != selection.Source.Size {
		return nil, ErrRedactionPreparationInvalid
	}
	if view.Version.MimeType != "application/pdf" {
		return nil, ErrRedactionPreparationUnsupported
	}
	return service.readNativePDFBlob(ctx, selection.Source)
}

func (service *Service) readNativePDFBlob(ctx context.Context, source document.PageSource) ([]byte, error) {
	stream, size, err := service.blobs.OpenStreamContext(ctx, source.SHA256)
	if err != nil {
		return nil, preparationSourceError(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: stream}, source.Size+1))
	if readErr == nil && (size != source.Size || int64(len(data)) != size || !stream.Verified()) {
		readErr = ErrRedactionPreparationIntegrity
	}
	err = errors.Join(readErr, stream.Close(), ctx.Err())
	if err == nil {
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != source.SHA256 {
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
