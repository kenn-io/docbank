package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type visualPreviewBlobs interface {
	renditionBlobWriter
	OpenSeekableContext(ctx context.Context, hash string) (io.ReadSeekCloser, int64, error)
	Remove(hash string) error
}

// EnsureVisualPreview retains one built-in recipe result; callers own the mutation gate.
func EnsureVisualPreview(ctx context.Context, catalog *store.Store, blobs *blob.Store, versionID string, recipe document.VisualPreviewRecipeV1) (store.VisualPreviewView, error) {
	return ensureVisualPreview(ctx, catalog, blobs, versionID, recipe)
}

func ensureVisualPreview(ctx context.Context, catalog *store.Store, blobs visualPreviewBlobs, versionID string, recipe document.VisualPreviewRecipeV1) (store.VisualPreviewView, error) {
	if err := ctx.Err(); err != nil {
		return store.VisualPreviewView{}, err
	}
	_, fingerprint, err := document.MarshalVisualPreviewRecipeV1(recipe)
	if err != nil {
		return store.VisualPreviewView{}, err
	}
	view, err := catalog.ContentVersionVisualPreviewByRecipe(ctx, versionID, fingerprint)
	if err == nil {
		return view, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.VisualPreviewView{}, err
	}
	version, err := catalog.ContentVersionByID(ctx, versionID)
	if err != nil {
		return store.VisualPreviewView{}, err
	}
	reader, size, err := blobs.OpenSeekableContext(ctx, version.BlobHash)
	if err != nil {
		return store.VisualPreviewView{}, sourceContentUnavailable(fmt.Errorf("opening visual preview source: %w", err))
	}
	if size != version.Size {
		return store.VisualPreviewView{}, sourceContentUnavailable(errors.Join(errors.New("visual preview source size differs from version"), reader.Close()))
	}
	target := VisualPreviewTarget{SourceSHA256: version.BlobHash, Size: version.Size, MediaType: version.MimeType}
	product, produceErr := ProduceVisualPreviewForRecipe(ctx, reader, target, recipe)
	closeErr := reader.Close()
	if closeErr != nil {
		closeErr = sourceContentUnavailable(closeErr)
	}
	if err := errors.Join(produceErr, closeErr); err != nil {
		return store.VisualPreviewView{}, err
	}
	canonical, _, err := document.MarshalVisualPreviewV1(product.Preview)
	if err != nil {
		return store.VisualPreviewView{}, err
	}
	publish := catalog.PublishVisualPreviewGeneration
	if recipe == CurrentVisualPreviewRecipe() {
		publish = catalog.PublishVisualPreview
	}
	if product.Preview.State != document.VisualPreviewReady {
		_, err = publish(ctx, versionID, canonical, nil)
	} else {
		err = blobs.WithMutation(ctx, func() (retErr error) {
			written, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(product.Output))
			if err != nil {
				return err
			}
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				recorded, authorityErr := catalog.HasBlob(cleanupCtx, written.Hash)
				if authorityErr != nil {
					if retErr != nil {
						retErr = errors.Join(retErr, authorityErr)
					}
					return
				}
				remove := !recorded
				if recorded {
					authority, err := catalog.PhysicalContent(cleanupCtx, written.Hash)
					if err != nil {
						if retErr != nil {
							retErr = errors.Join(retErr, err)
						}
						return
					}
					remove = authority.Kind == "packed"
				}
				if remove {
					cleanupErr := blobs.Remove(written.Hash)
					if retErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
						retErr = errors.Join(retErr, cleanupErr)
					}
				}
			}()
			if written.Hash != product.Preview.Output.BlobSHA256 || written.Size != product.Preview.Output.Size {
				return errors.New("visual preview output identity changed before publication")
			}
			encoding, err := written.EncodingName()
			if err != nil {
				return err
			}
			physical := store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created}
			_, err = publish(ctx, versionID, canonical, &physical)
			return err
		})
	}
	if err != nil {
		return store.VisualPreviewView{}, err
	}
	return catalog.ContentVersionVisualPreviewByRecipe(ctx, versionID, fingerprint)
}
