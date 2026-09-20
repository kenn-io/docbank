package emailpdf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"uuid"
)

var ErrUnavailable = errors.New("email PDF unavailable")

// CheckAvailable exercises the same user service and sandbox as rendering,
// without opening an email or invoking Chromium.
func (r *Runtime) CheckAvailable(ctx context.Context) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("%w: enable cgroup v2 on the Linux daemon host: %w", ErrUnavailable, err)
	}
	manager := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=ControlGroup", "--value")
	manager.Env = userManagerEnv()
	manager.WaitDelay = time.Second
	var group, diagnostic runtimeDiagnostic
	manager.Stdout, manager.Stderr = &group, &diagnostic
	if err := manager.Run(); err != nil {
		return fmt.Errorf("%w: start a reachable systemd user manager and provide its XDG_RUNTIME_DIR and DBUS_SESSION_BUS_ADDRESS to the daemon (%s): %w", ErrUnavailable, strings.TrimSpace(diagnostic.String()), err)
	}
	groupPath := strings.TrimPrefix(strings.TrimSpace(group.String()), "/")
	if groupPath == "" || !filepath.IsLocal(groupPath) {
		return fmt.Errorf("%w: systemd user manager did not report its cgroup", ErrUnavailable)
	}
	controllers, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", groupPath, "cgroup.controllers"))
	if err != nil || !slices.Contains(strings.Fields(string(controllers)), "memory") || !slices.Contains(strings.Fields(string(controllers)), "pids") {
		return fmt.Errorf("%w: delegate cgroup v2 memory and pids controllers to the systemd user manager", ErrUnavailable)
	}
	dir, err := os.MkdirTemp(r.config.Spool, "email-pdf-probe-")
	if err != nil {
		return fmt.Errorf("%w: make the email PDF spool writable: %w", ErrUnavailable, err)
	}
	cleanupSafe := true
	defer func() {
		if cleanupSafe {
			if err := os.RemoveAll(dir); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("%w: remove email PDF probe staging: %w", ErrUnavailable, err))
			}
		}
	}()
	unit := "docbank-email-pdf-" + uuid.New().String() + ".service"
	if err := os.WriteFile(filepath.Join(dir, unitMarker), []byte(unit), 0o600); err != nil {
		return fmt.Errorf("%w: write the email PDF probe marker: %w", ErrUnavailable, err)
	}
	cleanupSafe = false
	command := r.command(ctx, dir, unit, "/usr/bin/true")
	diagnostic.Reset()
	command.Stderr = &diagnostic
	err = command.Run()
	if cleanupErr := stopUnit(context.Background(), unit); cleanupErr != nil {
		return fmt.Errorf("%w: could not confirm probe service cleanup; restore access to the systemd user manager and restart the daemon: %w", ErrUnavailable, cleanupErr)
	}
	cleanupSafe = true
	if err = errors.Join(err, ctx.Err()); err != nil {
		return fmt.Errorf("%w: allow unprivileged bubblewrap user/network namespaces and systemd user services with delegated memory/pids limits (%s): %w", ErrUnavailable, strings.TrimSpace(diagnostic.String()), err)
	}
	return nil
}
