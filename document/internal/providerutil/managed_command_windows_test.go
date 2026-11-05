//go:build windows

package providerutil

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"testing"
	"unsafe"

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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stdinReader, _ := testPipe(t)
	stdoutReader, stdoutWriter := testPipe(t)
	command := exec.CommandContext(t.Context(), executable,
		"-test.run=^TestManagedCommandStopsOnCancellation$", "--", "block")
	command.Stdin = stdinReader
	command.Stdout = stdoutWriter
	tree, err := newManagedProcessTree(command)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		_ = tree.close()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() {
		exited <- command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = tree.kill()
		_ = command.Process.Kill()
		<-exited
		_ = tree.close()
	})

	// A thread created suspended reports a suspend count of at least one. A
	// count of zero means the helper could already run outside the Job Object.
	if count := primaryThreadSuspendCount(t, command.Process.Pid); count == 0 {
		t.Fatal("process ran before Job Object assignment")
	}
	if err := tree.attach(command.Process); err != nil {
		t.Fatal(err)
	}
	waitForHelperReady(t, stdoutReader, exited)
}

var procSuspendThread = windows.NewLazySystemDLL("kernel32.dll").NewProc("SuspendThread")

// primaryThreadSuspendCount reads the suspend count of the process's thread
// by suspending it once more and then undoing that suspension.
func primaryThreadSuspendCount(t *testing.T, pid int) uint32 {
	t.Helper()
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) { // #nosec G115 -- pid comes from the uint32 Windows process ID returned by os.Process.
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = windows.CloseHandle(thread) }()
		previous, _, suspendErr := procSuspendThread.Call(uintptr(thread))
		if uint32(previous) == math.MaxUint32 {
			t.Fatalf("suspend helper thread: %v", suspendErr)
		}
		if _, err := windows.ResumeThread(thread); err != nil {
			t.Fatalf("restore helper thread suspend count: %v", err)
		}
		return uint32(previous)
	}
	t.Fatalf("helper process thread was not found: %v", err)
	return 0
}
