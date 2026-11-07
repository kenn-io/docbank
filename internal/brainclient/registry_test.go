package brainclient

import (
	"bufio"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

func brainFixtureConfig(t *testing.T, name string) ConnectionConfig {
	t.Helper()
	root := t.TempDir()
	return ConnectionConfig{Name: name, SourceRoot: filepath.Join(root, "Notes α"), Destination: "/knowledge", LocalVaultRoot: filepath.Join(root, "archive"), Exclude: []string{".git", "cache/"}, SettleTime: 2 * time.Second, ScanInterval: 5 * time.Second}
}

// Rejecting a valid record, silently repairing bad config, or leaking shared
// slices would lose or redirect a saved publication.
func TestBrainRegistryRoundTripAndValidation(t *testing.T) {
	ctx := t.Context()
	root := filepath.Join(t.TempDir(), "registry")
	r, err := OpenRegistry(root)
	require.NoError(t, err)
	empty, err := r.List(ctx)
	require.NoError(t, err)
	require.Empty(t, empty)
	_, err = os.Stat(root)
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = r.Get(ctx, "missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
	first := brainFixtureConfig(t, "a")
	second := brainFixtureConfig(t, "z")
	require.NoError(t, r.Put(ctx, second))
	require.NoError(t, r.Put(ctx, first))
	require.NoError(t, r.Put(ctx, first))
	reopened, err := OpenRegistry(root)
	require.NoError(t, err)
	saved, err := reopened.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []ConnectionConfig{first, second}, saved)
	saved[0].Exclude[0] = "changed"
	firstCopy, err := r.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, first, firstCopy)
	firstCopy.Exclude[0] = "changed-again"
	firstCopy, err = r.Get(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, first, firstCopy)
	before, err := os.ReadFile(filepath.Join(root, "connections.json"))
	require.NoError(t, err)
	require.Contains(t, string(before), `"source_root"`)
	require.NotContains(t, string(before), "synthetic-file-key")
	var record struct {
		Version     int                `json:"version"`
		Connections []ConnectionConfig `json:"connections"`
	}
	require.NoError(t, json.Unmarshal(before, &record, jsonv1.FormatDurationAsNano(true)))
	require.Equal(t, 1, record.Version)
	require.NoError(t, safefileio.ValidatePrivateDir(root))
	file, err := safefileio.OpenCurrentUserFile(filepath.Join(root, "connections.json"))
	require.NoError(t, err)
	require.NoError(t, safefileio.ValidatePrivateCurrentUserFile(file))
	require.NoError(t, file.Close())
	changed := first
	changed.IndexLocal = true
	require.ErrorIs(t, r.Put(ctx, changed), fs.ErrExist)
	after, err := os.ReadFile(filepath.Join(root, "connections.json"))
	require.NoError(t, err)
	require.Equal(t, before, after)
	invalid := map[string]func(*ConnectionConfig){
		"name":                func(c *ConnectionConfig) { c.Name = "../bad" },
		"relative source":     func(c *ConnectionConfig) { c.SourceRoot = "relative" },
		"relative vault":      func(c *ConnectionConfig) { c.LocalVaultRoot = "relative" },
		"two targets":         func(c *ConnectionConfig) { c.RemoteOrigin = "https://daemon.example" },
		"no target":           func(c *ConnectionConfig) { c.LocalVaultRoot = "" },
		"key on local":        func(c *ConnectionConfig) { c.KeyFile = filepath.Join(root, "key") },
		"destination":         func(c *ConnectionConfig) { c.Destination = "relative" },
		"unclean destination": func(c *ConnectionConfig) { c.Destination = "/a/../knowledge" },
		"exclusion":           func(c *ConnectionConfig) { c.Exclude = []string{"../outside"} },
		"settle":              func(c *ConnectionConfig) { c.SettleTime = -1 },
		"age":                 func(c *ConnectionConfig) { c.MinimumAge = -1 },
		"scan":                func(c *ConnectionConfig) { c.ScanInterval = 0 },
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			bad := first
			mutate(&bad)
			require.Error(t, r.Put(t.Context(), bad))
			got, e := os.ReadFile(filepath.Join(root, "connections.json"))
			require.NoError(t, e)
			require.Equal(t, before, got)
		})
	}
	remote := brainFixtureConfig(t, "remote")
	remote.LocalVaultRoot = ""
	remote.RemoteOrigin = "https://daemon.example:9443"
	remote.KeyFile = filepath.Join(root, "existing-key")
	require.NoError(t, r.Put(ctx, remote)) // Registry stores the reference without reading a key or connecting.
	for _, origin := range []string{"https://u:p@daemon.example", "https://daemon.example/path", "http://daemon.example?key=secret", "file:///tmp/daemon"} {
		bad := remote
		bad.Name = "bad-remote"
		bad.RemoteOrigin = origin
		require.Error(t, r.Put(ctx, bad))
	}
	remote.Name = "env-remote"
	remote.KeyFile = ""
	require.NoError(t, r.Put(ctx, remote))
	require.NoError(t, r.Remove(ctx, "z"))
	_, err = r.Get(ctx, "z")
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.ErrorIs(t, r.Remove(ctx, "z"), fs.ErrNotExist)
	t.Run("bounded strict read retains corrupt bytes", func(t *testing.T) {
		cases := []string{`{`, `{"version":2,"connections":[]}`, `{"version":1,"connections":[],"secret":"x"}`, `{"version":1,"version":1,"connections":[]}`, `{"version":1,"connections":[{}]}`, `{"version":1,"connections":[]} {}`, strings.Repeat(" ", 1<<20) + `{}`, `null`, `{}`, `{"version":1,"connections":null}`, `{"version":1,"connections":[{"unknown":true}]}`}
		for i, raw := range cases {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				require.NoError(t, os.WriteFile(filepath.Join(root, "connections.json"), []byte(raw), 0o600))
				_, e := r.List(t.Context())
				require.Error(t, e)
				require.Error(t, r.Put(t.Context(), first))
				got, e := os.ReadFile(filepath.Join(root, "connections.json"))
				require.NoError(t, e)
				require.Equal(t, raw, string(got))
			})
		}
	})
}

