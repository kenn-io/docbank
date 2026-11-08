package brainservice

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeManager struct {
	goos                          string
	reg                           Registration
	installed, running            bool
	calls                         [][]string
	fail                          string
	mutateOnFailure, failReadback bool
	foreign                       bool
}

func (m *fakeManager) runner(_ context.Context, exe string, args ...string) ([]byte, error) {
	m.calls = append(m.calls, append([]string{exe}, args...))
	if m.failReadback && m.installed && (exe == "launchctl" && args[0] == "print" || exe == "systemctl" && args[1] == "show" || exe == "schtasks" && args[0] == "/Query") {
		return nil, errors.New("synthetic manager unavailable")
	}
	command := args[0]
	if exe == "systemctl" {
		requireFake(len(args) > 1)
		command = args[1]
	}
	failure := m.fail == command
	if failure && !m.mutateOnFailure {
		return nil, errors.New("synthetic operation failure")
	}
	switch exe {
	case "systemctl":
		requireFake(args[0] == "--user")
		switch command {
		case "show":
			if !m.installed {
				return []byte("LoadState=not-found\nActiveState=inactive\nFragmentPath=\n"), nil
			}
			state := "inactive"
			if m.running {
				state = "active"
			}
			return fmt.Appendf(nil, "LoadState=loaded\nActiveState=%s\nFragmentPath=%s\n", state, m.reg.Path), nil
		case "cat":
			data, err := os.ReadFile(m.reg.Path)
			if m.foreign {
				data = []byte("[Service]\nExecStart=/other\n")
			}
			if err != nil {
				return nil, err
			}
			return append([]byte("# "+m.reg.Path+"\n"), data...), nil
		case "enable":
			m.installed = true
			m.running = true
		case "disable":
			m.running = false
		case "daemon-reload":
			if _, err := os.Stat(m.reg.Path); os.IsNotExist(err) {
				m.installed = false
			}
		default:
			panic("unexpected systemctl command")
		}
	case "launchctl":
		switch command {
		case "print":
			if !strings.Contains(args[1], "/io.kenn.") {
				return []byte("domain available\n"), nil
			}
			if !m.installed {
				return nil, ErrNotInstalled
			}
			var b strings.Builder
			fmt.Fprintf(&b, "job = {\n path = %s\n program = %s\n state = ", m.reg.Path, m.reg.Argv[0])
			if m.running {
				b.WriteString("running")
			} else {
				b.WriteString("not running")
			}
			b.WriteString("\n arguments = {\n")
			for _, arg := range m.reg.Argv {
				fmt.Fprintf(&b, "  %s\n", arg)
			}
			b.WriteString(" }\n}\n")
			if m.foreign {
				return []byte(strings.Replace(b.String(), "--config-root", "--other-root", 1)), nil
			}
			return []byte(b.String()), nil
		case "bootstrap":
			m.installed = true
			m.running = true
		case "kickstart":
			m.running = true
		case "bootout":
			m.installed = false
			m.running = false
		default:
			panic("unexpected launchctl command")
		}
	case "schtasks":
		switch command {
		case "/Query":
			if !m.installed {
				return nil, ErrNotInstalled
			}
			if m.foreign {
				return []byte(strings.Replace(string(m.reg.Definition), "LeastPrivilege", "HighestAvailable", 1)), nil
			}
			return m.reg.Definition, nil
		case "/Create":
			for _, arg := range args {
				requireFake(arg != "/F")
			}
			m.installed = true
		case "/Run":
			m.running = true
		case "/End":
			m.running = false
		case "/Delete":
			m.installed = false
			m.running = false
		default:
			panic("unexpected schtasks command")
		}
	default:
		panic("non-manager executable in fake")
	}
	if failure {
		return nil, errors.New("synthetic lost operation receipt")
	}
	return nil, nil
}
func requireFake(ok bool) {
	if !ok {
		panic("unexpected fake manager invocation")
	}
}
func (m *fakeManager) taskState(context.Context, string) (int, error) {
	if !m.installed {
		return 0, ErrNotInstalled
	}
	if m.running {
		return 4, nil
	}
	return 3, nil
}
func driverFixture(t *testing.T) (*serviceDriver, Spec, *fakeManager) {
	t.Helper()
	return driverFixtureFor(t, runtime.GOOS)
}
func driverFixtureFor(t *testing.T, goos string) (*serviceDriver, Spec, *fakeManager) {
	t.Helper()
	spec := renderSpec(goos)
	base := t.TempDir()
	spec.ConfigRoot = filepath.Join(base, "registry")
	spec.UserHome = filepath.Join(base, "home")
	spec.UserConfigRoot = filepath.Join(base, "config")
	spec.Executable = filepath.Join(base, "Docbank café $&%", "docbank")
	reg, err := Render(goos, spec)
	require.NoError(t, err)
	manager := &fakeManager{goos: goos, reg: reg}
	driver, ok := NewDriver(manager.runner).(*serviceDriver)
	require.True(t, ok)
	driver.goos = goos
	driver.taskState = manager.taskState
	return driver, spec, manager
}

