package processing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/emailmime"
	"go.kenn.io/docbank/internal/store"
)

type emailCatalog interface {
	renditionPublicationCatalog
	VaultID() string
	EmailMetadata(ctx context.Context, versionID string) (store.EmailMetadataView, error)
	EmailGenerationForSource(ctx context.Context, hash string, size int64, recipe string) (store.EmailGenerationRecord, error)
	PublishEmailGeneration(ctx context.Context, publication store.EmailPublication) (store.EmailMetadataView, error)
	PublishEmailBody(ctx context.Context, publication store.EmailBodyPublication) error
	RecordEmailBodyUnavailable(ctx context.Context, attachmentID string, recipe string, path *string, reason string) error
	RenditionBuild(ctx context.Context, buildID string) (store.RenditionBuildRecord, error)
	MigrateLegacyPlainText(ctx context.Context) (store.LegacyMigrationReport, error)
}
type emailBlobs interface {
	renditionBlobWriter
	verifiedBlobReader
}

// EmailDecoderFingerprint identifies the built-in decoder and all its fixed policies.
func EmailDecoderFingerprint() (string, error) {
	return document.EmailRecipeFingerprint(emailmime.Recipe())
}

// EmailBodyProfileFingerprint identifies the local selected-body publication profile.
func EmailBodyProfileFingerprint() (string, error) {
	profile, err := document.EmailBodyProfileV1(emailmime.Recipe())
	if err != nil {
		return "", err
	}
	_, fingerprints, err := document.CanonicalProfile(profile)
	return fingerprints.Profile, err
}

// EmailBodyRecipeFingerprint identifies the selected-body conversion policy.
func EmailBodyRecipeFingerprint(recipe document.EmailRecipeV1) (string, error) {
	return document.EmailBodyRecipeFingerprint(recipe)
}

// BackfillEmailTargets attempts each target, retaining retryable errors without
// preventing later versions from completing their inventory and body results.
func BackfillEmailTargets(ctx context.Context, catalog *store.Store, blobs *blob.Store, spoolParent string, targets []store.EmailTarget) (int, error) {
	return backfillEmailTargets(ctx, catalog, blobs, spoolParent, targets)
}
func backfillEmailTargets(ctx context.Context, catalog emailCatalog, blobs emailBlobs, spoolParent string, targets []store.EmailTarget) (int, error) {
	completed := 0
	var failures error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return completed, errors.Join(failures, err)
		}
		view, err := ensureEmailTarget(ctx, catalog, blobs, spoolParent, target)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("processing email version %s: %w", target.Version.ID, err))
			continue
		}
		if view.BodySearch.State == "available" || view.BodySearch.State == "unavailable" {
			completed++
		}
	}
	return completed, failures
}

