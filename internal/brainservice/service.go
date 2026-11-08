package brainservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

type Status struct {
	ID, State, ProblemCode    string
	Installed, Running, Owned bool
}

// Runner executes a structured argv, never a shell command. Implementations
// must honor cancellation and bound output; the default caps it at 64 KiB.
type Runner func(context.Context, string, ...string) ([]byte, error)
type Driver interface {
	Install(ctx context.Context, spec Spec) (Status, error)
	InstallChecked(ctx context.Context, spec Spec, check func(context.Context) error) (Status, error)
	Status(ctx context.Context, spec Spec) (Status, error)
	Uninstall(ctx context.Context, spec Spec) error
	WithAbsent(ctx context.Context, spec Spec, action func(context.Context) error) error
}

var (
	ErrCollision    = errors.New("publisher launcher is not owned by this connection")
	ErrNotInstalled = errors.New("publisher launcher is not registered")
	ErrManager      = errors.New("publisher launcher manager unavailable or response invalid")
	ErrOutputLimit  = errors.New("publisher launcher manager response exceeds limit")
)

const maxDefinition = 64 << 10

// OperationError retains the state observed after a failed or uncertain change.
// Unknown is explicit; Installed=false alone never proves registration absent.
type OperationError struct {
	Status Status
	Cause  error
}

func (e *OperationError) Error() string { return "publisher launcher: " + e.Status.ProblemCode }
func (e *OperationError) Unwrap() error { return e.Cause }

type serviceDriver struct {
	goos      string
	runner    Runner
	taskState func(context.Context, string) (int, error)
	write     func(string, []byte) error
	remove    func(string) error
}

func newServiceDriver(goos string, runner Runner, state func(context.Context, string) (int, error)) *serviceDriver {
	if runner == nil {
		runner = commandRunner
	}
	return &serviceDriver{goos: goos, runner: runner, taskState: state, write: func(name string, b []byte) error { return atomicfile.WriteFile(name, b, atomicfile.WithPrivate()) }, remove: os.Remove}
}
func receiptPath(s Spec, r Registration) string {
	return filepath.Join(s.ConfigRoot, "services", r.ID+".receipt")
}
func (d *serviceDriver) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, err := d.runner(ctx, args[0], args[1:]...)
	if len(output) > maxDefinition {
		return nil, ErrOutputLimit
	}
	return output, errors.Join(err, ctx.Err())
}

type limitedOutput struct{ bytes.Buffer }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > maxDefinition-w.Len() {
		return 0, ErrOutputLimit
	}
	n, err := w.Buffer.Write(p)
	if err != nil {
		return n, fmt.Errorf("buffering launcher manager output: %w", err)
	}
	return n, nil
}
func commandRunner(ctx context.Context, executable string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Only these platform managers are used by the driver. Keep executable
	// selection separate from structured arguments; never invoke a shell.
	var cmd *exec.Cmd
	switch executable {
	case "systemctl":
		cmd = exec.CommandContext(ctx, "systemctl")
	case "launchctl":
		cmd = exec.CommandContext(ctx, "launchctl")
	case "schtasks":
		cmd = exec.CommandContext(ctx, "schtasks")
	default:
		return nil, fmt.Errorf("%w: unknown launcher manager", ErrManager)
	}
	cmd.Args = append(cmd.Args, args...)
	var out limitedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	// launchctl uses this specific error for an absent service. A missing GUI
	// domain or an inaccessible manager remains an error, never an absent job.
	if executable == "launchctl" && len(args) == 2 && args[0] == "print" {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 113 && bytes.Contains(out.Bytes(), []byte("Could not find service")) {
			err = ErrNotInstalled
		}
	}
	return out.Bytes(), errors.Join(err, ctx.Err())
}

