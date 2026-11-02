package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"go.kenn.io/kit/pack"
)

// SnapshotInfo identifies the exact snapshot held by a reader.
type SnapshotInfo struct {
	RepositoryID, SnapshotID string
	MinReaderVersion         int
}

// SnapshotObject describes a logical file listed in the selected snapshot.
type SnapshotObject struct {
	ID    pack.BlobID
	Bytes int64
}

// SnapshotReader copies selected logical files without restoring an application.
// It holds a shared repository lease until Close. Calls must be sequential;
// finish each copy before starting another or closing the reader.
type SnapshotReader struct {
	info     SnapshotInfo
	objects  map[pack.BlobID]SnapshotObject
	extras   map[string]SnapshotObject
	recipes  map[pack.BlobID]objectRecipe
	packs    *packReaders
	lease    *RepoLock
	closed   bool
	closeErr error
}

// OpenSnapshot opens an exact snapshot, never the latest. Membership comes
// from its attachment lists and extras tree, not the repository-wide index.
// The pack extension must match the application that created the snapshot.
// Lock contention wraps ErrRepoLocked; it does not indicate corruption.
func (r *Repo) OpenSnapshot(ctx context.Context, snapshotID, packExtension string) (_ *SnapshotReader, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validatePackExtension(packExtension); err != nil {
		return nil, err
	}
	lease, err := r.AcquireSharedLock("read snapshot", false)
	if err != nil {
		return nil, err
	}
	s := &SnapshotReader{lease: lease, objects: make(map[pack.BlobID]SnapshotObject), extras: make(map[string]SnapshotObject)}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	manifest, err := r.LoadManifest(snapshotID)
	if err != nil {
		return nil, err
	}
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	s.info = SnapshotInfo{RepositoryID: r.cfg.RepoID, SnapshotID: manifest.SnapshotID, MinReaderVersion: manifest.MinReaderVersion}
	s.packs = &packReaders{repo: r, known: known, ext: packExtension, readers: make(map[string]*cachedPack)}
	s.recipes, err = loadObjectRecipesWith(ctx, known, manifest, s.packs.open)
	if err != nil {
		return nil, err
	}
	if err := s.loadMembership(ctx, manifest, known); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SnapshotReader) loadMembership(ctx context.Context, m *Manifest, known map[pack.BlobID]IndexEntry) error {
	for _, list := range m.Attachments.Lists {
		raw, err := s.readMetadata(ctx, list)
		if err != nil {
			return err
		}
		refs, err := DecodeAttachmentList(raw)
		if err != nil {
			return fmt.Errorf("backup: snapshot attachment list %s: %w", list, err)
		}
		for _, ref := range refs {
			if _, err := s.addObject(ref.Hash, ref.Size, known); err != nil {
				return err
			}
		}
	}
	if m.Extras.Tree == "" {
		return nil
	}
	raw, err := s.readMetadata(ctx, m.Extras.Tree)
	if err != nil {
		return err
	}
	var tree ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("backup: snapshot extras tree: %w", err)
	}
	if err := checkExtrasCollisions(tree.Entries); err != nil {
		return err
	}
	for _, entry := range tree.Entries {
		object, err := s.addObject(entry.Blob, entry.Size, known)
		if err != nil {
			return fmt.Errorf("backup: snapshot extra %q: %w", entry.Path, err)
		}
		s.extras[entry.Path] = object
	}
	return nil
}

func (s *SnapshotReader) readMetadata(ctx context.Context, encoded string) ([]byte, error) {
	id, err := pack.ParseBlobID(encoded)
	if err != nil {
		return nil, fmt.Errorf("backup: snapshot metadata identity: %w", err)
	}
	stream, err := s.packs.open(ctx, id)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(stream)
	return raw, errors.Join(readErr, stream.Close())
}

func (s *SnapshotReader) addObject(encoded string, size int64, known map[pack.BlobID]IndexEntry) (SnapshotObject, error) {
	id, err := pack.ParseBlobID(encoded)
	if err != nil {
		return SnapshotObject{}, fmt.Errorf("backup: snapshot object identity: %w", err)
	}
	if size < 0 || size > MaxObjectBytes {
		return SnapshotObject{}, fmt.Errorf("backup: snapshot object %s has invalid size %d", id, size)
	}
	if recipe, exists := s.recipes[id]; exists {
		if recipe.Bytes != size {
			return SnapshotObject{}, fmt.Errorf("backup: object %s recipe differs from recorded size", id)
		}
	} else if _, exists := known[id]; !exists {
		return SnapshotObject{}, fmt.Errorf("backup: snapshot object %s not present in any index", id)
	}
	object := SnapshotObject{ID: id, Bytes: size}
	if old, exists := s.objects[id]; exists && old != object {
		return SnapshotObject{}, fmt.Errorf("backup: conflicting sizes for snapshot object %s", id)
	}
	s.objects[id] = object
	return object, nil
}

// Info returns immutable metadata that remains available after Close.
func (s *SnapshotReader) Info() SnapshotInfo { return s.info }

// Object looks up a logical file by hash. Metadata remains available after
// Close, but does not prove the payload is still available. Misses wrap fs.ErrNotExist.
func (s *SnapshotReader) Object(id pack.BlobID) (SnapshotObject, error) {
	object, exists := s.objects[id]
	if !exists {
		return SnapshotObject{}, fmt.Errorf("backup: object %s not in snapshot %s: %w", id, s.info.SnapshotID, fs.ErrNotExist)
	}
	return object, nil
}

// Extra looks up a file by its exact recorded name, also after Close.
// Misses wrap fs.ErrNotExist.
func (s *SnapshotReader) Extra(name string) (SnapshotObject, error) {
	object, exists := s.extras[name]
	if !exists {
		return SnapshotObject{}, fmt.Errorf("backup: extra %q not in snapshot %s: %w", name, s.info.SnapshotID, fs.ErrNotExist)
	}
	return object, nil
}

// CopyObject copies a snapshot member and verifies its complete hash and size.
// On any error, the caller must discard all output, including bytes already
// written. Closing the reader after failure still releases the lease.
func (s *SnapshotReader) CopyObject(ctx context.Context, id pack.BlobID, dst io.Writer) (int64, error) {
	if s.closed {
		return 0, fmt.Errorf("backup: snapshot reader: %w", fs.ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	object, err := s.Object(id)
	if err != nil {
		return 0, err
	}
	stream, err := openObjectWith(ctx, id, s.recipes, s.packs.open)
	if err != nil {
		return 0, err
	}
	if stream.Size() != object.Bytes {
		return 0, errors.Join(fmt.Errorf("backup: object %s differs from recorded size", id), stream.Close())
	}
	n, copyErr := io.Copy(dst, stream)
	if copyErr == nil && (n != object.Bytes || !stream.Verified()) {
		copyErr = fmt.Errorf("backup: object %s copy not completely verified", id)
	}
	return n, errors.Join(copyErr, stream.Close(), ctx.Err())
}

// CopyExtra copies an extra with the same verification and failure contract as CopyObject.
func (s *SnapshotReader) CopyExtra(ctx context.Context, name string, dst io.Writer) (int64, error) {
	object, err := s.Extra(name)
	if err != nil {
		return 0, err
	}
	return s.CopyObject(ctx, object.ID, dst)
}

// Close closes pack descriptors before releasing the repository lease.
// Repeated calls return the first close result.
func (s *SnapshotReader) Close() error {
	if !s.closed {
		s.closed = true
		if s.packs != nil {
			s.closeErr = s.packs.close()
		}
		s.closeErr = errors.Join(s.closeErr, s.lease.Release())
	}
	return s.closeErr
}
