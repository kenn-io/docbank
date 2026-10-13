package packagetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shirou/gopsutil/v4/process"
)

// LockDirEnv names the target-lock registry override shared with child processes.
const LockDirEnv = "DOCBANK_LOCK_DIR"

const inheritedLockDirEnv = "DOCBANK_TEST_LOCK_DIR"

const lockDirPrefix = "docbank-test-locks-"

// RunWithLockRegistry isolates test locks and lets children inherit their parent's registry.
func RunWithLockRegistry(m *testing.M) int {
	if dir := os.Getenv(LockDirEnv); dir != "" && os.Getenv(inheritedLockDirEnv) == dir {
		return m.Run()
	}
	removeAbandonedLockRegistries()
	dir, err := os.MkdirTemp("", fmt.Sprintf("%s%d-", lockDirPrefix, os.Getpid()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating test lock registry: %v\n", err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "removing test lock registry: %v\n", err)
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

func removeAbandonedLockRegistries() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading test lock registries: %v\n", err)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), lockDirPrefix) {
			continue
		}
		pidText, suffix, ok := strings.Cut(strings.TrimPrefix(entry.Name(), lockDirPrefix), "-")
		pid, err := strconv.ParseInt(pidText, 10, 32)
		if !ok || suffix == "" || err != nil || pid <= 0 {
			continue
		}
		// PID reuse and uncertain liveness keep the directory safe for active owners.
		alive, err := process.PidExists(int32(pid))
		if err != nil || alive {
			continue
		}
		if err := os.RemoveAll(filepath.Join(os.TempDir(), entry.Name())); err != nil {
			fmt.Fprintf(os.Stderr, "removing abandoned test lock registry: %v\n", err)
		}
	}
}
