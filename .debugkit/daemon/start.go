package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// StartDetachedOptions configures StartDetached.
type StartDetachedOptions struct {
	Executable      string
	Args            []string
	Dir             string
	Env             []string
	Stdout          io.Writer
	Stderr          io.Writer
	RefuseEphemeral bool
	AfterStart      func(*exec.Cmd)
	// Exited, when set, receives the child's exit result once the child
	// exits. A caller waiting for the daemon to become reachable uses it to
	// stop waiting for a child that has already died.
	Exited func(error)
}

// StartDetached starts a child process detached from the caller's process
// group where the platform supports it. On Windows the child starts without a
// console so terminal probes cannot block waiting for console input.
func StartDetached(ctx context.Context, opts StartDetachedOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe := opts.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return err
		}
	}
	if opts.RefuseEphemeral && IsEphemeralExecutable(exe) {
		return fmt.Errorf("refusing to auto-start daemon from ephemeral binary %s", filepath.Base(exe))
	}
	cmd := exec.Command(exe, opts.Args...) //nolint:forbidigo,noctx // the detached daemon must outlive the caller's context
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	detachChild(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached daemon: %w", err)
	}
	if opts.AfterStart != nil {
		opts.AfterStart(cmd)
	}
	go func() {
		err := cmd.Wait()
		if opts.Exited != nil {
			opts.Exited(err)
		}
	}()
	return nil
}

// IsEphemeralExecutable reports whether exe looks like a test or go-build
// binary that should not be used as a long-lived daemon.
func IsEphemeralExecutable(exe string) bool {
	base := filepath.Base(exe)
	lowerBase := strings.ToLower(base)
	if withoutExe, ok := strings.CutSuffix(lowerBase, ".exe"); ok {
		lowerBase = withoutExe
	}
	normalized := strings.ReplaceAll(exe, `\`, `/`)
	return strings.HasSuffix(lowerBase, ".test") || strings.Contains(normalized, "/go-build")
}
