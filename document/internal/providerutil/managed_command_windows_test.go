//go:build windows

package providerutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func detachManagedCommandHelper() error {
	return nil
}

func TestManagedCommandStartsWindowsProcessSuspended(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "heartbeat")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable,
		"-test.run=^TestManagedCommandTerminatesDescendants$", "--", "child", marker)
	tree, err := newManagedProcessTree(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		_ = tree.close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tree.kill()
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = tree.close()
	})

	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("suspended process ran before Job Object assignment: %v", err)
	}
	if err := tree.attach(command.Process); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, marker)
}