// Each real child starts from the same registry; dropping serialization loses
// a writer and fails the final authored list.
func TestBrainRegistryConcurrentWriters(t *testing.T) {
	root := filepath.Join(t.TempDir(), "registry")
	cfg := brainFixtureConfig(t, "a")
	commands := make([]*exec.Cmd, 2)
	for i, name := range []string{"a", "b"} {
		c := cfg
		c.Name = name
		b, err := json.Marshal(c, jsonv1.FormatDurationAsNano(true))
		require.NoError(t, err)
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestBrainRegistryChild$")
		cmd.Env = append(os.Environ(), "DOCBANK_REGISTRY_CHILD=put", "DOCBANK_REGISTRY_ROOT="+root, "DOCBANK_REGISTRY_CONFIG="+string(b))
		commands[i] = cmd
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
	}
	for _, cmd := range commands {
		require.NoError(t, cmd.Wait())
	}
	r, err := OpenRegistry(root)
	require.NoError(t, err)
	saved, err := r.List(t.Context())
	require.NoError(t, err)
	require.Len(t, saved, 2)
	require.Equal(t, "a", saved[0].Name)
	require.Equal(t, "b", saved[1].Name)
}

func TestBrainRegistryChild(t *testing.T) {
	mode := os.Getenv("DOCBANK_REGISTRY_CHILD")
	if mode == "" {
		return
	}
	var cfg ConnectionConfig
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("DOCBANK_REGISTRY_CONFIG")), &cfg, jsonv1.FormatDurationAsNano(true)))
	r, err := OpenRegistry(os.Getenv("DOCBANK_REGISTRY_ROOT"))
	require.NoError(t, err)
	switch mode {
	case "put":
		require.NoError(t, r.Put(t.Context(), cfg))
	case "hold":
		release, err := r.AcquireRun(t.Context(), cfg.Name)
		require.NoError(t, err)
		defer func() { require.NoError(t, release()) }()
		fmt.Println("registry-lock-ready")
		_, err = io.Copy(io.Discard, os.Stdin)
		require.NoError(t, err)
	default:
		t.Fatal("unknown synthetic child mode")
	}
}

