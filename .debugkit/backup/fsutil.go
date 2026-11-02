package backup

import (
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// publishFile publishes a repo object; tests replace it to inject failures.
var publishFile = atomicfile.WriteFile

// writeFileAtomic publishes data at finalRel (relative to the repo root)
// via staging -> fsync -> rename -> parent dir sync, so a crash never
// leaves a partially written repo object at its final path. The staging
// file lives in the repo's staging directory, so CleanStaging sweeps any
// debris a crash leaves behind.
func writeFileAtomic(r *Repo, finalRel string, data []byte) error {
	final := r.Path(finalRel)
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return fmt.Errorf("backup: creating parent dir for %s: %w", finalRel,
			err)
	}
	if err := publishFile(final, data,
		atomicfile.WithStagingDir(r.Path(stagingDirName)),
		atomicfile.WithPerm(0o600),
	); err != nil {
		return fmt.Errorf("backup: publishing %s: %w", finalRel, err)
	}
	return nil
}
