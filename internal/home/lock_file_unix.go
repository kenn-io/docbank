//go:build !windows

package home

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

func lockFile(file *os.File, exclusive bool) error {
	how := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(file.Fd()), how)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return errLockWouldBlock
		default:
			if err != nil {
				return fmt.Errorf("locking file: %w", err)
			}
			return nil
		}
	}
}

func unlockFile(file *os.File) error {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		if !errors.Is(err, syscall.EINTR) {
			if err != nil {
				return fmt.Errorf("unlocking file: %w", err)
			}
			return nil
		}
	}
}

func directoryIdentity(info os.FileInfo, path string) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("reading target-tree identity for %s", path)
	}
	return fmt.Sprintf("dev-%x-ino-%x", stat.Dev, stat.Ino), nil
}

func targetLockRegistryBase() (string, error) {
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		return "", fmt.Errorf("resolving effective user: %w", err)
	}
	if !filepath.IsAbs(account.HomeDir) {
		return "", fmt.Errorf("effective user home is not absolute: %q", account.HomeDir)
	}
	return filepath.Join(account.HomeDir, ".local", "state", "docbank", "target-locks"), nil
}
