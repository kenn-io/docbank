package packagetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if os.Getenv("DOCBANK_LOCK_REGISTRY_TEST_CHILD") == "1" {
		if os.Getenv("DOCBANK_LOCK_REGISTRY_TEST_REMOVE_FAIL") == "1" {
			os.Exit(runWithLockRegistry(m, func(string) error { return os.ErrPermission }))
		}
		os.Exit(RunWithLockRegistry(m))
	}
	os.Exit(m.Run())
}

func TestLockRegistryChild(t *testing.T) {
	if os.Getenv("DOCBANK_LOCK_REGISTRY_TEST_CHILD") != "1" {
		return
	}
	dir := os.Getenv(LockDirEnv)
	require.NotEmpty(t, dir)
	require.Equal(t, dir, os.Getenv(inheritedLockDirEnv))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
	require.NoError(t, os.WriteFile(os.Getenv("DOCBANK_LOCK_REGISTRY_TEST_RESULT"), []byte(dir), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "synthetic.lock"), nil, 0o600))
	if os.Getenv("DOCBANK_LOCK_REGISTRY_TEST_FAIL") == "1" {
		t.Fatal("synthetic test failure")
	}
}

func TestRunWithLockRegistry(t *testing.T) {
	t.Setenv("docbank_lock_dir", t.TempDir())
	executable, err := os.Executable()
	require.NoError(t, err)
	newCommand := func(result string) *exec.Cmd {
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestLockRegistryChild$")
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if !strings.EqualFold(key, LockDirEnv) && !strings.EqualFold(key, inheritedLockDirEnv) {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "DOCBANK_LOCK_REGISTRY_TEST_CHILD=1", "DOCBANK_LOCK_REGISTRY_TEST_RESULT="+result)
		return command
	}
	for _, mode := range []string{"success", "failure", "cleanup-failure", "test-and-cleanup-failure", "ambient", "ambient-lowercase", "mismatched", "inherited"} {
		t.Run(mode, func(t *testing.T) {
			result := filepath.Join(t.TempDir(), "registry-path")
			command := newCommand(result)
			for _, entry := range command.Env {
				key, _, _ := strings.Cut(entry, "=")
				require.False(t, strings.EqualFold(key, LockDirEnv))
			}
			var inherited string
			if mode == "inherited" || mode == "ambient" || mode == "ambient-lowercase" || mode == "mismatched" {
				inherited = t.TempDir()
				key := LockDirEnv
				if mode == "ambient-lowercase" {
					key = strings.ToLower(key)
				}
				command.Env = append(command.Env, key+"="+inherited)
			}
			if mode == "inherited" {
				command.Env = append(command.Env, inheritedLockDirEnv+"="+inherited)
			}
			if mode == "mismatched" {
				command.Env = append(command.Env, inheritedLockDirEnv+"="+t.TempDir())
			}
			cleanupFailure := mode == "cleanup-failure" || mode == "test-and-cleanup-failure"
			if cleanupFailure {
				command.Env = append(command.Env, "DOCBANK_LOCK_REGISTRY_TEST_REMOVE_FAIL=1")
			}
			if mode == "failure" || mode == "test-and-cleanup-failure" {
				command.Env = append(command.Env, "DOCBANK_LOCK_REGISTRY_TEST_FAIL=1")
			}
			output, err := command.CombinedOutput()
			if mode == "failure" || cleanupFailure {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, string(output))
				require.Equal(t, 1, exitErr.ExitCode())
			} else {
				require.NoError(t, err, string(output))
			}
			data, err := os.ReadFile(result)
			require.NoError(t, err)
			if cleanupFailure {
				t.Cleanup(func() { require.NoError(t, os.RemoveAll(string(data))) })
				require.Contains(t, string(output), "removing test lock registry:")
				require.DirExists(t, string(data))
			} else if mode == "inherited" {
				require.Equal(t, inherited, string(data))
				require.FileExists(t, filepath.Join(inherited, "synthetic.lock"))
			} else {
				require.NotEqual(t, inherited, string(data))
				require.NoDirExists(t, string(data))
				if inherited != "" {
					entries, err := os.ReadDir(inherited)
					require.NoError(t, err)
					require.Empty(t, entries)
				}
			}
		})
	}
}
