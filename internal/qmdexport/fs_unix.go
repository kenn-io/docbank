//go:build linux || darwin

package qmdexport

import (
	"errors"
	"os"
	"syscall"

	"go.kenn.io/kit/safefileio"
	"golang.org/x/sys/unix"
)

func nativeVolumeRoot(volume string) (*anchoredDir, error) {
	fd, err := unix.Open(volume, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	return &anchoredDir{file: os.NewFile(uintptr(fd), "qmd directory")}, nil
}

func nativeOpen(d *anchoredDir, name string, directory, pinned bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, flags, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	f := os.NewFile(uintptr(fd), "qmd entry")
	info, err := f.Stat()
	if err == nil && (info.IsDir() != directory || (!directory && !info.Mode().IsRegular())) {
		err = errEntry
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func nativeReopenDirectory(d *anchoredDir) (*os.File, error) { return nativeOpen(d, ".", true, false) }

func nativeCreate(d *anchoredDir, name string, directory bool, role entryRole) (*os.File, error) {
	if directory {
		if err := unix.Mkdirat(int(d.file.Fd()), name, 0o700); err != nil {
			return nil, nativeError(err)
		}
		return nativeOpen(d, name, true, role == stableEntry)
	}
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, nativeError(err)
	}
	return os.NewFile(uintptr(fd), "qmd entry"), nil
}

func validatePrivateEntry(file *os.File, directory bool) error {
	if !directory {
		if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
			return errPrivate
		}
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0o700 || int64(stat.Uid) != int64(os.Getuid()) {
		return errPrivate
	}
	return nativeDirectoryACL(file)
}

func nativeRemove(d *anchoredDir, name string, identity entryIdentity, directory bool) error {
	if err := d.sameEntry(name, identity); err != nil {
		return err
	}
	flags := 0
	if directory {
		flags = unix.AT_REMOVEDIR
	}
	return nativeError(unix.Unlinkat(int(d.file.Fd()), name, flags))
}

func nativeTryLock(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			return errLockBusy
		}
		return nativeError(err)
	}
}

func nativeUnlock(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		if !errors.Is(err, unix.EINTR) {
			return nativeError(err)
		}
	}
}

func confirmDirectoryEntries(dirs ...*anchoredDir) error {
	var err error
	for _, d := range dirs {
		err = errors.Join(err, d.file.Sync())
	}
	return err
}

func confirmInitialization(r *ownedRoot) error {
	return confirmDirectoryEntries(r.generations, r.staging, r.root, r.ancestors[len(r.ancestors)-1])
}