func TestBrainServiceOwnedLifecycle(t *testing.T) {
	driver, spec, manager := driverFixture(t)
	ctx := t.Context()
	initial, err := driver.Status(ctx, spec)
	require.NoError(t, err)
	require.False(t, initial.Installed)
	_, err = os.Stat(spec.ConfigRoot)
	require.True(t, os.IsNotExist(err), "status must not create client files")
	installed, err := driver.Install(ctx, spec)
	require.NoError(t, err)
	require.True(t, installed.Installed && installed.Running && installed.Owned)
	status, err := driver.Status(ctx, spec)
	require.NoError(t, err)
	require.Equal(t, installed, status)
	calls := len(manager.calls)
	_, err = driver.Install(ctx, spec)
	require.NoError(t, err)
	for _, call := range manager.calls[calls:] {
		require.NotContains(t, []string{"enable", "bootstrap", "/Create", "/Run"}, managerOperation(call))
	}
	manager.running = false
	restarted, err := driver.Install(ctx, spec)
	require.NoError(t, err)
	require.True(t, restarted.Running)
	require.NoError(t, driver.Uninstall(ctx, spec))
	status, err = driver.Status(ctx, spec)
	require.NoError(t, err)
	require.False(t, status.Installed)
	require.False(t, status.Owned)
	_, err = os.Stat(manager.reg.Path)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(receiptPath(spec, manager.reg))
	require.True(t, os.IsNotExist(err))
}
func TestBrainServicePreservesUnownedDefinition(t *testing.T) {
	for _, kind := range []string{"manager", "file", "modified-owned"} {
		t.Run(kind, func(t *testing.T) {
			driver, spec, manager := driverFixture(t)
			switch kind {
			case "modified-owned":
				_, err := driver.Install(t.Context(), spec)
				require.NoError(t, err)
				manager.foreign = true
			case "manager":
				manager.installed = true
				manager.foreign = true
				require.NoError(t, os.MkdirAll(filepath.Dir(manager.reg.Path), 0o700))
				require.NoError(t, os.WriteFile(manager.reg.Path, []byte("unrelated"), 0o600))
			default:
				require.NoError(t, os.MkdirAll(filepath.Dir(manager.reg.Path), 0o700))
				require.NoError(t, os.WriteFile(manager.reg.Path, []byte("unrelated"), 0o600))
			}
			before := len(manager.calls)
			_, err := driver.Install(t.Context(), spec)
			require.ErrorIs(t, err, ErrCollision)
			require.ErrorIs(t, driver.Uninstall(t.Context(), spec), ErrCollision)
			for _, call := range manager.calls[before:] {
				for _, arg := range call[1:] {
					require.NotContains(t, []string{"disable", "enable", "bootstrap", "bootout", "kickstart", "/Create", "/End", "/Delete", "/Run"}, arg)
				}
			}
			if kind == "file" {
				data, err := os.ReadFile(manager.reg.Path)
				require.NoError(t, err)
				require.Equal(t, "unrelated", string(data))
			}
		})
	}
}
func TestBrainServicePartialFailure(t *testing.T) {
	registration := map[string]string{"linux": "enable", "darwin": "bootstrap", "windows": "/Create"}[runtime.GOOS]
	for _, failure := range []string{"before-write", "definition-write", "before-register", "after-register", "readback", "uninstall"} {
		t.Run(failure, func(t *testing.T) {
			driver, spec, manager := driverFixture(t)
			switch failure {
			case "before-write":
				driver.write = func(string, []byte) error { return fs.ErrPermission }
			case "definition-write":
				write := driver.write
				driver.write = func(name string, data []byte) error {
					if name == manager.reg.Path {
						return fs.ErrPermission
					}
					return write(name, data)
				}
			case "before-register":
				manager.fail = registration
			case "after-register":
				manager.fail = registration
				manager.mutateOnFailure = true
			case "readback":
				manager.failReadback = true
			}
			if failure == "uninstall" {
				_, err := driver.Install(t.Context(), spec)
				require.NoError(t, err)
				manager.fail = map[string]string{"linux": "disable", "darwin": "bootout", "windows": "/End"}[runtime.GOOS]
				require.Error(t, driver.Uninstall(t.Context(), spec))
				require.True(t, manager.installed)
			} else {
				status, err := driver.Install(t.Context(), spec)
				require.Error(t, err)
				if failure == "readback" {
					require.Equal(t, "unknown", status.State)
					require.False(t, status.Installed)
				}
				if failure == "after-register" {
					require.True(t, status.Installed && status.Owned)
				}
			}
			_, err := os.Stat(receiptPath(spec, manager.reg))
			if failure == "before-write" {
				require.True(t, os.IsNotExist(err))
			} else {
				require.NoError(t, err, "never discard ownership after an uncertain registration")
			}
		})
	}
}
func TestBrainServiceInvalidBeforeIO(t *testing.T) {
	driver, spec, manager := driverFixture(t)
	spec.Name = "invalid/name"
	_, err := driver.Install(t.Context(), spec)
	require.Error(t, err)
	require.Empty(t, manager.calls)
	_, err = os.Stat(spec.ConfigRoot)
	require.True(t, os.IsNotExist(err))
}