// EnsureEmailTarget publishes a verified inventory and the chosen outer body.
// A failed body publication leaves its inventory readable and pending for retry.
func EnsureEmailTarget(ctx context.Context, catalog *store.Store, blobs *blob.Store, spoolParent string, target store.EmailTarget) (store.EmailMetadataView, error) {
	return ensureEmailTarget(ctx, catalog, blobs, spoolParent, target)
}
func ensureEmailTarget(ctx context.Context, catalog emailCatalog, blobs emailBlobs, spoolParent string, target store.EmailTarget) (store.EmailMetadataView, error) {
	if err := ctx.Err(); err != nil {
		return store.EmailMetadataView{}, err
	}
	// Resolve immutable source authority afresh; target discovery is only a hint.
	view, err := catalog.EmailMetadata(ctx, target.Version.ID)
	if err != nil && !errors.Is(err, store.ErrEmailPending) && !errors.Is(err, store.ErrEmailNotSupported) {
		return view, err
	}
	recipe, err := EmailDecoderFingerprint()
	if err != nil {
		return view, err
	}
	if view.Generation.RecipeFingerprint != recipe {
		view, err = publishEmailInventory(ctx, catalog, blobs, spoolParent, view.Version, recipe)
		if err != nil {
			return view, err
		}
	}
	if view.BodySearch.State != "pending" {
		if view.BodySearch.Reason != nil && *view.BodySearch.Reason == "inventory_unavailable" {
			bodyRecipe, err := EmailBodyRecipeFingerprint(view.Evidence.Recipe)
			if err != nil {
				return view, err
			}
			err = catalog.RecordEmailBodyUnavailable(ctx, view.Attachment.ID, bodyRecipe, nil, "inventory_unavailable")
			if err != nil {
				return view, err
			}
		}
		return view, nil
	}
	if err = publishEmailBody(ctx, catalog, blobs, view); err != nil {
		// Both the generic staging fence and final email transaction can observe a
		// concurrent purge. Read authoritative state, never parse error messages.
		current, readErr := catalog.EmailMetadata(ctx, view.Version.ID)
		if errors.Is(readErr, store.ErrEmailDerivativeSuppressed) {
			return current, readErr
		}
		if readErr == nil && current.Attachment.ID == view.Attachment.ID && current.BodySearch.State != "pending" {
			return current, nil
		}
		return view, errors.Join(err, readErr)
	}
	return catalog.EmailMetadata(ctx, view.Version.ID)
}
func publishEmailInventory(ctx context.Context, catalog emailCatalog, blobs emailBlobs, spoolParent string, version store.ContentVersion, recipe string) (_ store.EmailMetadataView, retErr error) {
	reusable, err := catalog.EmailGenerationForSource(ctx, version.BlobHash, version.Size, recipe)
	if err == nil {
		return catalog.PublishEmailGeneration(ctx, store.EmailPublication{ContentVersionID: version.ID, CanonicalJSON: reusable.CanonicalJSON, Artifacts: reusable.Artifacts})
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.EmailMetadataView{}, err
	}
	var decoded *emailmime.Result
	if version.Size > emailmime.Recipe().Limits.SourceBytes {
		// Catalog-only refusal must not open an over-limit original.
		decoded, err = emailmime.Decode(ctx, version.BlobHash, version.Size, strings.NewReader(""), spoolParent)
	} else {
		stream, size, openErr := blobs.OpenStreamContext(ctx, version.BlobHash)
		if openErr != nil {
			return store.EmailMetadataView{}, openErr
		}
		if size != version.Size {
			return store.EmailMetadataView{}, errors.Join(errors.New("email original stream size differs from retained version"), stream.Close())
		}
		decoded, err = emailmime.Decode(ctx, version.BlobHash, version.Size, stream, spoolParent)
		err = errors.Join(err, stream.Close())
	}
	if decoded != nil {
		defer func() { retErr = errors.Join(retErr, decoded.Close()) }()
	}
	if err != nil {
		return store.EmailMetadataView{}, err
	}
	canonical, _, err := document.MarshalEmailV1(decoded.Evidence)
	if err != nil {
		return store.EmailMetadataView{}, err
	}
	publication := store.EmailPublication{ContentVersionID: version.ID, CanonicalJSON: canonical, Artifacts: []store.EmailPartArtifactRecord{}}
	var view store.EmailMetadataView
	err = blobs.WithMutation(ctx, func() error {
		// Stream one listed occurrence at a time. CAS deduplicates equal hashes;
		// each exact occurrence and role remains in the final reference set.
		for _, artifact := range decoded.Artifacts() {
			if err := ctx.Err(); err != nil {
				return err
			}
			reader, err := decoded.OpenArtifact(ctx, artifact.PartPath, string(artifact.Reference.Role))
			if err != nil {
				return err
			}
			receipt, writeErr := blobs.WriteDetailedContext(ctx, io.LimitReader(reader, artifact.Reference.Size+1))
			closeErr := reader.Close()
			var recordErr error
			if receipt.Hash != "" {
				encoding, err := receipt.EncodingName()
				if err != nil {
					recordErr = err
				} else {
					recordErr = catalog.RecordRenditionBlob(ctx,
						receipt.Hash,
						receipt.Size,
						store.BlobPhysical{Encoding: encoding,
							StoredBytes:  receipt.StoredSize,
							PackEligible: receipt.PackEligible,
							Created:      receipt.Created})
				}
			}
			if err := errors.Join(writeErr, closeErr, recordErr); err != nil {
				return err
			}
			if receipt.Hash != artifact.Reference.SHA256 || receipt.Size != artifact.Reference.Size {
				return errors.New("email artifact receipt differs from canonical reference")
			}
			publication.Artifacts = append(publication.Artifacts,
				store.EmailPartArtifactRecord{PartPath: artifact.PartPath,
					Role:       string(artifact.Reference.Role),
					BlobSHA256: receipt.Hash,
					Size:       receipt.Size})
		}
		var err error
		view, err = catalog.PublishEmailGeneration(ctx, publication)
		return err
	})
	return view, err
}
