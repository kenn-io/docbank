//go:build unix

package providerutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type managedProcessTree struct {
	processGroupID int
}

func newManagedProcessTree(command *exec.Cmd) (*managedProcessTree, error) {
	attributes := &syscall.SysProcAttr{}
	if command.SysProcAttr != nil {
		copied := *command.SysProcAttr
		attributes = &copied
	}
	attributes.Setpgid = true
	attributes.Pgid = 0
	command.SysProcAttr = attributes
	return &managedProcessTree{}, nil
}

func (tree *managedProcessTree) attach(process *os.Process) error {
	tree.processGroupID = process.Pid
	return nil
}

func (tree *managedProcessTree) kill() error {
	if tree.processGroupID <= 0 {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-tree.processGroupID, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return fmt.Errorf("kill process group: %w", err)
	}
	return nil
}

func (tree *managedProcessTree) close() error {
	err := tree.kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
