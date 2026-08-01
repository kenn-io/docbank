package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/kit/safefileio"
)

const (
	primaryRestoreHandoffFormat = 1
	primaryRestoreHandoffName   = "primary-restore-handoff.json"
	maxPrimaryRestoreHandoff    = 16 << 10
)

type primaryRestoreHandoffRecord struct {
	Format              uint32               `json:"format"`
	Prior               *packstore.Ownership `json:"prior,omitempty"`
	PriorDatabaseDigest *string              `json:"prior_database_digest"`
	Next                packstore.Ownership  `json:"next"`
}

// PrimaryRestoreHandoff coordinates the built-in primary marker with one
// staged restore database. The durable record lets the next opener determine
// whether a crash happened before or after database publication.
type PrimaryRestoreHandoff struct {
	blobsDir string
	record   primaryRestoreHandoffRecord
	started  bool
}

// NewPrimaryRestoreHandoff prepares a handoff value without changing storage.
func NewPrimaryRestoreHandoff(
	blobsDir string, next packstore.Ownership, priorDatabaseDigest *string,
) (*PrimaryRestoreHandoff, error) {
	if err := next.Validate(); err != nil {
		return nil, fmt.Errorf("validating restored primary ownership: %w", err)
	}
	if priorDatabaseDigest == nil {
		return nil, errors.New("prior restore database discriminator is required")
	}
	layout, err := newLayout(blobsDir)
	if err != nil {
		return nil, err
	}
	return &PrimaryRestoreHandoff{
		blobsDir: layout.Root(),
		record: primaryRestoreHandoffRecord{
			Format:              primaryRestoreHandoffFormat,
			PriorDatabaseDigest: new(*priorDatabaseDigest),
			Next:                next,
		},
	}, nil
}

// Prepare durably records the transition, then replaces and verifies the
// primary marker. The caller must hold exclusive coordination for the vault.
func (h *PrimaryRestoreHandoff) Prepare(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pending, err := PrimaryRestoreHandoffPending(h.blobsDir)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("primary restore handoff already requires recovery")
	}
	backend, current, err := openPrimaryOwnershipBackend(ctx, h.blobsDir)
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()
	h.record.Prior = current
	if err := writePrimaryRestoreHandoff(h.blobsDir, h.record); err != nil {
		return err
	}
	h.started = true
	if err := backend.ReplaceOwnership(ctx, h.record.Next, current); err != nil {
		return fmt.Errorf("publishing restored primary ownership: %w", err)
	}
	actual, err := backend.Ownership(ctx)
	if err != nil {
		return fmt.Errorf("reading restored primary ownership: %w", err)
	}
	if actual != h.record.Next {
		return errors.New("restored primary ownership failed read-back")
	}
	return nil
}

// Commit verifies the post-restore marker and removes the recovery record.
func (h *PrimaryRestoreHandoff) Commit(ctx context.Context) error {
	return h.finish(ctx, &h.record.Next)
}

// Rollback restores the marker that preceded an unpublished restore.
func (h *PrimaryRestoreHandoff) Rollback(ctx context.Context) error {
	return h.finish(ctx, h.record.Prior)
}

