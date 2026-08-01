package backupapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/storenamespace"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
)

const (
	RestoreStoreMapVersion     = 1
	restoreStoreKindFilesystem = "filesystem"
	restoreStoreKindS3         = "s3"
)

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
	Map                      *RestoreStoreMap
	Bindings                 map[string]config.StoreBindingConfig
	ProtectedFilesystemRoots []string
}

type preparedRestoreMapping struct {
	mapping RestoreStoreMapping
	binding config.StoreBindingConfig
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
		if err := store.ValidateSecondaryBlobStoreName(mapped.Name); err != nil {
			return fmt.Errorf("backupapp: store-map entry %d target name: %w", index, err)
		}
		if mapped.Binding == "" {
			return fmt.Errorf("backupapp: store-map entry %d requires a binding", index)
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

func prepareRestoreMappings(
	target string,
	mapping RestoreStoreMap,
	manifest placementManifest,
	bindings map[string]config.StoreBindingConfig,
	protectedFilesystemRoots []string,
) ([]preparedRestoreMapping, error) {
	if err := validateRestoreStoreMap(mapping, manifest); err != nil {
		return nil, err
	}
	prepared := make([]preparedRestoreMapping, 0, len(mapping.Stores))
	for _, mapped := range mapping.Stores {
		binding, ok := bindings[mapped.Binding]
		if !ok {
			return nil, fmt.Errorf(
				"backupapp: restore binding %q is not loaded; update config.toml and restart the daemon",
				mapped.Binding,
			)
		}
		if err := validateRestoreBinding(mapped.Binding, binding); err != nil {
			return nil, err
		}
		prepared = append(prepared, preparedRestoreMapping{
			mapping: mapped,
			binding: binding,
		})
	}
	if err := validateRestoreNamespaces(target, prepared, protectedFilesystemRoots); err != nil {
		return nil, err
	}
	return prepared, nil
}

func validateRestoreBinding(name string, binding config.StoreBindingConfig) error {
	switch binding.Kind {
	case restoreStoreKindFilesystem:
		if binding.Path == "" || !filepath.IsAbs(binding.Path) {
			return fmt.Errorf(
				"backupapp: filesystem binding %q requires an absolute path", name,
			)
		}
	case restoreStoreKindS3:
		if binding.Bucket == "" {
			return fmt.Errorf("backupapp: S3 binding %q requires a bucket", name)
		}
		if _, err := canonicalS3Endpoint(binding.Endpoint, binding.Region); err != nil {
			return fmt.Errorf("backupapp: S3 binding %q endpoint: %w", name, err)
		}
		if binding.Prefix != "" &&
			(strings.HasPrefix(binding.Prefix, "/") ||
				strings.Contains(binding.Prefix, `\`) ||
				strings.Contains(binding.Prefix, "//") ||
				path.Clean(binding.Prefix) != binding.Prefix ||
				binding.Prefix == "." || binding.Prefix == ".." ||
				strings.HasPrefix(binding.Prefix, "../")) {
			return fmt.Errorf(
				"backupapp: S3 binding %q prefix %q is not canonical",
				name, binding.Prefix,
			)
		}
	default:
		return fmt.Errorf(
			"backupapp: restore binding %q has unsupported kind %q", name, binding.Kind,
		)
	}
	return nil
}

type restoreNamespace struct {
	name     string
	kind     string
	path     string
	endpoint string
	bucket   string
	prefix   string
}

func validateRestoreNamespaces(
	target string,
	prepared []preparedRestoreMapping,
	protectedFilesystemRoots []string,
) error {
	primary, err := canonicalFilesystemNamespace(target)
	if err != nil {
		return fmt.Errorf("backupapp: resolving restored primary namespace: %w", err)
	}
	namespaces := []restoreNamespace{{
		name: "restored vault", kind: restoreStoreKindFilesystem, path: primary,
	}}
	for _, protected := range protectedFilesystemRoots {
		if protected == "" {
			continue
		}
		canonical, canonicalErr := canonicalFilesystemNamespace(protected)
		if canonicalErr != nil {
			return fmt.Errorf("backupapp: resolving protected storage: %w", canonicalErr)
		}
		namespaces = append(namespaces, restoreNamespace{
			name: "protected storage", kind: restoreStoreKindFilesystem, path: canonical,
		})
	}
	for _, item := range prepared {
		namespace := restoreNamespace{
			name: item.mapping.Name, kind: item.binding.Kind,
		}
		switch item.binding.Kind {
		case restoreStoreKindFilesystem:
			namespace.path, err = canonicalFilesystemNamespace(item.binding.Path)
			if err != nil {
				return fmt.Errorf(
					"backupapp: resolving restore binding %q: %w",
					item.mapping.Binding, err,
				)
			}
		case restoreStoreKindS3:
			namespace.endpoint, err = canonicalS3Endpoint(
				item.binding.Endpoint, item.binding.Region,
			)
			if err != nil {
				return fmt.Errorf(
					"backupapp: resolving restore binding %q endpoint: %w",
					item.mapping.Binding, err,
				)
			}
			namespace.bucket = item.binding.Bucket
			namespace.prefix = item.binding.Prefix
		}
		for _, prior := range namespaces {
			overlaps, overlapErr := restoreNamespacesOverlap(prior, namespace)
			if overlapErr != nil {
				return fmt.Errorf(
					"backupapp: comparing target store %q with %q: %w",
					namespace.name, prior.name, overlapErr,
				)
			}
			if overlaps {
				return fmt.Errorf(
					"backupapp: target store %q overlaps %q",
					namespace.name, prior.name,
				)
			}
		}
		namespaces = append(namespaces, namespace)
	}
	return nil
}

func canonicalFilesystemNamespace(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var missing []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for _, name := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, name)
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, fs.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func canonicalS3Endpoint(raw, region string) (string, error) {
	return storenamespace.CanonicalS3Endpoint(raw, region)
}

func restoreNamespacesOverlap(first, second restoreNamespace) (bool, error) {
	if first.kind != second.kind {
		return false, nil
	}
	if first.kind == restoreStoreKindFilesystem {
		overlaps, err := ingest.PathsOverlap(first.path, second.path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return overlaps, err
	}
	if first.endpoint != second.endpoint || first.bucket != second.bucket {
		return false, nil
	}
	return s3PrefixContains(first.prefix, second.prefix) ||
		s3PrefixContains(second.prefix, first.prefix), nil
}

func s3PrefixContains(parent, child string) bool {
	return parent == "" || parent == child || strings.HasPrefix(child, parent+"/")
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
	prepared, err := prepareRestoreMappings(
		target, *options.Map, manifest, options.Bindings,
		options.ProtectedFilesystemRoots,
	)
	if err != nil {
		return err
	}
	restoreBindings := make(map[string]config.StoreBindingConfig, len(prepared))
	for _, item := range prepared {
		restoreBindings[item.mapping.Binding] = item.binding
	}
	if err := config.EnsureStoreBindings(target, restoreBindings); err != nil {
		return fmt.Errorf("backupapp: provisioning restored store bindings: %w", err)
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
	primaryOwnership := store.NewPackCatalog(metadata).PrimaryOwnership()
	layout, err := packstore.NewLayout(
		filepath.Join(target, "blobs"), packstore.LayoutOptions{
			Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
		},
	)
	if err != nil {
		return fmt.Errorf("backupapp: opening restored primary layout: %w", err)
	}
	primaryBackend, err := packstore.NewFilesystemBackend(
		layout, packstore.FilesystemBackendOptions{
			ExpectedOwnership: &primaryOwnership,
			Limits:            blob.StorageLimits(),
		},
	)
	if err != nil {
		return fmt.Errorf("backupapp: opening restored primary: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, primaryBackend.Close())
	}()
	actual, err := primaryBackend.Ownership(ctx)
	if err != nil || actual != primaryOwnership {
		return errors.Join(
			errors.New("backupapp: restored primary ownership does not match staged catalog"),
			err,
		)
	}
	var sourceBackend packstore.ReadBackend = primaryBackend

	locationsBySource := make(map[string][]string)
	for _, location := range manifest.Locations {
		for _, sourceID := range location.StoreIDs {
			locationsBySource[sourceID] = append(locationsBySource[sourceID], location.Hash)
		}
	}
	remoteOnly := make(map[string]bool)
	allowAuditedRemoteOnly := make(map[string]bool)
	claimedNamespaces := make(map[packstore.Ownership]string, len(prepared))
	for _, item := range prepared {
		mapped := item.mapping
		binding := item.binding
		candidate, err := metadata.PrepareSecondaryBlobStore(
			mapped.Name, binding.Kind, mapped.Binding,
		)
		if err != nil {
			return err
		}
		backend, err := claimRestoreBackend(
			ctx, metadata.VaultID(), candidate, binding, mapped.Takeover,
			claimedNamespaces,
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
		_, err := metadata.RetireRestoredPrimary(
			ctx, hash, allowAuditedRemoteOnly[hash],
		)
		if err != nil {
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
	claimedNamespaces map[packstore.Ownership]string,
) (packstore.Backend, error) {
	inspector, err := blob.NewInspectionBackend(ctx, binding)
	if err != nil {
		return nil, err
	}
	closeInspector := func() error {
		if closer, ok := inspector.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	}
	closeInspectorOnError := func(err error) (packstore.Backend, error) {
		return nil, errors.Join(err, closeInspector())
	}
	var prior *packstore.Ownership
	current, markerErr := inspector.Ownership(ctx)
	if markerErr == nil {
		if claimedBy, claimed := claimedNamespaces[current]; claimed {
			return closeInspectorOnError(fmt.Errorf(
				"target namespace was already claimed by target store %q",
				claimedBy,
			))
		}
	}
	switch {
	case errors.Is(markerErr, fs.ErrNotExist),
		errors.Is(markerErr, packstore.ErrPhysicalMissing):
		if takeover {
			return closeInspectorOnError(errors.New(
				"takeover was requested but the target namespace has no ownership marker",
			))
		}
		if err := requireEmptyRestoreNamespace(ctx, inspector); err != nil {
			return closeInspectorOnError(err)
		}
	case markerErr != nil:
		return closeInspectorOnError(markerErr)
	case !takeover:
		return closeInspectorOnError(errors.New(
			"target namespace is already owned; explicit takeover is required",
		))
	default:
		prior = &current
	}
	if err := closeInspector(); err != nil {
		return nil, err
	}
	if binding.Kind == restoreStoreKindFilesystem {
		if err := blob.EnsureFilesystemNamespace(binding.Path); err != nil {
			return nil, fmt.Errorf("prepare private restore store: %w", err)
		}
	}
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
	revalidated, revalidateErr := backend.Ownership(ctx)
	if prior == nil {
		if !errors.Is(revalidateErr, fs.ErrNotExist) &&
			!errors.Is(revalidateErr, packstore.ErrPhysicalMissing) {
			if revalidateErr == nil {
				revalidateErr = errors.New("target ownership marker changed during restore preflight")
			}
			return closeOnError(revalidateErr)
		}
		if err := requireEmptyRestoreNamespace(ctx, backend); err != nil {
			return closeOnError(err)
		}
	} else if revalidateErr != nil || revalidated != *prior {
		return closeOnError(errors.New(
			"target ownership marker changed during restore preflight",
		))
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
	if closer, ok := backend.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			return nil, err
		}
	}
	fenced, err := blob.NewConfiguredBackend(ctx, binding, &next)
	if err != nil {
		return nil, err
	}
	closeFencedOnError := func(err error) (packstore.Backend, error) {
		if closer, ok := fenced.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, err
	}
	actual, err = fenced.Ownership(ctx)
	if err != nil {
		return closeFencedOnError(err)
	}
	if actual != next {
		return closeFencedOnError(errors.New(
			"target ownership marker changed after restore claim",
		))
	}
	if err := blob.ProbeConfiguredBackend(ctx, fenced); err != nil {
		return closeFencedOnError(err)
	}
	if fenced.StoreID() != next.Store {
		return closeFencedOnError(errors.New(
			"target restore backend is not fenced to its claimed store",
		))
	}
	claimedNamespaces[next] = candidate.Name
	return fenced, nil
}

func requireEmptyRestoreNamespace(
	ctx context.Context, backend packstore.Backend,
) error {
	inspector, ok := backend.(packstore.NamespaceInspector)
	if !ok {
		return errors.New("target backend cannot prove an unmarked namespace is empty")
	}
	empty, err := inspector.NamespaceEmpty(ctx)
	if err != nil {
		return fmt.Errorf("inspect target namespace: %w", err)
	}
	if !empty {
		return errors.New("unmarked target namespace is not empty")
	}
	return nil
}
