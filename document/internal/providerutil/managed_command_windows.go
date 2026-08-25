//go:build windows

package providerutil

import (
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type managedProcessTree struct {
	job      windows.Handle
	attached bool
}

func newManagedProcessTree(_ *exec.Cmd) (*managedProcessTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
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
		return assignErr
	}
	tree.attached = true
	return nil
}

func (tree *managedProcessTree) kill() error {
	if !tree.attached {
		return os.ErrProcessDone
	}
	return windows.TerminateJobObject(tree.job, 1)
}

func (tree *managedProcessTree) close() error {
	if tree.job == 0 {
		return nil
	}
	err := windows.CloseHandle(tree.job)
	tree.job = 0
	tree.attached = false
	return err
}
