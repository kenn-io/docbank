package packagetest

import (
	"fmt"
	"os"
	"testing"
)

// LockDirEnv names the target-lock registry override that test processes share.
const LockDirEnv = "DOCBANK_LOCK_DIR"

// RunWithLockRegistry runs m with a fresh target-lock registry. The account
// registry grows with every test vault, and Windows rewrites its ACL on each
// lock, so sharing it makes every lock slower. Children launched with the
// inherited environment keep the parent's registry, so they still contend.
func RunWithLockRegistry(m *testing.M) int {
	if os.Getenv(LockDirEnv) != "" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "docbank-test-locks-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating test lock registry: %v\n", err)
		return 1
	}
	if err := os.Setenv(LockDirEnv, dir); err != nil {
		fmt.Fprintf(os.Stderr, "setting %s: %v\n", LockDirEnv, err)
		return 1
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	return code
}
