package providerutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// ManagedCommand runs a CommandContext in an isolated process tree and tears
// down every process in that tree when the command exits or is canceled.
type ManagedCommand struct {
	mu       sync.Mutex
	command  *exec.Cmd
	tree     *managedProcessTree
	started  bool
	finished bool
}

// NewManagedCommand prepares a CommandContext for process-tree cleanup.
func NewManagedCommand(command *exec.Cmd) (*ManagedCommand, error) {
	if command == nil {
		return nil, errors.New("managed command is required")
	}
	tree, err := newManagedProcessTree(command)
	if err != nil {
		return nil, fmt.Errorf("prepare managed process tree: %w", err)
	}
	managed := &ManagedCommand{command: command, tree: tree}
	command.Cancel = managed.Kill
	return managed, nil
}

// Run starts the command, attaches it to its process tree, waits for it, and
// terminates any descendants that remain after the direct child exits.
func (managed *ManagedCommand) Run() error {
	managed.mu.Lock()
	if managed.started {
		managed.mu.Unlock()
		return errors.New("managed command already started")
	}
	managed.started = true
	if err := managed.command.Start(); err != nil {
		managed.finished = true
		cleanupErr := managed.tree.close()
		managed.mu.Unlock()
		return errors.Join(err, cleanupErr)
	}
	attachErr := managed.tree.attach(managed.command.Process)
	if attachErr != nil {
		_ = managed.command.Process.Kill()
	}
	managed.mu.Unlock()

	waitErr := managed.command.Wait()
	cleanupErr := managed.finish()
	if attachErr != nil {
		attachErr = fmt.Errorf("attach managed process tree: %w", attachErr)
	}
	return errors.Join(attachErr, waitErr, cleanupErr)
}

// Kill terminates the command and every descendant in its managed process tree.
func (managed *ManagedCommand) Kill() error {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if !managed.started || managed.finished || managed.command.Process == nil {
		return os.ErrProcessDone
	}
	treeErr := managed.tree.kill()
	processErr := managed.command.Process.Kill()
	if treeErr == nil || errors.Is(treeErr, os.ErrProcessDone) && processErr == nil {
		return nil
	}
	if processErr == nil || errors.Is(processErr, os.ErrProcessDone) {
		return treeErr
	}
	if errors.Is(treeErr, os.ErrProcessDone) {
		return processErr
	}
	return errors.Join(treeErr, processErr)
}

func (managed *ManagedCommand) finish() error {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	if managed.finished {
		return nil
	}
	managed.finished = true
	return managed.tree.close()
}
