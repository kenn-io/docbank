package packagetest

import (
	"fmt"
	"os"
	"testing"
)

// LockDirEnv names the target-lock registry override shared with child processes.
const LockDirEnv = "DOCBANK_LOCK_DIR"

const inheritedLockDirEnv = "DOCBANK_TEST_LOCK_DIR"

// RunWithLockRegistry isolates test locks and lets children inherit their parent's registry.
func RunWithLockRegistry(m *testing.M) int {
	return runWithLockRegistry(m, os.RemoveAll)
}

func runWithLockRegistry(m *testing.M, removeAll func(string) error) (code int) {
	if dir := os.Getenv(LockDirEnv); dir != "" && os.Getenv(inheritedLockDirEnv) == dir {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "docbank-test-locks-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating test lock registry: %v\n", err)
		return 1
	}
	defer func() {
		if code != 0 {
			fmt.Fprintf(os.Stderr, "keeping test lock registry after failure: %s\n", dir)
			return
		}
		if err := removeAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "removing test lock registry: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}()
	if err := os.Setenv(LockDirEnv, dir); err != nil {
		fmt.Fprintf(os.Stderr, "setting %s: %v\n", LockDirEnv, err)
		return 1
	}
	if err := os.Setenv(inheritedLockDirEnv, dir); err != nil {
		fmt.Fprintf(os.Stderr, "setting %s: %v\n", inheritedLockDirEnv, err)
		return 1
	}
	return m.Run()
}