func readDefinition(name string) ([]byte, bool, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxDefinition {
		return nil, true, ErrCollision
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxDefinition+1))
	if len(data) > maxDefinition {
		return nil, true, ErrOutputLimit
	}
	return data, true, err
}
func (d *serviceDriver) Status(ctx context.Context, s Spec) (Status, error) {
	reg, err := Render(d.goos, s)
	if err != nil {
		return Status{State: "unknown", ProblemCode: "invalid_spec"}, err
	}
	status := Status{ID: reg.ID, State: "unknown"}
	installed, running, matched, err := d.query(ctx, s, reg)
	if err != nil {
		status.ProblemCode = "manager_unavailable"
		return status, errors.Join(ErrManager, err)
	}
	status.Installed, status.Running = installed, running
	if !installed {
		status.State = "absent"
	} else if running {
		status.State = "running"
	} else {
		status.State = "stopped"
	}
	receipt, hasReceipt, err := readDefinition(receiptPath(s, reg))
	if err != nil {
		status.State = "unknown"
		status.ProblemCode = "receipt_unreadable"
		return status, err
	}
	definition, hasDefinition, err := readDefinition(reg.Path)
	if err != nil {
		status.State = "unknown"
		status.ProblemCode = "definition_unreadable"
		return status, err
	}
	if (hasReceipt && !bytes.Equal(receipt, reg.Definition)) || (hasDefinition && (!hasReceipt || !bytes.Equal(definition, reg.Definition))) || (installed && (!hasReceipt || !hasDefinition || !matched)) {
		status.ProblemCode = "ownership_conflict"
		return status, ErrCollision
	}
	status.Owned = hasReceipt
	return status, nil
}
func (d *serviceDriver) failed(ctx context.Context, s Spec, code string, cause error) (Status, error) {
	status, err := d.Status(ctx, s)
	if err != nil {
		status.State = "unknown"
	}
	status.ProblemCode = code
	return status, &OperationError{Status: status, Cause: errors.Join(cause, err)}
}
func lockService(ctx context.Context, s Spec) (func(), error) {
	dir := filepath.Join(s.ConfigRoot, "services")
	if err := safefileio.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("preparing launcher lock directory: %w", err)
	}
	lock := flock.New(filepath.Join(dir, "launcher.lock"))
	ok, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquiring launcher lock: %w", err)
	}
	if !ok {
		return nil, ctx.Err()
	}
	return func() { _ = lock.Unlock() }, nil
}
func (d *serviceDriver) writeDefinition(name string, data []byte) error {
	prior, exists, err := readDefinition(name)
	if err != nil {
		return err
	}
	if exists {
		if bytes.Equal(prior, data) {
			return nil
		}
		return ErrCollision
	}
	if err := safefileio.EnsurePrivateDir(filepath.Dir(name)); err != nil {
		return fmt.Errorf("preparing launcher definition directory: %w", err)
	}
	return d.write(name, data)
}
func (d *serviceDriver) Install(ctx context.Context, s Spec) (Status, error) {
	return d.InstallChecked(ctx, s, nil)
}

// InstallChecked runs the caller's registry check under the same lock used by
// disconnect, before registration. It never starts a launcher if check fails.
func (d *serviceDriver) InstallChecked(ctx context.Context, s Spec, check func(context.Context) error) (Status, error) {
	reg, err := Render(d.goos, s)
	if err != nil {
		return Status{State: "unknown", ProblemCode: "invalid_spec"}, err
	}
	unlock, err := lockService(ctx, s)
	if err != nil {
		return Status{ID: reg.ID, State: "unknown", ProblemCode: "lock_failed"}, err
	}
	defer unlock()
	if check != nil {
		if err := check(ctx); err != nil {
			return Status{ID: reg.ID, State: "unknown", ProblemCode: "configuration_invalid"}, err
		}
	}
	status, err := d.Status(ctx, s)
	if err != nil {
		return status, err
	}
	if status.Installed && status.Running {
		return status, nil
	}
	if err := d.writeDefinition(receiptPath(s, reg), reg.Definition); err != nil {
		return d.failed(ctx, s, "receipt_write_failed", err)
	}
	if err := d.writeDefinition(reg.Path, reg.Definition); err != nil {
		return d.failed(ctx, s, "definition_write_failed", err)
	}
	var commands [][]string
	switch d.goos {
	case "linux":
		commands = [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", reg.ID}}
	case "darwin":
		if status.Installed {
			commands = [][]string{{"launchctl", "kickstart", "gui/" + s.UserID + "/" + reg.ID}}
		} else {
			commands = [][]string{{"launchctl", "bootstrap", "gui/" + s.UserID, reg.Path}}
		}
	case "windows":
		if !status.Installed {
			commands = append(commands, []string{"schtasks", "/Create", "/TN", reg.ID, "/XML", reg.Path})
		}
		commands = append(commands, []string{"schtasks", "/Run", "/TN", reg.ID})
	}
	for _, command := range commands {
		if _, err := d.run(ctx, command...); err != nil {
			return d.failed(ctx, s, "registration_failed", err)
		}
	}
	status, err = d.Status(ctx, s)
	if err != nil {
		return status, &OperationError{Status: status, Cause: err}
	}
	if !status.Installed || !status.Owned {
		return d.failed(ctx, s, "registration_unconfirmed", ErrManager)
	}
	return status, nil
}

// WithAbsent serializes the absent check and client removal with installation.
// An unreadable manager or leftover owned receipt is not proof of absence.
func (d *serviceDriver) WithAbsent(ctx context.Context, s Spec, action func(context.Context) error) error {
	if _, err := Render(d.goos, s); err != nil {
		return err
	}
	if action == nil {
		return errors.New("missing disconnect action")
	}
	unlock, err := lockService(ctx, s)
	if err != nil {
		return err
	}
	defer unlock()
	status, err := d.Status(ctx, s)
	if err != nil {
		return err
	}
	if status.Installed || status.Owned {
		return errors.New("uninstall the publisher service before disconnecting")
	}
	return action(ctx)
}
func (d *serviceDriver) Uninstall(ctx context.Context, s Spec) error {
	reg, err := Render(d.goos, s)
	if err != nil {
		return err
	}
	unlock, err := lockService(ctx, s)
	if err != nil {
		return err
	}
	defer unlock()
	status, err := d.Status(ctx, s)
	if err != nil {
		return err
	}
	if !status.Installed && !status.Owned {
		return nil
	}
	fail := func(code string, cause error) error { _, err := d.failed(ctx, s, code, cause); return err }
	if status.Installed {
		switch d.goos {
		case "linux":
			if _, err = d.run(ctx, "systemctl", "--user", "disable", "--now", reg.ID); err != nil {
				return fail("stop_failed", err)
			}
		case "darwin":
			if _, err = d.run(ctx, "launchctl", "bootout", "gui/"+s.UserID+"/"+reg.ID); err != nil {
				return fail("stop_failed", err)
			}
		case "windows":
			if status.Running {
				if _, err = d.run(ctx, "schtasks", "/End", "/TN", reg.ID); err != nil {
					return fail("stop_failed", err)
				}
			}
			if _, err = d.run(ctx, "schtasks", "/Delete", "/TN", reg.ID, "/F"); err != nil {
				return fail("delete_failed", err)
			}
		}
	}
	if d.goos == "linux" {
		if err = d.remove(reg.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail("definition_remove_failed", err)
		}
		if _, err = d.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fail("reload_failed", err)
		}
	}
	installed, _, _, err := d.query(ctx, s, reg)
	if err != nil || installed {
		return fail("removal_unconfirmed", errors.Join(ErrManager, err))
	}
	if d.goos != "linux" {
		if err = d.remove(reg.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail("definition_remove_failed", err)
		}
	}
	if err = d.remove(receiptPath(s, reg)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail("receipt_remove_failed", err)
	}
	return nil
}

