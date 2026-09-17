//go:build windows

package providerutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestManagedProcessTreeWindowsErrorBoundaries(t *testing.T) {
	t.Run("kill preserves invalid handle", func(t *testing.T) {
		tree := &managedProcessTree{attached: true}
		if err := tree.kill(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatalf("kill error = %v, want ERROR_INVALID_HANDLE", err)
		}
	})

	t.Run("protected close clears state", func(t *testing.T) {
		const protectFromClose uint32 = 0x00000002
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = windows.SetHandleInformation(job, protectFromClose, 0)
			_ = windows.CloseHandle(job)
		})
		if err := windows.SetHandleInformation(job, protectFromClose, protectFromClose); err != nil {
			t.Fatal(err)
		}

		tree := &managedProcessTree{job: job, attached: true}
		if err := tree.close(); !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			t.Fatalf("close error = %v, want ERROR_INVALID_HANDLE", err)
		}
		if tree.job != 0 || tree.attached {
			t.Fatalf("tree state after failed close = job %v, attached %v", tree.job, tree.attached)
		}
		if err := tree.close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})

	t.Run("unattached kill returns process done", func(t *testing.T) {
		tree := &managedProcessTree{}
		if err := tree.kill(); !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("kill error = %v, want os.ErrProcessDone", err)
		}
	})

	t.Run("real job closes twice", func(t *testing.T) {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		tree := &managedProcessTree{job: job}
		t.Cleanup(func() { _ = tree.close() })
		if err := tree.close(); err != nil {
			t.Fatalf("first close: %v", err)
		}
		if err := tree.close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})
}

func TestManagedCommandStartsWindowsProcessSuspended(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "heartbeat")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable,
		"-test.run=^TestManagedCommandStopsOnCancellation$", "--", "process", marker)
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
