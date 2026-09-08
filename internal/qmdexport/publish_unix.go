//go:build linux || darwin

package qmdexport

import (
	"errors"
	"os"
)

func confirmStage(ledger entryLedger) error {
	var err error
	for _, entry := range ledger {
		if entry.dir != nil {
			err = errors.Join(err, confirmDirectoryEntries(entry.dir))
		}
	}
	return nativeError(err)
}

func confirmPlacement(root *ownedRoot) error {
	return nativeError(confirmDirectoryEntries(root.generations, root.staging))
}
func confirmPublication(root *ownedRoot, pointer *os.File, hooks publishHooks) error {
	return nativeError(errors.Join(syncPublicationFile(pointer, hooks), confirmDirectoryEntries(root.root)))
}
func confirmCleanup(root *ownedRoot) error {
	return nativeError(confirmDirectoryEntries(root.generations, root.staging))
}
