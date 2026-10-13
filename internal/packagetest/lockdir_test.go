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
	executable, err := os.Executable()
	require.NoError(t, err)
	for _, mode := range []string{"success", "failure", "inherited"} {
		t.Run(mode, func(t *testing.T) {
			result := filepath.Join(t.TempDir(), "registry-path")
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestLockRegistryChild$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, LockDirEnv+"=") {
					command.Env = append(command.Env, entry)
				}
			}
			command.Env = append(command.Env, "DOCBANK_LOCK_REGISTRY_TEST_CHILD=1", "DOCBANK_LOCK_REGISTRY_TEST_RESULT="+result)
			var inherited string
			if mode == "inherited" {
				inherited = t.TempDir()
				command.Env = append(command.Env, LockDirEnv+"="+inherited)
			}
			if mode == "failure" {
				command.Env = append(command.Env, "DOCBANK_LOCK_REGISTRY_TEST_FAIL=1")
			}
			output, err := command.CombinedOutput()
			if mode == "failure" {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, string(output))
				require.Equal(t, 1, exitErr.ExitCode())
			} else {
				require.NoError(t, err, string(output))
			}
			data, err := os.ReadFile(result)
			require.NoError(t, err)
			if mode == "inherited" {
				require.Equal(t, inherited, string(data))
				require.FileExists(t, filepath.Join(inherited, "synthetic.lock"))
			} else {
				require.NoDirExists(t, string(data))
			}
		})
	}
}
