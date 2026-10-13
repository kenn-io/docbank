package packagetest

import (
	"fmt"
	"os"
	"testing"
)

// LockDirEnv names the target-lock registry override shared with child processes.
const LockDirEnv = "DOCBANK_LOCK_DIR"

// RunWithLockRegistry isolates test locks and lets children inherit their parent's registry.
func RunWithLockRegistry(m *testing.M) int {
	if os.Getenv(LockDirEnv) != "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "docbank-test-locks-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating test lock registry: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.Setenv(LockDirEnv, dir); err != nil {
		fmt.Fprintf(os.Stderr, "setting %s: %v\n", LockDirEnv, err)
		return 1
	}
	return m.Run()
}
