package qmdexport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	markerName       = ".docbank-qmd-export.json"
	lockName         = ".publish.lock"
	rootEntryMaximum = 1024
	markerMaximum    = 4096
)

type rootMarker struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
	ID      string `json:"id"`
}

type ownedRoot struct {
	root, generations, staging *anchoredDir
	marker                     rootMarker
	absolute                   string // receipt only
	ancestors                  []*anchoredDir
	lock                       *os.File
	lockIdentity               entryIdentity
	locked                     bool
}

type ownershipHooks struct {
	waitingOnLock      func()
	afterBootstrapLock func()
	beforeClaimRecheck func()
	afterMarker        func()
}

func filesystemRoot(absolute string) bool {
	clean := filepath.Clean(absolute)
	volume := filepath.VolumeName(clean)
	return clean == volume+string(filepath.Separator) || (volume != "" && clean == volume && filepath.IsAbs(clean))
}

func checkRootRequest(ctx context.Context, absolute string) error {
	if ctx == nil {
		return errors.New("qmd export requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(absolute) || filesystemRoot(absolute) {
		return errors.New("qmd export root is invalid")
	}
	return nil
}

// acquireChain holds every traversed directory. Only the final component may
// be absent, and creating it never grants authority to delete the selected root.
func acquireChain(ctx context.Context, absolute string, create bool) (_ *ownedRoot, err error) {
	if err := checkRootRequest(ctx, absolute); err != nil {
		return nil, err
	}
	// Separator conversion does not clean dot components or follow links.
	nativePath := filepath.FromSlash(absolute)
	volume := filepath.VolumeName(nativePath)
	path := strings.TrimPrefix(nativePath, volume)
	path = strings.TrimSuffix(path, string(filepath.Separator))
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for _, part := range parts {
		if !validComponent(part) {
			return nil, errEntry
		}
	}
	r := &ownedRoot{absolute: absolute}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.Close())
		}
	}()
	parent, err := nativeVolumeRoot(volume + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	r.ancestors = append(r.ancestors, parent)
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		child, id, err := parent.openDirectory(part, true, false)
		if errors.Is(err, os.ErrNotExist) && i == len(parts)-1 && create {
			child, id, err = parent.createDir(part, stableEntry)
			if errors.Is(err, os.ErrExist) {
				child, id, err = parent.openDir(part)
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && i == len(parts)-1 && !create {
				return r, nil
			}
			return nil, err
		}
		if i == len(parts)-1 {
			r.root = child
		} else {
			r.ancestors = append(r.ancestors, child)
		}
		if err := parent.sameEntry(part, id); err != nil {
			return nil, err
		}
		parent = child
	}
	if err := validatePrivateEntry(r.root.file, true); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *ownedRoot) Close() error {
	if r == nil {
		return nil
	}
	var err error
	if r.locked {
		err = errors.Join(err, nativeUnlock(r.lock))
		r.locked = false
	}
	if r.lock != nil {
		err = errors.Join(err, r.lock.Close())
		r.lock = nil
	}
	err = errors.Join(err, r.staging.Close(), r.generations.Close(), r.root.Close())
	for _, ancestor := range slices.Backward(r.ancestors) {
		err = errors.Join(err, ancestor.Close())
	}
	r.ancestors = nil
	return err
}

func readBoundedFile(d *anchoredDir, name string, maximum int64) (_ []byte, err error) {
	f, id, err := d.openFile(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if id.info.Size() > maximum {
		return nil, errEntry
	}
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errEntry
	}
	if err := d.sameEntry(name, id); err != nil {
		return nil, err
	}
	return data, nil
}

func readRootMarker(d *anchoredDir) (rootMarker, error) {
	data, err := readBoundedFile(d, markerName, markerMaximum)
	if err != nil {
		return rootMarker{}, err
	}
	var marker rootMarker
	if err := json.Unmarshal(data, &marker, json.RejectUnknownMembers(true)); err != nil {
		return rootMarker{}, errEntry
	}
	if marker.Format != "docbank-qmd-export-root" || marker.Version != 1 || len(marker.ID) != 32 {
		return rootMarker{}, errEntry
	}
	for _, c := range marker.ID {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return rootMarker{}, errEntry
		}
	}
	return marker, nil
}

func validateCurrent(d *anchoredDir) error {
	data, err := readBoundedFile(d, "CURRENT", 65)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) != 65 || data[64] != '\n' || !validChecksum(string(data[:64])) {
		return errEntry
	}
	return nil
}

// validateLayout neither creates nor repairs any reserved entry. Unknown root
// entries remain present for the publication layer's incomplete-cleanup report.
func (r *ownedRoot) validateLayout(ctx context.Context) error {
	if err := validatePrivateEntry(r.root.file, true); err != nil {
		return err
	}
	if _, err := r.root.entries(ctx, rootEntryMaximum); err != nil {
		return err
	}
	marker, err := readRootMarker(r.root)
	if err != nil {
		return err
	}
	lock, id, err := r.root.openRegular(lockName, true)
	if err != nil {
		return err
	}
	if r.lock != nil {
		if err := heldIdentity(r.lock, id); err != nil {
			return errors.Join(err, lock.Close())
		}
	}
	if err := lock.Close(); err != nil {
		return err
	}
	g, _, err := r.root.openDir("generations")
	if err != nil {
		return err
	}
	s, _, err := r.root.openDir(".staging")
	if err != nil {
		return errors.Join(err, g.Close())
	}
	if err := validateCurrent(r.root); err != nil {
		return errors.Join(err, s.Close(), g.Close())
	}
	r.marker = marker
	r.generations = g
	r.staging = s
	return ctx.Err()
}