// PrimaryRestoreHandoffPending reports whether a prior restore needs marker
// reconciliation. It does not follow a replacement symlink at the record path.
func PrimaryRestoreHandoffPending(blobsDir string) (bool, error) {
	info, err := os.Lstat(primaryRestoreHandoffPath(blobsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking primary restore handoff: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("primary restore handoff is not a regular file")
	}
	return true, nil
}

// RecoverPrimaryRestoreHandoff reconciles an interrupted marker transition
// against the ownership recorded by the database that is currently published.
func RecoverPrimaryRestoreHandoff(
	ctx context.Context,
	blobsDir string,
	published *packstore.Ownership,
	publishedDatabaseDigest *string,
) error {
	record, exists, err := readPrimaryRestoreHandoff(blobsDir)
	if err != nil || !exists {
		return err
	}
	if published != nil {
		if err := published.Validate(); err != nil {
			return fmt.Errorf("validating published primary ownership: %w", err)
		}
	}
	var desired *packstore.Ownership
	switch {
	case publishedDatabaseDigest != nil &&
		stringPointerEqual(publishedDatabaseDigest, record.PriorDatabaseDigest):
		desired = record.Prior
	case published != nil && *published == record.Next:
		desired = &record.Next
	case publishedDatabaseDigest == nil && published != nil &&
		record.Prior != nil && *published == *record.Prior:
		desired = record.Prior
	default:
		return fmt.Errorf(
			"%w: published database matches neither side of the restore handoff",
			packstore.ErrStoreFenced,
		)
	}
	return reconcilePrimaryOwnership(ctx, blobsDir, record, desired)
}

func (h *PrimaryRestoreHandoff) finish(
	ctx context.Context, desired *packstore.Ownership,
) error {
	record, exists, err := readPrimaryRestoreHandoff(h.blobsDir)
	if err != nil {
		return err
	}
	if !exists {
		if !h.started {
			return nil
		}
		record = h.record
	} else if !samePrimaryRestoreHandoff(record, h.record) {
		return fmt.Errorf(
			"%w: primary restore handoff changed during restore",
			packstore.ErrStoreFenced,
		)
	}
	return reconcilePrimaryOwnership(ctx, h.blobsDir, record, desired)
}

func reconcilePrimaryOwnership(
	ctx context.Context,
	blobsDir string,
	record primaryRestoreHandoffRecord,
	desired *packstore.Ownership,
) error {
	backend, actual, err := openPrimaryOwnershipBackend(ctx, blobsDir)
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()
	if !ownershipPointerEqual(actual, record.Prior) &&
		!ownershipPointerEqual(actual, &record.Next) {
		return fmt.Errorf(
			"%w: primary marker matches neither side of the restore handoff",
			packstore.ErrStoreFenced,
		)
	}
	if !ownershipPointerEqual(desired, record.Prior) &&
		!ownershipPointerEqual(desired, &record.Next) {
		return errors.New("requested primary restore handoff state is invalid")
	}
	if !ownershipPointerEqual(actual, desired) {
		if desired == nil {
			if err := removePrimaryOwnershipMarker(blobsDir); err != nil {
				return err
			}
		} else if err := backend.ReplaceOwnership(ctx, *desired, actual); err != nil {
			return fmt.Errorf("reconciling restored primary ownership: %w", err)
		}
	}
	if desired != nil {
		verified, verifyErr := backend.Ownership(ctx)
		if verifyErr != nil || verified != *desired {
			return errors.Join(errors.New("reconciled primary ownership failed read-back"), verifyErr)
		}
	}
	return removePrimaryRestoreHandoff(blobsDir)
}

func openPrimaryOwnershipBackend(
	ctx context.Context, blobsDir string,
) (*packstore.FilesystemBackend, *packstore.Ownership, error) {
	layout, err := newLayout(blobsDir)
	if err != nil {
		return nil, nil, err
	}
	backend, err := packstore.NewFilesystemBackend(
		layout, packstore.FilesystemBackendOptions{Limits: StorageLimits()},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("opening primary ownership backend: %w", err)
	}
	actual, err := backend.Ownership(ctx)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, packstore.ErrPhysicalMissing) {
		return backend, nil, nil
	}
	if err != nil {
		_ = backend.Close()
		return nil, nil, fmt.Errorf("reading primary ownership marker: %w", err)
	}
	return backend, &actual, nil
}

