//go:build darwin || linux

package providerutil

import (
	"os"
	"os/exec"
)

// Unix has no portable kernel-enforced process-tree boundary. The managed
// command therefore owns only the exact child process held by exec.Cmd. Local
// provider bridges are operator-pinned and must not daemonize.
type managedProcessTree struct{}

func newManagedProcessTree(_ *exec.Cmd) (*managedProcessTree, error) {
	return &managedProcessTree{}, nil
}

func (*managedProcessTree) attach(_ *os.Process) error {
	return nil
}

func (*managedProcessTree) kill() error {
	return os.ErrProcessDone
}

func (*managedProcessTree) close() error {
	return nil
}
