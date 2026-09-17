//go:build windows

package providerutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type managedProcessTree struct {
	job      windows.Handle
	attached bool
}

func newManagedProcessTree(command *exec.Cmd) (*managedProcessTree, error) {
	attributes := &syscall.SysProcAttr{}
	if command.SysProcAttr != nil {
		copied := *command.SysProcAttr
		attributes = &copied
	}
	attributes.CreationFlags |= windows.CREATE_SUSPENDED
	command.SysProcAttr = attributes
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create process job object: %w", err)
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)), // #nosec G103 -- Windows requires the address of this job information structure.
		uint32(unsafe.Sizeof(information)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure process job object: %w", err)
	}
	return &managedProcessTree{job: job}, nil
}

func (tree *managedProcessTree) attach(process *os.Process) error {
	var assignErr error
	err := process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(tree.job, windows.Handle(handle))
	})
	if err != nil {
		return err
	}
	if assignErr != nil {
		return fmt.Errorf("assign process to job object: %w", assignErr)
	}
	tree.attached = true
	return resumeProcess(process.Pid)
}

func resumeProcess(pid int) (result error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot suspended process threads: %w", err)
	}
	defer func() {
		if err := windows.CloseHandle(snapshot); err != nil {
			result = errors.Join(result, fmt.Errorf("close thread snapshot: %w", err))
		}
	}()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("read suspended process threads: %w", err)
	}
	for {
		if entry.OwnerProcessID == uint32(pid) { // #nosec G115 -- pid comes from the uint32 Windows process ID returned by os.Process.
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open suspended process thread: %w", err)
			}
			previousCount, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil || previousCount == 0 {
				var suspendedErr error
				if previousCount == 0 {
					suspendedErr = errors.New("process thread was not suspended")
				}
				if closeErr != nil {
					closeErr = fmt.Errorf("close suspended process thread: %w", closeErr)
				}
				return errors.Join(errors.New("resume suspended process thread"),
					resumeErr, suspendedErr, closeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close suspended process thread: %w", closeErr)
			}
			return nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return errors.New("suspended process thread was not found")
			}
			return fmt.Errorf("read suspended process threads: %w", err)
		}
	}
}

func (tree *managedProcessTree) kill() error {
	if !tree.attached {
		return os.ErrProcessDone
	}
	if err := windows.TerminateJobObject(tree.job, 1); err != nil {
		return fmt.Errorf("terminate process job object: %w", err)
	}
	return nil
}

func (tree *managedProcessTree) close() error {
	if tree.job == 0 {
		return nil
	}
	err := windows.CloseHandle(tree.job)
	tree.job = 0
	tree.attached = false
	if err != nil {
		return fmt.Errorf("close process job object: %w", err)
	}
	return nil
}
