package backupapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const RestoreStoreMapVersion = 1

// RestoreStoreMap maps portable source placement identities to daemon-loaded
// target bindings. Binding definitions and secrets remain outside the file.
type RestoreStoreMap struct {
	Version int                   `toml:"version"`
	Stores  []RestoreStoreMapping `toml:"stores"`
}

type RestoreStoreMapping struct {
	SourceID               string `toml:"source_id"`
	Name                   string `toml:"name"`
	Binding                string `toml:"binding"`
	Takeover               bool   `toml:"takeover"`
	RemoteOnly             bool   `toml:"remote_only"`
	AllowAuditedRemoteOnly bool   `toml:"allow_audited_remote_only"`
}

// RestorePlacementOptions enables explicit placement reconstruction after Kit
// has independently restored every byte to the target's fresh local primary.
type RestorePlacementOptions struct {
	Map      *RestoreStoreMap
	Bindings map[string]config.StoreBindingConfig
}

func validateRestoreStoreMap(
	mapping RestoreStoreMap, manifest placementManifest,
) error {
	if mapping.Version != RestoreStoreMapVersion {
		return fmt.Errorf("backupapp: restore store-map version must be %d", RestoreStoreMapVersion)
	}
	sourceStores := make(map[string]placementStore, len(manifest.Stores))
	for _, source := range manifest.Stores {
		sourceStores[source.ID] = source
	}
	seenSource := make(map[string]struct{}, len(mapping.Stores))
	seenName := make(map[string]struct{}, len(mapping.Stores))
	for index, mapped := range mapping.Stores {
		if _, ok := sourceStores[mapped.SourceID]; !ok {
			return fmt.Errorf(
				"backupapp: store-map entry %d names unknown source store %q",
				index, mapped.SourceID,
			)
		}
		if mapped.Name == "" || mapped.Binding == "" {
			return fmt.Errorf("backupapp: store-map entry %d requires name and binding", index)
		}
		if _, duplicate := seenSource[mapped.SourceID]; duplicate {
			return fmt.Errorf("backupapp: source store %s is mapped more than once", mapped.SourceID)
		}
		if _, duplicate := seenName[mapped.Name]; duplicate {
			return fmt.Errorf("backupapp: target store name %q is repeated", mapped.Name)
		}
		if mapped.AllowAuditedRemoteOnly && !mapped.RemoteOnly {
			return fmt.Errorf(
				"backupapp: store-map entry %d acknowledges audited remote-only without selecting remote-only",
				index,
			)
		}
		seenSource[mapped.SourceID] = struct{}{}
		seenName[mapped.Name] = struct{}{}
	}
	return nil
}