func (d *serviceDriver) query(ctx context.Context, s Spec, reg Registration) (installed, running, matched bool, err error) {
	switch d.goos {
	case "linux":
		data, queryErr := d.run(ctx, "systemctl", "--user", "show", reg.ID, "--property=LoadState,ActiveState,FragmentPath", "--no-pager")
		fields := make(map[string]string)
		for line := range strings.SplitSeq(string(data), "\n") {
			if line == "" {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok || fields[key] != "" {
				return false, false, false, ErrManager
			}
			fields[key] = value
		}
		if queryErr == nil && fields["LoadState"] == "not-found" && fields["FragmentPath"] == "" && (fields["ActiveState"] == "" || fields["ActiveState"] == "inactive") {
			return false, false, false, nil
		}
		if queryErr != nil || fields["LoadState"] == "" || fields["ActiveState"] == "" {
			return false, false, false, errors.Join(ErrManager, queryErr)
		}
		definition, err := d.run(ctx, "systemctl", "--user", "cat", reg.ID, "--no-pager")
		if err != nil {
			return true, false, false, err
		}
		prefix := []byte("# " + reg.Path + "\n")
		definition = bytes.TrimPrefix(definition, prefix)
		return true, fields["ActiveState"] == "active", fields["LoadState"] == "loaded" && fields["FragmentPath"] == reg.Path && bytes.Equal(definition, reg.Definition), nil
	case "darwin":
		data, err := d.run(ctx, "launchctl", "print", "gui/"+s.UserID+"/"+reg.ID)
		if errors.Is(err, ErrNotInstalled) {
			if _, domainErr := d.run(ctx, "launchctl", "print", "gui/"+s.UserID); domainErr != nil {
				return false, false, false, domainErr
			}
			return false, false, false, nil
		}
		if err != nil {
			return false, false, false, err
		}
		fields := make(map[string]string)
		var argv []string
		inArgs := false
		for raw := range strings.SplitSeq(string(data), "\n") {
			line := strings.TrimSpace(raw)
			if inArgs {
				if line == "}" {
					inArgs = false
					continue
				}
				if strings.HasPrefix(line, `"`) {
					unquoted, err := strconv.Unquote(line)
					if err != nil {
						return true, false, false, ErrManager
					}
					line = unquoted
				}
				argv = append(argv, line)
				continue
			}
			if line == "arguments = {" {
				inArgs = true
				continue
			}
			if key, value, ok := strings.Cut(line, " = "); ok && (key == "path" || key == "program" || key == "state") {
				if _, exists := fields[key]; exists {
					return true, false, false, ErrManager
				}
				fields[key] = value
			}
		}
		if fields["state"] == "" || inArgs {
			return true, false, false, ErrManager
		}
		return true, fields["state"] == "running", fields["path"] == reg.Path && fields["program"] == reg.Argv[0] && equalStrings(argv, reg.Argv), nil
	case "windows":
		if d.taskState == nil {
			return false, false, false, ErrManager
		}
		state, err := d.taskState(ctx, reg.ID)
		if errors.Is(err, ErrNotInstalled) {
			return false, false, false, nil
		}
		if err != nil {
			return false, false, false, err
		}
		if state < 1 || state > 4 {
			return false, false, false, ErrManager
		}
		data, err := d.run(ctx, "schtasks", "/Query", "/TN", reg.ID, "/XML")
		if err != nil {
			return true, false, false, err
		}
		matched, err := equalTaskXML(data, reg.Definition)
		return true, state == 4, matched, err
	default:
		return false, false, false, ErrUnsupported
	}
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
