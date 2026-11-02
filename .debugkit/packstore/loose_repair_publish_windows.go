//go:build windows

package packstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"go.kenn.io/kit/atomicfile"
	"golang.org/x/sys/windows"
)

func newLooseRepairBackupPath(final string) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate repair backup path: %w", err)
	}
	return filepath.Join(
		filepath.Dir(final),
		"."+filepath.Base(final)+".repair-backup-"+hex.EncodeToString(suffix[:]),
	), nil
}

var (
	procReplaceFileW           = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")
	replaceLooseFileWindows    = replaceLooseFileWindowsAPI
	linkLooseRepairFileWindows = os.Link
)

// replaceLooseRepairFile uses ReplaceFileW when the canonical name exists.
// ReplaceFileW preserves handles opened with FILE_SHARE_DELETE, as Kit's
// no-follow reader does. If the target is absent, publication uses a hard link
// or an atomic no-replace move on filesystems without hard-link support. Losing
// either creation race returns to ReplaceFileW rather than remove-then-rename.
// The absent-target hard link may leave staging for caller-owned cleanup.
func replaceLooseRepairFile(staging, final string, verified fs.FileInfo) (looseRepairPublishResult, error) {
	backup, err := newLooseRepairBackupPath(final)
	if err != nil {
		return looseRepairPublishResult{KeepStaging: true, SyncStaging: true}, err
	}
	err = replaceLooseFileWindows(staging, final, backup)
	if err == nil {
		return looseRepairPublishResult{Created: true, SyncShard: true}, cleanupLooseRepairBackup(backup, true)
	}
	if !isWindowsNotExist(err) {
		return reconcileLooseRepairReplacement(staging, final, backup, verified, err)
	}
	linkErr := linkLooseRepairFileWindows(staging, final)
	if linkErr == nil {
		return looseRepairPublishResult{Created: true, SyncShard: true}, nil
	}
	if !isWindowsExist(linkErr) {
		renameErr := atomicfile.RenameNoReplace(staging, final)
		if renameErr == nil {
			return looseRepairPublishResult{Created: true, SyncShard: true}, nil
		}
		if !isWindowsExist(renameErr) {
			return reconcileLooseRepairReplacement(staging, final, backup, verified, errors.Join(
				fmt.Errorf("hard-link loose repair publication: %w", linkErr),
				fmt.Errorf("no-replace rename loose repair publication: %w", renameErr),
			))
		}
	}
	err = replaceLooseFileWindows(staging, final, backup)
	if err == nil {
		return looseRepairPublishResult{Created: true, SyncShard: true}, cleanupLooseRepairBackup(backup, true)
	}
	return reconcileLooseRepairReplacement(staging, final, backup, verified, err)
}

func isWindowsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func isWindowsExist(err error) bool {
	return errors.Is(err, fs.ErrExist) ||
		errors.Is(err, windows.ERROR_FILE_EXISTS) ||
		errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

func replaceLooseFileWindowsAPI(staging, final, backup string) error {
	replacedName, err := windows.UTF16PtrFromString(final)
	if err != nil {
		return err
	}
	replacementName, err := windows.UTF16PtrFromString(staging)
	if err != nil {
		return err
	}
	backupName, err := windows.UTF16PtrFromString(backup)
	if err != nil {
		return err
	}
	r1, _, callErr := procReplaceFileW.Call(
		uintptr(unsafe.Pointer(replacedName)),
		uintptr(unsafe.Pointer(replacementName)),
		uintptr(unsafe.Pointer(backupName)),
		0,
		0,
		0,
	)
	if r1 != 0 {
		return nil
	}
	if errors.Is(callErr, syscall.Errno(0)) {
		callErr = syscall.EINVAL
	}
	return &os.LinkError{Op: "ReplaceFileW", Old: staging, New: final, Err: callErr}
}