func preflightRoot(ctx context.Context, absolute string) (err error) {
	r, err := acquireChain(ctx, absolute, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	if r.root == nil {
		return nil
	}
	entries, err := r.root.entries(ctx, rootEntryMaximum)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if len(entries) == 1 && entries[0].Name() == lockName {
		lock, _, err := r.root.openRegular(lockName, true)
		if err != nil {
			return err
		}
		return lock.Close()
	}
	return r.validateLayout(ctx)
}

func openOwnedRoot(ctx context.Context, absolute string) (_ *ownedRoot, err error) {
	r, err := acquireChain(ctx, absolute, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.Close())
		}
	}()
	if r.root == nil {
		return nil, os.ErrNotExist
	}
	if err := r.validateLayout(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *ownedRoot) acquireLock(ctx context.Context, hooks ownershipHooks) error {
	notified := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := nativeTryLock(r.lock)
		if err == nil {
			r.locked = true
			if err := validatePrivateEntry(r.lock, false); err != nil {
				return err
			}
			return r.root.sameEntry(lockName, r.lockIdentity)
		}
		if !errors.Is(err, errLockBusy) {
			return err
		}
		if !notified && hooks.waitingOnLock != nil {
			hooks.waitingOnLock()
			notified = true
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func acquireOwnedRoot(ctx context.Context, absolute string, hooks ownershipHooks) (_ *ownedRoot, err error) {
	r, err := acquireChain(ctx, absolute, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.Close())
		}
	}()
	entries, err := r.root.entries(ctx, rootEntryMaximum)
	if err != nil {
		return nil, err
	}
	creator := false
	if len(entries) == 0 {
		r.lock, r.lockIdentity, err = r.root.createFile(lockName, stableEntry)
		creator = err == nil
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	if !creator {
		r.lock, r.lockIdentity, err = r.root.openRegular(lockName, true)
		if err != nil {
			return nil, err
		}
	}
	if err := r.acquireLock(ctx, hooks); err != nil {
		return nil, err
	}
	if !creator {
		if err := r.validateLayout(ctx); err != nil {
			return nil, err
		}
		return r, nil
	}
	if hooks.afterBootstrapLock != nil {
		hooks.afterBootstrapLock()
	}
	if err := r.initialize(ctx, hooks); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *ownedRoot) initialize(ctx context.Context, hooks ownershipHooks) (err error) {
	type createdEntry struct {
		name      string
		id        entryIdentity
		directory bool
	}
	var created []createdEntry
	defer func() {
		if err == nil {
			return
		}
		// Never remove the selected root or permanent lock, even on failure.
		// Keep serialization while closing child pins and undoing exact entries.
		err = errors.Join(err, r.staging.Close(), r.generations.Close())
		for _, entry := range slices.Backward(created) {
			err = errors.Join(err, r.root.removeEntry(entry.name, entry.id, entry.directory))
		}
	}()
	if hooks.beforeClaimRecheck != nil {
		hooks.beforeClaimRecheck()
	}
	if err := validatePrivateEntry(r.root.file, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := r.root.entries(ctx, rootEntryMaximum)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != lockName {
		return errEntry
	}
	if err := r.root.sameEntry(lockName, r.lockIdentity); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nativeError(err)
	}
	r.marker = rootMarker{Format: "docbank-qmd-export-root", Version: 1, ID: hex.EncodeToString(nonce[:])}
	encoded, err := json.Marshal(r.marker, json.Deterministic(true))
	if err != nil {
		return errEntry
	}
	f, id, err := r.root.createFile(markerName, stableEntry)
	if err != nil {
		return err
	}
	created = append(created, createdEntry{name: markerName, id: id})
	_, writeErr := f.Write(append(encoded, '\n'))
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if hooks.afterMarker != nil {
		hooks.afterMarker()
	}
	for _, name := range []string{"generations", ".staging"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		d, id, err := r.root.createDir(name, stableEntry)
		if err != nil {
			return err
		}
		created = append(created, createdEntry{name: name, id: id, directory: true})
		if name == "generations" {
			r.generations = d
		} else {
			r.staging = d
		}
	}
	if err := r.root.sameEntry(lockName, r.lockIdentity); err != nil {
		return err
	}
	for _, entry := range created {
		if err := r.root.sameEntry(entry.name, entry.id); err != nil {
			return err
		}
	}
	marker, err := readRootMarker(r.root)
	if err != nil {
		return err
	}
	if marker != r.marker {
		return errIdentity
	}
	if err := confirmInitialization(r); err != nil {
		return err
	}
	return ctx.Err()
}
