package qmdexport

import (
	"os"
)

// Windows confirmation flushes regular files, including the retained writable
// pointer after replacement. It does not emulate Unix directory fsync.
func confirmStage(ledger entryLedger) error {
	// Stage files have already been flushed and closed by writeStageFile.
	// Revalidate held directory identities rather than claim a directory flush.
	for _, entry := range ledger {
		if entry.dir != nil {
			if err := heldIdentity(entry.dir.file, entry.id); err != nil {
				return err
			}
		}
	}
	return nil
}
func confirmPlacement(root *ownedRoot) error { return root.root.sameEntry(lockName, root.lockIdentity) }
func confirmPublication(_ *ownedRoot, pointer *os.File, hooks publishHooks) error {
	return syncPublicationFile(pointer, hooks)
}
func confirmCleanup(root *ownedRoot) error { return root.root.sameEntry(lockName, root.lockIdentity) }
