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
	for _, tc := range []struct {
		name        string
		ambientKey  string
		marker      string
		removeFails bool
		testFails   bool
	}{
		{name: "success"},
		{name: "failure", testFails: true},
		{name: "cleanup-failure", removeFails: true},
		{name: "ambient/missing-marker", ambientKey: LockDirEnv},
		{name: "ambient/mismatched-marker", ambientKey: LockDirEnv, marker: "mismatched"},
		{name: "inherited", ambientKey: LockDirEnv, marker: "matching"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := filepath.Join(t.TempDir(), "registry-path")
			command := newCommand(result)
			var inherited string
			if tc.ambientKey != "" {
				inherited = t.TempDir()
				command.Env = append(command.Env, tc.ambientKey+"="+inherited)
			}
			switch tc.marker {
			case "matching":
				command.Env = append(command.Env, inheritedLockDirEnv+"="+inherited)
			case "mismatched":
				command.Env = append(command.Env, inheritedLockDirEnv+"="+t.TempDir())
			}
			if tc.removeFails {
				command.Env = append(command.Env, "DOCBANK_LOCK_REGISTRY_TEST_REMOVE_FAIL=1")
			}
			if tc.testFails {
				command.Env = append(command.Env, "DOCBANK_LOCK_REGISTRY_TEST_FAIL=1")
			}
			output, err := command.CombinedOutput()
			if tc.testFails || tc.removeFails {
				var exitErr *exec.ExitError
				require.ErrorAs(t, err, &exitErr, string(output))
				require.Equal(t, 1, exitErr.ExitCode())
			} else {
				require.NoError(t, err, string(output))
			}
			data, err := os.ReadFile(result)
			require.NoError(t, err)
			if tc.removeFails {
				t.Cleanup(func() { require.NoError(t, os.RemoveAll(string(data))) })
				require.Contains(t, string(output), "removing test lock registry:")
				require.DirExists(t, string(data))
			} else if tc.marker == "matching" {
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