func applyRestorePlacement(
	ctx context.Context,
	target string,
	databasePath string,
	driver docsqlite.Driver,
	manifest placementManifest,
	options RestorePlacementOptions,
) (retErr error) {
	if options.Map == nil {
		return nil
	}
	if err := validateRestoreStoreMap(*options.Map, manifest); err != nil {
		return err
	}
	metadata, err := store.Open(databasePath, driver)
	if err != nil {
		return fmt.Errorf("backupapp: opening restored placement catalog: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, metadata.Close())
	}()
	primary, err := metadata.PrimaryBlobStore(ctx)
	if err != nil {
		return err
	}
	physical, err := blob.New(
		store.NewPackCatalog(metadata), filepath.Join(target, "blobs"),
	)
	if err != nil {
		return fmt.Errorf("backupapp: opening restored primary: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, physical.Close())
	}()
	sourceBackend, ok := physical.ReadBackend(packstore.StoreID(primary.ID))
	if !ok {
		return errors.New("backupapp: restored primary backend is unavailable")
	}

	locationsBySource := make(map[string][]string)
	for _, location := range manifest.Locations {
		for _, sourceID := range location.StoreIDs {
			locationsBySource[sourceID] = append(locationsBySource[sourceID], location.Hash)
		}
	}
	remoteOnly := make(map[string]bool)
	allowAuditedRemoteOnly := make(map[string]bool)
	for _, mapped := range options.Map.Stores {
		binding, ok := options.Bindings[mapped.Binding]
		if !ok {
			return fmt.Errorf(
				"backupapp: restore binding %q is not loaded; update config.toml and restart the daemon",
				mapped.Binding,
			)
		}
		candidate, err := metadata.PrepareSecondaryBlobStore(
			mapped.Name, binding.Kind, mapped.Binding,
		)
		if err != nil {
			return err
		}
		backend, err := claimRestoreBackend(
			ctx, metadata.VaultID(), candidate, binding, mapped.Takeover,
		)
		if err != nil {
			return fmt.Errorf("backupapp: mapping source store %s: %w", mapped.SourceID, err)
		}
		func() {
			defer func() {
				if closer, ok := backend.(io.Closer); ok {
					retErr = errors.Join(retErr, closer.Close())
				}
			}()
			if err = metadata.RegisterBlobStore(ctx, candidate); err != nil {
				return
			}
			for _, hashText := range locationsBySource[mapped.SourceID] {
				var hash packstore.Hash
				hash, err = packstore.ParseHash(hashText)
				if err != nil {
					return
				}
				var info store.BlobInfo
				info, err = metadata.BlobInfo(ctx, hashText)
				if err != nil {
					return
				}
				var resolution packstore.Resolution
				resolution, err = metadata.ResolveBlobLocations(ctx, hash)
				if err != nil {
					return
				}
				index := slices.IndexFunc(resolution.Candidates, func(
					location packstore.ReadLocation,
				) bool {
					return location.StoreID == packstore.StoreID(primary.ID)
				})
				if index < 0 {
					err = fmt.Errorf("restored blob %s has no primary source", hashText)
					return
				}
				var receipt packstore.MoveReceipt
				receipt, err = moveRestoredBlob(
					ctx, sourceBackend, backend, mapped.Takeover, packstore.MoveRequest{
						Source:      resolution.Candidates[index],
						Destination: packstore.StoreID(candidate.ID),
						Identity:    packstore.BlobIdentity{Hash: hash, Size: info.Size},
					},
				)
				if err != nil {
					return
				}
				if !receipt.Verified {
					err = fmt.Errorf("restored destination %s did not verify %s", candidate.ID, hashText)
					return
				}
				err = metadata.AddRestoredBlobLocation(ctx, hashText, receipt.Destination)
				if err != nil {
					return
				}
				if mapped.RemoteOnly {
					remoteOnly[hashText] = true
					allowAuditedRemoteOnly[hashText] =
						allowAuditedRemoteOnly[hashText] || mapped.AllowAuditedRemoteOnly
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	for hash := range remoteOnly {
		if err := metadata.RetireRestoredPrimary(
			ctx, hash, allowAuditedRemoteOnly[hash],
		); err != nil {
			return err
		}
	}
	return nil
}

func moveRestoredBlob(
	ctx context.Context,
	source packstore.ReadBackend,
	destination packstore.Backend,
	takeover bool,
	request packstore.MoveRequest,
) (packstore.MoveReceipt, error) {
	receipt, err := packstore.Move(ctx, source, destination, request)
	repairable := errors.Is(err, packstore.ErrPhysicalCorrupt) ||
		errors.Is(err, packstore.ErrContentMismatch)
	if err == nil || !takeover || !repairable {
		if err != nil {
			return receipt, fmt.Errorf("backupapp: moving restored blob: %w", err)
		}
		return receipt, nil
	}
	repair, ok := destination.(packstore.RepairBackend)
	if !ok {
		return packstore.MoveReceipt{}, fmt.Errorf(
			"backupapp: taken-over destination is corrupt and cannot be repaired: %w", err,
		)
	}
	if request.Source.Loose == nil {
		return packstore.MoveReceipt{}, fmt.Errorf(
			"backupapp: restored primary source is not loose: %w", err,
		)
	}
	stream, size, openErr := source.OpenLoose(
		ctx, request.Identity.Hash, *request.Source.Loose,
	)
	if openErr != nil {
		return packstore.MoveReceipt{}, fmt.Errorf(
			"backupapp: opening restored primary source: %w", openErr,
		)
	}
	if size != request.Identity.Size {
		_ = stream.Close()
		return packstore.MoveReceipt{}, fmt.Errorf(
			"%w: restored primary size %d does not match %d",
			packstore.ErrPhysicalCorrupt, size, request.Identity.Size,
		)
	}
	repaired, repairErr := repair.RepairLoose(
		ctx, request.Identity.Hash, stream, packstore.PublishOptions{
			ExpectedSize: request.Identity.Size,
			SizeKnown:    true,
			MaxBytes:     request.Identity.Size,
			Durability:   packstore.DurablePublication,
		},
	)
	sourceErr := errors.Join(stream.Verify(), stream.Close())
	if err := errors.Join(repairErr, sourceErr); err != nil {
		return packstore.MoveReceipt{}, err
	}
	readback, readbackSize, err := destination.OpenLoose(
		ctx, request.Identity.Hash, repaired.Location,
	)
	if err != nil {
		return packstore.MoveReceipt{}, fmt.Errorf(
			"backupapp: opening repaired destination: %w", err,
		)
	}
	if readbackSize != request.Identity.Size {
		_ = readback.Close()
		return packstore.MoveReceipt{}, fmt.Errorf(
			"%w: repaired destination size %d does not match %d",
			packstore.ErrPhysicalCorrupt, readbackSize, request.Identity.Size,
		)
	}
	written, copyErr := io.Copy(io.Discard, readback)
	verifyErr := readback.Verify()
	closeErr := readback.Close()
	if err := errors.Join(copyErr, verifyErr, closeErr); err != nil {
		return packstore.MoveReceipt{}, err
	}
	if written != request.Identity.Size {
		return packstore.MoveReceipt{}, fmt.Errorf(
			"%w: repaired destination produced %d bytes, expected %d",
			packstore.ErrPhysicalCorrupt, written, request.Identity.Size,
		)
	}
	return packstore.MoveReceipt{
		Destination: packstore.ReadLocation{
			StoreID: repaired.StoreID, Generation: repaired.Generation,
			Loose: &repaired.Location,
		},
		Verified: true,
		Created:  repaired.Created,
	}, nil
}

func claimRestoreBackend(
	ctx context.Context,
	vaultID string,
	candidate store.BlobStore,
	binding config.StoreBindingConfig,
	takeover bool,
) (packstore.Backend, error) {
	backend, err := blob.NewConfiguredBackend(ctx, binding, nil)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (packstore.Backend, error) {
		if closer, ok := backend.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, err
	}
	var prior *packstore.Ownership
	current, markerErr := backend.Ownership(ctx)
	switch {
	case errors.Is(markerErr, fs.ErrNotExist),
		errors.Is(markerErr, packstore.ErrPhysicalMissing):
		if takeover {
			return closeOnError(errors.New(
				"takeover was requested but the target namespace has no ownership marker",
			))
		}
		inspector, ok := backend.(packstore.NamespaceInspector)
		if !ok {
			return closeOnError(errors.New(
				"target backend cannot prove an unmarked namespace is empty",
			))
		}
		empty, err := inspector.NamespaceEmpty(ctx)
		if err != nil {
			return closeOnError(err)
		}
		if !empty {
			return closeOnError(errors.New("unmarked target namespace is not empty"))
		}
	case markerErr != nil:
		return closeOnError(markerErr)
	case !takeover:
		return closeOnError(errors.New(
			"target namespace is already owned; explicit takeover is required",
		))
	default:
		prior = &current
	}
	next := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  vaultID, Store: packstore.StoreID(candidate.ID),
		Epoch: candidate.OwnershipEpoch,
	}
	if err := backend.ReplaceOwnership(ctx, next, prior); err != nil {
		return closeOnError(err)
	}
	actual, err := backend.Ownership(ctx)
	if err != nil {
		return closeOnError(err)
	}
	if actual != next {
		return closeOnError(errors.New("target ownership marker failed read-back"))
	}
	if err := blob.ProbeConfiguredBackend(ctx, backend); err != nil {
		return closeOnError(err)
	}
	return backend, nil
}
