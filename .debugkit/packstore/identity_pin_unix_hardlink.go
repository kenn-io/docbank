//go:build unix && !linux

package packstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/kit/pack"
)

// hardlinkIdentityPin retains an inode reference without opening its content.
// The link lives in an exclusive same-directory namespace so creation is on
// the same filesystem and cleanup never broadens beyond exact-owned paths.
type hardlinkIdentityPin struct {
	path     string
	dir      string
	identity fs.FileInfo
	closed   bool
}

func (p *hardlinkIdentityPin) Stat() (fs.FileInfo, error) {
	if p.closed {
		return nil, os.ErrClosed
	}
	current, err := os.Lstat(p.path)
	if err != nil {
		return nil, err
	}
	if p.identity == nil {
		return nil, fmt.Errorf("%w: loose identity pin %s has no captured identity", errIdentityChanged, p.path)
	}
	if err := validateRegularNoFollow(p.path, current); err != nil {
		return nil, err
	}
	if !os.SameFile(p.identity, current) {
		return nil, fmt.Errorf("%w: loose identity pin %s changed", errIdentityChanged, p.path)
	}
	return current, nil
}

func (p *hardlinkIdentityPin) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	current, statErr := os.Lstat(p.path)
	if errors.Is(statErr, fs.ErrNotExist) {
		statErr = nil
	} else if statErr == nil && p.identity != nil && !os.SameFile(p.identity, current) {
		statErr = fmt.Errorf("%w: loose identity pin %s changed before cleanup", errIdentityChanged, p.path)
	}
	var removeErr error
	if statErr == nil && current != nil {
		removeErr = os.Remove(p.path)
	}
	dirErr := os.Remove(p.dir)
	if errors.Is(dirErr, fs.ErrNotExist) {
		dirErr = nil
	}
	return errors.Join(statErr, removeErr, dirErr)
}

func (p *hardlinkIdentityPin) removeClaim(path string) error {
	return removeLooseCanonicalFile(path)
}

// Unix platforms without O_PATH first use the ordinary readable no-follow
// descriptor. Permission-denied files fall back to an exclusive hard link,
// which pins the inode while preserving unlink-as-parent-directory semantics.
func openLooseRemovalIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	file, identity, err := openLooseRepairPin(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		if err != nil {
			return nil, nil, err
		}
		return &fileIdentityPin{File: file}, identity, nil
	}
	return openHardlinkIdentityPin(path)
}

func openHardlinkIdentityPin(path string) (identityPin, fs.FileInfo, error) {
	dir, err := makeHardlinkIdentityPinDir(path)
	if err != nil {
		return nil, nil, err
	}
	pinPath := filepath.Join(dir, "pinned")
	if err := os.Link(path, pinPath); err != nil {
		return nil, nil, errors.Join(
			fmt.Errorf("packstore: hard-link identity pin for %s: %w", path, err),
			os.Remove(dir),
		)
	}
	pinIdentity, pinErr := os.Lstat(pinPath)
	sourceIdentity, sourceErr := os.Lstat(path)
	pin := &hardlinkIdentityPin{path: pinPath, dir: dir, identity: pinIdentity}
	if pinErr == nil {
		pinErr = validateRegularNoFollow(pinPath, pinIdentity)
	}
	if sourceErr == nil {
		sourceErr = validateRegularNoFollow(path, sourceIdentity)
	}
	if identityErr := errors.Join(pinErr, sourceErr); identityErr != nil {
		return nil, nil, errors.Join(identityErr, pin.Close())
	}
	if pinIdentity == nil || sourceIdentity == nil {
		return nil, nil, errors.Join(fmt.Errorf("packstore: hard-link identity pin for %s has incomplete state", path), pin.Close())
	}
	if !os.SameFile(pinIdentity, sourceIdentity) {
		return nil, nil, errors.Join(errIdentityChanged, pin.Close())
	}
	return pin, pinIdentity, nil
}

func makeHardlinkIdentityPinDir(path string) (string, error) {
	const attempts = 8
	for range attempts {
		dir := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".pin-"+pack.NewPackID())
		if err := os.Mkdir(dir, 0o700); err == nil {
			return dir, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("packstore: create identity pin directory for %s: %w", path, err)
		}
	}
	return "", fmt.Errorf("packstore: create unique identity pin directory for %s: %w", path, fs.ErrExist)
}