func managerOperation(call []string) string {
	if call[0] == "systemctl" {
		return call[2]
	}
	return call[1]
}

func TestBrainServicePostpublicationWriteFailure(t *testing.T) {
	for _, kind := range []string{"receipt", "definition"} {
		t.Run(kind, func(t *testing.T) {
			d, s, m := driverFixture(t)
			write := d.write
			d.write = func(name string, data []byte) error {
				if err := write(name, data); err != nil {
					return err
				}
				if (kind == "receipt" && name == receiptPath(s, m.reg)) || (kind == "definition" && name == m.reg.Path) {
					return fs.ErrPermission
				}
				return nil
			}
			status, err := d.Install(t.Context(), s)
			require.Error(t, err)
			require.True(t, status.Owned, "receipt survived publication even when acknowledgment failed")
			require.False(t, status.Installed)
			d.write = write
			status, err = d.Install(t.Context(), s)
			require.NoError(t, err)
			require.True(t, status.Installed && status.Owned)
			require.NoError(t, d.Uninstall(t.Context(), s))
		})
	}
}
func TestBrainServiceRemovalFailureRetainsReceipt(t *testing.T) {
	for _, kind := range []string{"definition", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			d, s, m := driverFixture(t)
			_, err := d.Install(t.Context(), s)
			require.NoError(t, err)
			remove := d.remove
			d.remove = func(name string) error {
				if (kind == "definition" && name == m.reg.Path) || (kind == "receipt" && name == receiptPath(s, m.reg)) {
					return fs.ErrPermission
				}
				return remove(name)
			}
			err = d.Uninstall(t.Context(), s)
			require.Error(t, err)
			var op *OperationError
			require.ErrorAs(t, err, &op)
			_, err = os.Stat(receiptPath(s, m.reg))
			require.NoError(t, err)
			d.remove = remove
			require.NoError(t, d.Uninstall(t.Context(), s), "retry after failed file removal remains owned")
		})
	}
}
func TestBrainServiceNativeCommands(t *testing.T) {
	d, s, m := driverFixture(t)
	_, err := d.Install(t.Context(), s)
	require.NoError(t, err)
	require.NoError(t, d.Uninstall(t.Context(), s))
	var mutations [][]string
	for _, call := range m.calls {
		switch managerOperation(call) {
		case "show", "cat", "print", "/Query":
			continue
		}
		mutations = append(mutations, call)
	}
	var want [][]string
	switch runtime.GOOS {
	case "linux":
		want = [][]string{
			{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", "--now", m.reg.ID},
			{"systemctl", "--user", "disable", "--now", m.reg.ID}, {"systemctl", "--user", "daemon-reload"},
		}
	case "darwin":
		want = [][]string{
			{"launchctl", "bootstrap", "gui/" + s.UserID, m.reg.Path}, {"launchctl", "bootout", "gui/" + s.UserID + "/" + m.reg.ID},
		}
	case "windows":
		want = [][]string{
			{"schtasks", "/Create", "/TN", m.reg.ID, "/XML", m.reg.Path}, {"schtasks", "/Run", "/TN", m.reg.ID},
			{"schtasks", "/End", "/TN", m.reg.ID}, {"schtasks", "/Delete", "/TN", m.reg.ID, "/F"},
		}
	}
	require.Equal(t, want, mutations)
}
