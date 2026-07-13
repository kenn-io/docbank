//go:build windows

package home

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func lockFile(file *os.File, exclusive bool) error {
	var flags uint32 = windows.LOCKFILE_FAIL_IMMEDIATELY
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLockWouldBlock
	}
	if err != nil {
		return fmt.Errorf("locking file: %w", err)
	}
	return nil
}

func unlockFile(file *os.File) error {
	err := windows.UnlockFileEx(
		windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		return fmt.Errorf("unlocking file: %w", err)
	}
	return nil
}

func directoryIdentity(info os.FileInfo, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening target-tree ancestor %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	held, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("checking target-tree ancestor %s: %w", path, err)
	}
	if !os.SameFile(info, held) {
		return "", fmt.Errorf("target-tree ancestor %s changed while reading its identity", path)
	}
	var identity windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(file.Fd()), &identity); err != nil {
		return "", fmt.Errorf("reading target-tree identity for %s: %w", path, err)
	}
	return fmt.Sprintf("vol-%x-file-%x%08x",
		identity.VolumeSerialNumber, identity.FileIndexHigh, identity.FileIndexLow), nil
}

func targetLockRegistryBase() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolving current user: %w", err)
	}
	if !filepath.IsAbs(account.HomeDir) {
		return "", fmt.Errorf("current user home is not absolute: %q", account.HomeDir)
	}
	return filepath.Join(account.HomeDir, ".local", "state", "docbank", "target-locks"), nil
}