// The operating-system lock, rather than just an in-memory mutex, excludes a
// second process until the owning process releases its run.
func TestBrainRegistryRunLockAcrossProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "registry")
	cfg := brainFixtureConfig(t, "a")
	r, err := OpenRegistry(root)
	require.NoError(t, err)
	require.NoError(t, r.Put(t.Context(), cfg))
	raw, err := json.Marshal(cfg, jsonv1.FormatDurationAsNano(true))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrainRegistryChild$")
	cmd.Env = append(os.Environ(), "DOCBANK_REGISTRY_CHILD=hold", "DOCBANK_REGISTRY_ROOT="+root, "DOCBANK_REGISTRY_CONFIG="+string(raw))
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = input.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	line, err := bufio.NewReader(output).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "registry-lock-ready\n", line)
	_, err = r.AcquireRun(t.Context(), cfg.Name)
	require.ErrorIs(t, err, ErrBusy)
	require.ErrorIs(t, r.Remove(t.Context(), cfg.Name), ErrBusy)
	require.NoError(t, input.Close())
	require.NoError(t, cmd.Wait())
	release, err := r.AcquireRun(t.Context(), cfg.Name)
	require.NoError(t, err)
	require.NoError(t, release())
}

// Persistence errors before publication keep old bytes; errors after it retain
// the typed visible/uncertain result and never trigger automatic rollback.
func TestBrainRegistryAtomicFailure(t *testing.T) {
	ctx := t.Context()
	root := filepath.Join(t.TempDir(), "registry")
	r, err := OpenRegistry(root)
	require.NoError(t, err)
	first := brainFixtureConfig(t, "a")
	second := brainFixtureConfig(t, "b")
	require.NoError(t, r.Put(ctx, first))
	prior, err := os.ReadFile(filepath.Join(root, "connections.json"))
	require.NoError(t, err)
	writes := 0
	r.writeFile = func(string, []byte, ...atomicfile.Option) error {
		writes++
		return errors.New("synthetic before publish")
	}
	require.Error(t, r.Put(ctx, second))
	after, err := os.ReadFile(filepath.Join(root, "connections.json"))
	require.NoError(t, err)
	require.Equal(t, prior, after)
	require.Equal(t, 1, writes)
	for _, kind := range []error{atomicfile.ErrPublished, atomicfile.ErrNotDurable} {
		t.Run(kind.Error(), func(t *testing.T) {
			writes = 0
			r.writeFile = func(path string, b []byte, opts ...atomicfile.Option) error {
				writes++
				if e := atomicfile.WriteFile(path, b, opts...); e != nil {
					return e
				}
				return fmt.Errorf("%w: synthetic after publish", kind)
			}
			err = r.Put(ctx, second)
			require.ErrorIs(t, err, kind)
			require.ErrorIs(t, err, atomicfile.ErrPublished)
			require.Equal(t, 1, writes)
			saved, e := r.Get(ctx, "b")
			require.NoError(t, e)
			require.Equal(t, second, saved)
			r.writeFile = atomicfile.WriteFile
			require.NoError(t, r.Remove(ctx, "b"))
		})
	}
}