func writePrimaryRestoreHandoff(
	blobsDir string, record primaryRestoreHandoffRecord,
) (resultErr error) {
	if err := validatePrimaryRestoreHandoff(record); err != nil {
		return err
	}
	dir := filepath.Dir(primaryRestoreHandoffPath(blobsDir))
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return fmt.Errorf("securing primary restore handoff directory: %w", err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding primary restore handoff: %w", err)
	}
	staged, err := os.CreateTemp(dir, ".primary-restore-")
	if err != nil {
		return fmt.Errorf("creating primary restore handoff staging: %w", err)
	}
	stagedPath := staged.Name()
	open := true
	defer func() {
		if open {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if err := os.Remove(stagedPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := staged.Chmod(0o600); err != nil {
		return fmt.Errorf("protecting primary restore handoff staging: %w", err)
	}
	if _, err := staged.Write(encoded); err != nil {
		return fmt.Errorf("writing primary restore handoff staging: %w", err)
	}
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("syncing primary restore handoff staging: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("closing primary restore handoff staging: %w", err)
	}
	open = false
	if err := os.Rename(stagedPath, primaryRestoreHandoffPath(blobsDir)); err != nil {
		return fmt.Errorf("publishing primary restore handoff: %w", err)
	}
	if err := pack.SyncDir(dir); err != nil {
		return fmt.Errorf("syncing primary restore handoff directory: %w", err)
	}
	return nil
}

func readPrimaryRestoreHandoff(
	blobsDir string,
) (primaryRestoreHandoffRecord, bool, error) {
	path := primaryRestoreHandoffPath(blobsDir)
	file, err := safefileio.OpenCurrentUserFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return primaryRestoreHandoffRecord{}, false, nil
	}
	if err != nil {
		return primaryRestoreHandoffRecord{}, false,
			fmt.Errorf("opening primary restore handoff: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxPrimaryRestoreHandoff+1))
	if err != nil {
		return primaryRestoreHandoffRecord{}, false,
			fmt.Errorf("reading primary restore handoff: %w", err)
	}
	if len(raw) > maxPrimaryRestoreHandoff {
		return primaryRestoreHandoffRecord{}, false,
			errors.New("primary restore handoff is too large")
	}
	var record primaryRestoreHandoffRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return primaryRestoreHandoffRecord{}, false,
			fmt.Errorf("decoding primary restore handoff: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return primaryRestoreHandoffRecord{}, false,
			errors.New("primary restore handoff contains trailing JSON")
	}
	if err := validatePrimaryRestoreHandoff(record); err != nil {
		return primaryRestoreHandoffRecord{}, false, err
	}
	return record, true, nil
}

func validatePrimaryRestoreHandoff(record primaryRestoreHandoffRecord) error {
	if record.Format != primaryRestoreHandoffFormat {
		return fmt.Errorf("unsupported primary restore handoff format %d", record.Format)
	}
	if err := record.Next.Validate(); err != nil {
		return fmt.Errorf("invalid next primary ownership: %w", err)
	}
	if record.PriorDatabaseDigest == nil {
		return errors.New("primary restore handoff has no prior database discriminator")
	}
	if *record.PriorDatabaseDigest != "" &&
		!isCanonicalSHA256(*record.PriorDatabaseDigest) {
		return errors.New("primary restore handoff has an invalid prior database digest")
	}
	if record.Prior != nil {
		if err := record.Prior.Validate(); err != nil {
			return fmt.Errorf("invalid prior primary ownership: %w", err)
		}
		if *record.Prior == record.Next {
			return errors.New("primary restore handoff does not change ownership")
		}
	}
	return nil
}

func removePrimaryOwnershipMarker(blobsDir string) error {
	layout, err := newLayout(blobsDir)
	if err != nil {
		return err
	}
	if err := os.Remove(layout.OwnershipPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing restored primary ownership: %w", err)
	}
	if err := pack.SyncDir(layout.Root()); err != nil {
		return fmt.Errorf("syncing restored primary ownership removal: %w", err)
	}
	return nil
}

func removePrimaryRestoreHandoff(blobsDir string) error {
	path := primaryRestoreHandoffPath(blobsDir)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing primary restore handoff: %w", err)
	}
	if err := pack.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("syncing primary restore handoff removal: %w", err)
	}
	return nil
}

func primaryRestoreHandoffPath(blobsDir string) string {
	return filepath.Join(blobsDir, "tmp", primaryRestoreHandoffName)
}

func ownershipPointerEqual(left, right *packstore.Ownership) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func samePrimaryRestoreHandoff(left, right primaryRestoreHandoffRecord) bool {
	return left.Format == right.Format && left.Next == right.Next &&
		ownershipPointerEqual(left.Prior, right.Prior) &&
		stringPointerEqual(left.PriorDatabaseDigest, right.PriorDatabaseDigest)
}

func stringPointerEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func isCanonicalSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, b := range []byte(value) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}
