package providerutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// ManagedCommand gives a CommandContext the platform process scope used for
// cancellation and cleanup.
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

// Run starts the command, attaches its process scope, and waits for it.
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
		attachErr = errors.Join(attachErr, managed.killLocked())
	}
	managed.mu.Unlock()

	waitErr := managed.command.Wait()
	cleanupErr := managed.finish()
	if attachErr != nil {
		attachErr = fmt.Errorf("attach managed process tree: %w", attachErr)
	}
	return errors.Join(attachErr, waitErr, cleanupErr)
}

// Kill terminates the command through its platform process scope.
func (managed *ManagedCommand) Kill() error {
	managed.mu.Lock()
	defer managed.mu.Unlock()
	return managed.killLocked()
}

func (managed *ManagedCommand) killLocked() error {
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