// A lock on a name must outlive registry rewrites and block independent handles,
// while allowing another name to run and canceled waiters to return.
func TestBrainRegistryRunLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "registry")
	r, err := OpenRegistry(root)
	require.NoError(t, err)
	a := brainFixtureConfig(t, "a")
	b := brainFixtureConfig(t, "b")
	require.NoError(t, r.Put(t.Context(), a))
	require.NoError(t, r.Put(t.Context(), b))
	other, err := OpenRegistry(root)
	require.NoError(t, err)
	release, err := r.AcquireRun(t.Context(), "a")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, release()) })
	_, err = other.AcquireRun(t.Context(), "a")
	require.Error(t, err)
	require.Error(t, other.Remove(t.Context(), "a"))
	changed := a
	changed.IndexLocal = true
	require.Error(t, other.Put(t.Context(), changed))
	require.NoError(t, other.Remove(t.Context(), "b"))
	require.NoError(t, other.Put(t.Context(), b))
	_, err = other.AcquireRun(t.Context(), "a")
	require.Error(t, err)
	releaseB, err := other.AcquireRun(t.Context(), "b")
	require.NoError(t, err)
	require.NoError(t, releaseB())
	require.NoError(t, release())
	require.NoError(t, other.Remove(t.Context(), "a"))
	_, err = other.AcquireRun(t.Context(), "a")
	require.ErrorIs(t, err, fs.ErrNotExist)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, r.Put(canceled, a), context.Canceled)
	_, err = r.List(canceled)
	require.ErrorIs(t, err, context.Canceled)
}

// Key-file failures must not use a valid environment fallback. The parser keeps
// current push behavior so shell-created files with trailing newlines work.
func TestBrainKeySources(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-env-key")
	key, err := ReadKey("")
	require.NoError(t, err)
	require.Equal(t, "synthetic-env-key", key)
	file := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(file, []byte("synthetic-file-key"), 0o600))
	key, err = ReadKey(file)
	require.NoError(t, err)
	require.Equal(t, "synthetic-file-key", key)
	for _, value := range []string{"", " \r\n", "synthetic-file-key\nembedded"} {
		require.NoError(t, os.WriteFile(file, []byte(value), 0o600))
		key, err = ReadKey(file)
		require.Error(t, err)
		require.Empty(t, key)
		require.NotContains(t, err.Error(), "synthetic-file-key")
	}
	_, err = ReadKey(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	_, err = ReadKey(t.TempDir())
	require.Error(t, err)
	for _, value := range []string{"", "synthetic-env-key\n"} {
		t.Setenv("DOCBANK_API_KEY", value)
		_, err = ReadKey("")
		require.Error(t, err)
	}
	t.Setenv("DOCBANK_API_KEY", " synthetic-env-key ")
	key, err = ReadKey("")
	require.NoError(t, err)
	require.Equal(t, " synthetic-env-key ", key)
}
func TestBrainKeyFileTrailingNewlineAccepted(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-env-key")
	for _, ending := range []string{"\n", "\r\n"} {
		file := filepath.Join(t.TempDir(), "key")
		require.NoError(t, os.WriteFile(file, []byte("synthetic-file-key"+ending), 0o600))
		key, err := ReadKey(file)
		require.NoError(t, err)
		require.Equal(t, "synthetic-file-key", key)
	}
}
func TestBrainKeyFileInternalNewlineRejected(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-env-key")
	for _, ending := range []string{"\n", "\r\n"} {
		file := filepath.Join(t.TempDir(), "key")
		require.NoError(t, os.WriteFile(file, []byte("synthetic-file"+ending+"key"), 0o600))
		key, err := ReadKey(file)
		require.Error(t, err)
		require.Empty(t, key)
	}
}

func TestBrainRegistryDefaultRoot(t *testing.T) {
	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
	configRoot, err := DefaultConfigRoot()
	require.NoError(t, err)
	base, err := os.UserConfigDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(base, "docbank", "brain"), configRoot)
	_, err = os.Stat(configRoot)
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = OpenRegistry("")
	require.Error(t, err)
}

// Atomic replacement cannot replace a held lock inode. Nor may canceled
// registry-lock waiters publish after their caller stopped waiting.
func TestBrainRegistryCanceledWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry"))
		require.NoError(t, err)
		cfg := brainFixtureConfig(t, "a")
		require.NoError(t, r.Put(t.Context(), cfg))
		lock, err := r.lockRegistry(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, lock.Close()) })
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		other := cfg
		other.Name = "b"
		require.ErrorIs(t, r.Put(ctx, other), context.DeadlineExceeded)
		require.NoError(t, lock.Close())
		_, err = r.Get(t.Context(), "b")
		require.ErrorIs(t, err, fs.ErrNotExist)
		require.NoError(t, r.Put(t.Context(), other))
	})
}

// Unsafe roots and symlinked private files must fail without reading/writing
// their targets. Kit supplies the actual platform checks.
func TestBrainRegistryRefusesLinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	require.NoError(t, safefileio.EnsurePrivateDir(target))
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := OpenRegistry(link)
	require.Error(t, err)
	r, err := OpenRegistry(target)
	require.NoError(t, err)
	sentinel := filepath.Join(root, "sentinel")
	require.NoError(t, os.WriteFile(sentinel, []byte("synthetic sentinel"), 0o600))
	require.NoError(t, os.Symlink(sentinel, filepath.Join(target, "connections.json")))
	_, err = r.List(t.Context())
	require.Error(t, err)
	require.Error(t, r.Put(t.Context(), brainFixtureConfig(t, "a")))
	got, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	require.Equal(t, "synthetic sentinel", string(got))
}

func TestBrainRegistryRemoveAtomicFailure(t *testing.T) {
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry"))
	require.NoError(t, err)
	cfg := brainFixtureConfig(t, "a")
	require.NoError(t, r.Put(t.Context(), cfg))
	r.writeFile = func(string, []byte, ...atomicfile.Option) error { return errors.New("synthetic prepublication error") }
	require.Error(t, r.Remove(t.Context(), cfg.Name))
	saved, err := r.Get(t.Context(), cfg.Name)
	require.NoError(t, err)
	require.Equal(t, cfg, saved)
	r.writeFile = func(path string, data []byte, opts ...atomicfile.Option) error {
		if err := atomicfile.WriteFile(path, data, opts...); err != nil {
			return err
		}
		return atomicfile.ErrNotDurable
	}
	require.ErrorIs(t, r.Remove(t.Context(), cfg.Name), atomicfile.ErrNotDurable)
	_, err = r.Get(t.Context(), cfg.Name)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestBrainRegistryRejectsOversizeWrite(t *testing.T) {
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry"))
	require.NoError(t, err)
	cfg := brainFixtureConfig(t, "a")
	require.NoError(t, r.Put(t.Context(), cfg))
	cfg.Name = "b"
	cfg.Exclude = []string{strings.Repeat("a", 1<<20)}
	require.Error(t, r.Put(t.Context(), cfg))
	saved, err := r.List(t.Context())
	require.NoError(t, err)
	require.Len(t, saved, 1)
	require.Equal(t, "a", saved[0].Name)
}

// Saved push origins keep the accepted spelling, including a root slash and
// explicit default port; browser-origin normalization must not rewrite them.
func TestBrainRegistryPreservesPushOrigin(t *testing.T) {
	r, err := OpenRegistry(filepath.Join(t.TempDir(), "registry"))
	require.NoError(t, err)
	for i, origin := range []string{"https://daemon.example/", "http://daemon.example:80", "https://DAEMON.example:443/"} {
		cfg := brainFixtureConfig(t, fmt.Sprintf("remote-%d", i))
		cfg.LocalVaultRoot = ""
		cfg.RemoteOrigin = origin
		require.NoError(t, r.Put(t.Context(), cfg))
		saved, err := r.Get(t.Context(), cfg.Name)
		require.NoError(t, err)
		require.Equal(t, origin, saved.RemoteOrigin)
	}
}
