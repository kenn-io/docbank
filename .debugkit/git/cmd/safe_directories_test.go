package gitcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachedTrustFilesystemBound(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "contention"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				originalAdmission := safeDirectoryFilesystem
				safeDirectoryFilesystem = &safeDirectoryFilesystemAdmission{}
				t.Cleanup(func() { safeDirectoryFilesystem = originalAdmission })
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				started, release := make(chan struct{}), make(chan struct{})
				result := make(chan bool, 3)
				var callbackErr error
				followed := false
				go func() {
					result <- evaluateFilesystem(ctx, func(checkCtx context.Context) {
						close(started)
						<-release
						callbackErr = checkCtx.Err()
						if callbackErr == nil {
							followed = true
						}
					})
				}()
				<-started
				if mode == "contention" {
					completed := make(chan struct{}, 2)
					for range 2 {
						go func() {
							result <- evaluateFilesystem(t.Context(), func(context.Context) { completed <- struct{}{} })
						}()
					}
					synctest.Wait()
					assert.Empty(t, completed)
					assert.Empty(t, result)
					close(release)
					for range 3 {
						assert.True(t, <-result)
					}
					synctest.Wait()
					assert.Len(t, completed, 2)
					require.NoError(t, callbackErr)
					assert.True(t, followed)
					return
				}
				called := false
				go func() {
					result <- evaluateFilesystem(t.Context(), func(context.Context) { called = true })
				}()
				synctest.Wait()
				assert.Empty(t, result)
				endedAt := time.Now()
				if mode == "cancel" {
					cancel()
				} else {
					time.Sleep(safeDirectoryProbeTimeout)
				}
				synctest.Wait()
				require.False(t, <-result)
				require.False(t, <-result)
				if mode == "cancel" {
					assert.Zero(t, time.Since(endedAt))
				}
				before := time.Now()
				assert.False(t, evaluateFilesystem(t.Context(), func(context.Context) { called = true }))
				assert.Zero(t, time.Since(before))
				assert.False(t, called)
				close(release)
				synctest.Wait()
				require.Error(t, callbackErr)
				assert.False(t, followed)
				assert.True(t, evaluateFilesystem(t.Context(), func(context.Context) { called = true }))
				assert.True(t, called)
			})
		})
	}
}

func TestSafeDirectoryFingerprintStreaming(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitconfig")
	env := safeDirectoryTestEnv(t, path)
	fingerprint := func() ([32]byte, error) {
		snapshot, err := readSafeDirectorySnapshot(t.Context(), env, "", "global", safeDirectoryScope{paths: []string{path}})
		return snapshot.fingerprint, err
	}
	missing, err := fingerprint()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("# comment\n", 1<<15)), 0o600))
	before, err := fingerprint()
	require.NoError(t, err)
	again, err := fingerprint()
	require.NoError(t, err)
	assert.Equal(t, before, again)
	assert.NotEqual(t, missing, before)
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("# changed\n", 1<<15)), 0o600))
	after, err := fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
	require.NoError(t, os.Remove(path))
	after, err = fingerprint()
	require.NoError(t, err)
	assert.Equal(t, missing, after)
}

func extendTrustProbeTimeout(t *testing.T) {
	t.Helper()
	originalTimeout := safeDirectoryProbeTimeout
	safeDirectoryProbeTimeout = 30 * time.Second
	t.Cleanup(func() { safeDirectoryProbeTimeout = originalTimeout })
}

func trustTrace(t *testing.T, path string) func(int) {
	t.Helper()
	count := func() int {
		contents, err := os.ReadFile(path)
		if !os.IsNotExist(err) {
			require.NoError(t, err)
		}
		return strings.Count(string(contents), "built-in:")
	}
	previous := count()
	return func(increment int) {
		t.Helper()
		actual := count()
		assert.Equal(t, previous+increment, actual)
		previous = actual
	}
}

func TestRunnerWithEnvKeepsIndependentTrustCaches(t *testing.T) {
	extendTrustProbeTimeout(t)
	dir := t.TempDir()
	config, otherConfig := filepath.Join(dir, "global"), filepath.Join(dir, "other")
	trace := filepath.Join(dir, "trace")
	require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /original\n"), 0o600))
	require.NoError(t, os.WriteFile(otherConfig, []byte("[safe]\n directory = /other\n"), 0o600))
	runner := New()
	runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_CONFIG_NOSYSTEM=1", "GIT_TRACE="+filepath.ToSlash(trace))
	checkTrace := trustTrace(t, trace)
	check := func(r Runner, want string, probes int) {
		t.Helper()
		out, err := r.Output(t.Context(), dir, "config", "--get-all", "safe.directory")
		require.NoError(t, err)
		assert.Equal(t, want+"\n", string(out))
		checkTrace(probes)
	}
	check(runner, "/original", 1)
	check(runner, "/original", 2)
	check(runner, "/original", 0)
	env := append(slices.Clone(runner.Env), "GIT_CONFIG_GLOBAL="+otherConfig)
	other := runner.WithEnv(env)
	// WithEnv takes a snapshot, so later caller edits cannot change it.
	env[len(env)-1] = "GIT_CONFIG_GLOBAL=" + config
	check(other, "/other", 1)
	check(other, "/other", 2)
	check(other, "/other", 0)
	check(runner, "/original", 0)
	require.NoError(t, os.WriteFile(otherConfig, []byte("[safe]\n directory = /edited\n"), 0o600))
	check(other, "/edited", 1)
	check(other, "/edited", 0)
	check(runner, "/original", 0)
}

func trustHomeShare(t *testing.T, env []string) ([]string, string, string) {
	t.Helper()
	dir := t.TempDir()
	share, profile := filepath.Join(dir, "share"), filepath.Join(dir, "profile")
	for _, home := range []string{share, profile} {
		require.NoError(t, os.Mkdir(home, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[safe]\n directory = /"+filepath.Base(home)+"\n"), 0o600))
	}
	env = slices.DeleteFunc(env, func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return strings.EqualFold(key, "HOME") || strings.EqualFold(key, "GIT_CONFIG_GLOBAL") || strings.EqualFold(key, "XDG_CONFIG_HOME")
	})
	drive := filepath.VolumeName(share)
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "HOMEDRIVE="+drive, "HOMEPATH="+strings.TrimPrefix(share, drive), "USERPROFILE="+profile)
	return env, share, profile
}

func TestCachedTrustWithGitConfig(t *testing.T) {
	extendTrustProbeTimeout(t)
	for _, mode := range []string{"file", "empty"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			config, trace := filepath.Join(dir, "global"), filepath.Join(dir, "trace")
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = *\n"), 0o600))
			runner := New()
			runner.Env = safeDirectoryTestEnv(t, config)
			init := gitCommand(t.Context(), true, "init", dir)
			init.Env = runner.Env
			require.NoError(t, init.Run())
			control := New()
			control.Env = runner.Env
			trusted := control.Command(t.Context(), dir, "status")
			trusted.Env = append(trusted.Env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
			require.NoError(t, trusted.Run())
			value := config
			if mode == "empty" {
				value = ""
			}
			runner.Env = append(runner.Env, "GIT_CONFIG="+value, "GIT_TRACE="+filepath.ToSlash(trace))
			checkTrace := trustTrace(t, trace)
			for range 3 {
				cmd := runner.Command(t.Context(), dir, "status")
				assert.Empty(t, gitConfigValue(strings.Join(cmd.Env, "\n"), "safe.directory"))
				checkTrace(2)
				cmd.Env = append(cmd.Env, "GIT_TEST_ASSUME_DIFFERENT_OWNER=1")
				output, err := cmd.CombinedOutput()
				require.Error(t, err)
				assert.Contains(t, string(output), "detected dubious ownership")
				checkTrace(0)
			}
		})
	}
}

func TestCachedTrust(t *testing.T) {
	extendTrustProbeTimeout(t)
	for _, tc := range []struct {
		name, noSystem string
	}{{"no system 0", "0"}, {"no system 1", "1"}} {
		t.Run(tc.name, func(t *testing.T) {
			noSystem := tc.noSystem
			dir := t.TempDir()
			config, otherConfig := filepath.Join(dir, "config..old"), filepath.Join(dir, "other")
			trace := filepath.Join(dir, "trace")
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
			require.NoError(t, os.WriteFile(otherConfig, []byte("[safe]\n directory = /other\n"), 0o600))
			runner := New()
			runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_TRACE="+filepath.ToSlash(trace), "GIT_CONFIG_NOSYSTEM="+noSystem, "HOME="+dir+"/unused/..\n", "XDG_CONFIG_HOME="+dir+"/unused/..\r")
			system, _ := envValue(runner.Env, "GIT_CONFIG_SYSTEM")
			if noSystem == "0" {
				system = filepath.Dir(system) + "/unused/../" + filepath.Base(system)
			} else {
				system = "/inactive\nconfig\r"
			}
			runner.Env = append(runner.Env, "GIT_CONFIG_SYSTEM="+system)
			other := runner
			other.Env = append(append([]string(nil), runner.Env...), "GIT_CONFIG_GLOBAL="+otherConfig)
			checkTrace := trustTrace(t, trace)
			check := func(r Runner, want string, increment int) {
				assert.Equal(t, []string{want}, r.trust.read(t.Context(), r.Env, dir))
				checkTrace(increment)
			}
			baseline := 2
			if noSystem == "1" {
				baseline = 1
			}
			check(runner, "/trusted", baseline)
			check(other, "/other", baseline)
			check(runner, "/trusted", 2*baseline)
			check(other, "/other", baseline)
			check(runner, "/trusted", 0)
			var calls sync.WaitGroup
			for worker := range 8 {
				calls.Go(func() {
					for i := worker; i < 1369; i += 8 {
						cmd := runner.Command(t.Context(), dir, "status")
						assert.Equal(t, "/trusted", gitConfigValue(strings.Join(cmd.Env, "\n"), "safe.directory"))
					}
				})
			}
			calls.Wait()
			check(runner, "/trusted", 0)
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
			check(runner, "/trusted", 0)
			check(runner.WithConfig("gc.auto", "1"), "/trusted", 0)
			fresh := New()
			fresh.Env = runner.Env
			check(fresh, "/trusted", baseline)
		})
	}
	t.Run("discovery recovery", func(t *testing.T) {
		for _, mode := range []string{"deleted directory", "repaired config"} {
			t.Run(mode, func(t *testing.T) {
				dir := t.TempDir()
				config, trace := filepath.Join(dir, "global"), filepath.Join(dir, "trace")
				require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
				runner := New()
				runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_TRACE="+filepath.ToSlash(trace))
				bad := filepath.Join(dir, "bad")
				var original []byte
				if mode == "repaired config" {
					cmd := gitCommand(t.Context(), true, "init", bad)
					cmd.Env = runner.Env
					require.NoError(t, cmd.Run())
					var err error
					original, err = os.ReadFile(filepath.Join(bad, ".git", "config"))
					require.NoError(t, err)
				}
				require.NoError(t, os.WriteFile(trace, nil, 0o600))
				checkTrace := trustTrace(t, trace)
				check := func(cwd string, want []string, increment int) {
					assert.Equal(t, want, runner.trust.read(t.Context(), runner.Env, cwd))
					checkTrace(increment)
				}
				check(dir, []string{"/trusted"}, 2)
				if mode == "deleted directory" {
					require.NoError(t, os.Mkdir(bad, 0o700))
					require.NoError(t, os.Remove(bad))
					check(bad, nil, 0)
				} else {
					require.NoError(t, os.WriteFile(filepath.Join(bad, ".git", "config"), append(slices.Clone(original), []byte("\n[core]\n autocrlf = garbage\n")...), 0o600))
					check(bad, []string{"/trusted"}, 3)
					check(bad, []string{"/trusted"}, 3)
					require.NoError(t, os.WriteFile(filepath.Join(bad, ".git", "config"), original, 0o600))
				}
				check(dir, []string{"/trusted"}, 4)
				check(dir, []string{"/trusted"}, 0)
				require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /updated\n"), 0o600))
				check(dir, []string{"/updated"}, 1)
			})
		}
	})
	t.Run("global selection", func(t *testing.T) {
		dir := t.TempDir()
		home, xdg := filepath.Join(dir, "home"), filepath.Join(dir, "xdg")
		user, fallback := filepath.Join(home, ".gitconfig"), filepath.Join(xdg, "git", "config")
		for _, path := range []string{user, fallback} {
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		}
		require.NoError(t, os.WriteFile(user, []byte("[safe]\n directory = /home\n directory =\n directory = /after\n"), 0o600))
		require.NoError(t, os.WriteFile(fallback, []byte("[safe]\n directory = /xdg\n"), 0o600))
		runner := New()
		runner.Env = slices.DeleteFunc(safeDirectoryTestEnv(t, ""), func(entry string) bool { return strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=") })
		runner.Env = append(runner.Env, "HOME="+home, "XDG_CONFIG_HOME="+xdg, "GIT_CONFIG_NOSYSTEM=1")
		for range 3 {
			assert.Equal(t, []string{"/home", "", "/after"}, runner.trust.read(t.Context(), runner.Env, dir))
		}
		require.NoError(t, os.Remove(user))
		assert.Equal(t, []string{"/xdg"}, runner.trust.read(t.Context(), runner.Env, dir))
		require.NoError(t, os.WriteFile(user, []byte("[safe]\n directory = /preferred\n"), 0o600))
		assert.Equal(t, []string{"/preferred"}, runner.trust.read(t.Context(), runner.Env, dir))
		require.NoError(t, os.Remove(user))
		require.NoError(t, os.Remove(fallback))
		assert.Empty(t, runner.trust.read(t.Context(), runner.Env, dir))
	})
	t.Run("home share", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("requires Git for Windows HOME selection")
		}
		dir := t.TempDir()
		runner := New()
		var share, profile string
		runner.Env, share, profile = trustHomeShare(t, safeDirectoryTestEnv(t, ""))
		read := func(want string) {
			assert.Equal(t, []string{want}, runner.trust.read(t.Context(), runner.Env, dir))
		}
		for range 3 {
			read("/share")
		}
		require.NoError(t, os.Rename(share, share+"-offline"))
		read("/profile")
		require.NoError(t, os.WriteFile(filepath.Join(profile, ".gitconfig"), []byte("[safe]\n directory = /updated\n"), 0o600))
		read("/updated")
		require.NoError(t, os.Rename(share+"-offline", share))
		read("/share")
	})
	t.Run("invalidation", func(t *testing.T) {
		dir := t.TempDir()
		config := filepath.Join(dir, "gitconfig")
		trace := filepath.Join(dir, "trace")
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		bin := buildTestGit(t, `package main
import ("os"; "os/exec")
func main() {
 file, err := os.OpenFile(os.Getenv("TRUST_TEST_TRACE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
 if err != nil { os.Exit(2) }; file.WriteString("start\n"); file.Close()
 forwardGit()
}
`+trustGitForwarder)
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		runner := New()
		runner.Env = append(safeDirectoryTestEnv(t, config), "TRUST_TEST_TRACE="+trace, "TRUST_TEST_GIT="+realGit)
		read := func() []string {
			return runner.trust.read(t.Context(), runner.Env, dir)
		}
		assert.Empty(t, read())
		require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /first\n"), 0o600))
		assert.Equal(t, []string{"/first"}, read())
		info, err := os.Stat(config)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /other\n"), 0o600))
		require.NoError(t, os.Chtimes(config, info.ModTime(), info.ModTime()))
		assert.Equal(t, []string{"/other"}, read())
		contents, err := os.ReadFile(trace)
		require.NoError(t, err)
		count := strings.Count(string(contents), "start\n")
		check := func(want []string, increment int) {
			assert.Equal(t, want, read())
			contents, err := os.ReadFile(trace)
			require.NoError(t, err)
			actual := strings.Count(string(contents), "start\n")
			assert.Equal(t, count+increment, actual)
			count = actual
		}
		require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /untrusted\n[broken\n"), 0o600))
		check(nil, 2)
		check(nil, 1)
		require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /repaired\n"), 0o600))
		check([]string{"/repaired"}, 1)
		check([]string{"/repaired"}, 1)
		check([]string{"/repaired"}, 0)
	})
	t.Run("includes", func(t *testing.T) {
		dir := t.TempDir()
		config := filepath.Join(dir, "gitconfig")
		trace := filepath.Join(dir, "trace")
		included := filepath.Join(dir, "included")
		require.NoError(t, os.WriteFile(config, []byte("[include]\n path = included\n"), 0o600))
		runner := New()
		runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_TRACE="+filepath.ToSlash(trace))
		assert.Empty(t, runner.trust.read(t.Context(), runner.Env, dir))
		for _, trust := range []string{"/first", "/other"} {
			require.NoError(t, os.WriteFile(included, []byte("[safe]\n directory = "+trust+"\n"), 0o600))
			assert.Equal(t, []string{trust}, runner.trust.read(t.Context(), runner.Env, dir))
		}
		require.NoError(t, os.WriteFile(config, []byte("[includeIf \"onbranch:trusted\"]\n path = included\n"), 0o600))
		require.NoError(t, os.WriteFile(included, []byte("[safe]\n directory = /branch\n"), 0o600))
		_, err := runner.Output(t.Context(), dir, "init", "-b", "trusted")
		require.NoError(t, err)
		assert.Equal(t, []string{"/branch"}, runner.trust.read(t.Context(), runner.Env, dir))
		_, err = runner.Output(t.Context(), dir, "symbolic-ref", "HEAD", "refs/heads/other")
		require.NoError(t, err)
		assert.Empty(t, runner.trust.read(t.Context(), runner.Env, dir))
		checkTrace := trustTrace(t, trace)
		require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /direct\n"), 0o600))
		for _, increment := range []int{1, 1, 0} {
			assert.Equal(t, []string{"/direct"}, runner.trust.read(t.Context(), runner.Env, dir))
			checkTrace(increment)
		}
	})
	t.Run("ambiguous paths", func(t *testing.T) {
		for _, tc := range []struct{ key, suffix, component string }{
			{"GIT_CONFIG_GLOBAL", "gitconfig", "sub"},
			{"GIT_CONFIG_GLOBAL", "config", "link/.."},
			{"GIT_CONFIG_GLOBAL", "config", "line\npart"},
			{"HOME", ".gitconfig", "line\npart"},
			{"XDG_CONFIG_HOME", "git/config", "line\npart"},
			{"GIT_CONFIG_SYSTEM", "config", "line\npart"},
			{"GIT_CONFIG_GLOBAL", "config", "line\rpart"},
			{"GIT_CONFIG_SYSTEM", "config", "line\rpart"},
		} {
			t.Run(tc.key+" "+strings.ReplaceAll(strings.ReplaceAll(tc.component, "\n", "LF"), "\r", "CR"), func(t *testing.T) {
				relative := tc.component == "sub"
				if !relative && runtime.GOOS == "windows" {
					t.Skip("requires native Unix path resolution")
				}
				dir := t.TempDir()
				cwd := dir
				target := filepath.Join(dir, tc.component)
				if tc.component == "link/.." {
					target = filepath.Join(dir, "target")
					require.NoError(t, os.MkdirAll(filepath.Join(target, "nested"), 0o700))
					require.NoError(t, os.Symlink(filepath.Join(target, "nested"), filepath.Join(dir, "link")))
				}
				actual, decoy := filepath.Join(target, tc.suffix), filepath.Join(dir, tc.suffix)
				if relative {
					actual, decoy, cwd = decoy, actual, target
				}
				for _, path := range []string{actual, decoy} {
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
					require.NoError(t, os.WriteFile(path, []byte("[safe]\n directory = /trusted\n"), 0o600))
				}
				raw := dir + "/" + tc.component
				if tc.key == "GIT_CONFIG_GLOBAL" || tc.key == "GIT_CONFIG_SYSTEM" {
					raw += "/" + tc.suffix
				}
				if relative {
					raw = tc.suffix
				}
				env := slices.DeleteFunc(safeDirectoryTestEnv(t, actual), func(entry string) bool { return strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=") })
				env = append(env, "HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "xdg"), tc.key+"="+raw)
				if tc.key == "GIT_CONFIG_SYSTEM" {
					env = append(env, "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "absent"))
				}
				runner := New()
				if relative {
					env = append(env, "GIT_CONFIG_NOSYSTEM=1")
					runner.DisableSafeDirectoryForward = true
				}
				runner.Env = env
				if relative {
					_, err := runner.Output(t.Context(), dir, "init")
					require.NoError(t, err)
				}
				for range 3 {
					assert.Equal(t, []string{"/trusted"}, runner.trust.read(t.Context(), env, cwd))
				}
				require.NoError(t, os.WriteFile(actual, nil, 0o600))
				fresh := readSafeDirectories(t.Context(), env, cwd)
				assert.Empty(t, fresh)
				assert.Equal(t, fresh, runner.trust.read(t.Context(), env, cwd))
			})
		}
	})
}

const trustGitForwarder = `
func forwardGit() {
 cmd := exec.Command(os.Getenv("TRUST_TEST_GIT"), os.Args[1:]...)
 cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, os.Environ()
 if err := cmd.Run(); err != nil { if exit, ok := err.(*exec.ExitError); ok { os.Exit(exit.ExitCode()) }; os.Exit(2) }
}
`

func coordinatedTrustGit(t *testing.T) (Runner, chan chan struct{}) {
	t.Helper()
	extendTrustProbeTimeout(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	started := make(chan chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := make(chan struct{})
		started <- release
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	bin := buildTestGit(t, `package main
import ("net/http"; "os"; "os/exec")
func main() {
 if len(os.Args) > 1 && (os.Args[1] == "config" || os.Args[1] == "var" && os.Getenv("TRUST_TEST_BLOCK_VAR") == "1") {
  response, err := http.Get(os.Getenv("TRUST_TEST_URL"))
  if err != nil { os.Exit(2) }; response.Body.Close()
 }
 forwardGit()
 if len(os.Args) > 1 && (os.Args[1] == "config" && os.Getenv("TRUST_TEST_BLOCK_AFTER") == "1" || os.Args[1] == "var" && os.Getenv("TRUST_TEST_BLOCK_VAR_AFTER") == "1") { response, err := http.Get(os.Getenv("TRUST_TEST_URL")); if err != nil { os.Exit(2) }; response.Body.Close() }
}
`+trustGitForwarder)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
	runner := New()
	runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_CONFIG_NOSYSTEM=1", "TRUST_TEST_URL="+server.URL, "TRUST_TEST_GIT="+realGit)
	return runner, started
}

func TestCachedTrustConcurrentEvaluation(t *testing.T) {
	for _, mode := range []string{"canceled fill", "canceled discovery", "competing environments", "reverted edit", "reverted missing", "home interruption"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "home interruption" && runtime.GOOS != "windows" {
				t.Skip("requires Git for Windows HOME selection")
			}
			runner, started := coordinatedTrustGit(t)
			if mode == "home interruption" {
				dir := t.TempDir()
				var share string
				runner.Env, share, _ = trustHomeShare(t, runner.Env)
				trace := filepath.Join(dir, "trace")
				runner.Env = append(runner.Env, "GIT_TRACE="+filepath.ToSlash(trace), "TRUST_TEST_BLOCK_VAR=1", "TRUST_TEST_BLOCK_VAR_AFTER=1")
				result := make(chan []string, 1)
				read := func() { go func() { result <- runner.trust.read(t.Context(), runner.Env, dir) }() }
				read()
				close(<-started)
				assert.Equal(t, []string{"/share"}, <-result)
				read()
				before := <-started
				require.NoError(t, os.Rename(share, share+"-offline"))
				close(before)
				after := <-started
				require.NoError(t, os.Rename(share+"-offline", share))
				close(after)
				close(<-started)
				assert.Equal(t, []string{"/share"}, <-result)
				require.Nil(t, runner.trust.current.scopes)
				read()
				close(<-started)
				close(<-started)
				close(<-started)
				assert.Equal(t, []string{"/share"}, <-result)
				beforeTrace, err := os.ReadFile(trace)
				require.NoError(t, err)
				assert.Equal(t, []string{"/share"}, runner.trust.read(t.Context(), runner.Env, dir))
				afterTrace, err := os.ReadFile(trace)
				require.NoError(t, err)
				assert.Equal(t, beforeTrace, afterTrace)
				return
			}
			missing := mode == "reverted missing"
			if missing {
				config, _ := envValue(runner.Env, "GIT_CONFIG_GLOBAL")
				require.NoError(t, os.Remove(config))
			}

			if mode != "competing environments" {
				if strings.HasPrefix(mode, "reverted") {
					runner.Env = append(runner.Env, "TRUST_TEST_BLOCK_AFTER=1")
				}
				if mode == "canceled discovery" {
					runner.Env = append(runner.Env, "TRUST_TEST_BLOCK_VAR=1")
				}
				result := make(chan []string, 1)
				go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
				close(<-started)
				if strings.HasPrefix(mode, "reverted") && !missing {
					close(<-started)
				}
				want := []string{"/trusted"}
				if missing {
					want = nil
				}
				assert.Equal(t, want, <-result)
				if strings.HasPrefix(mode, "reverted") {
					config, _ := envValue(runner.Env, "GIT_CONFIG_GLOBAL")
					go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
					if !missing {
						close(<-started)
						close(<-started)
					}
					assert.Equal(t, want, <-result)
					if !missing {
						require.NoError(t, os.Rename(config, config+".original"))
					}
					require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /b\n"), 0o600))
					go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
					close(<-started)
					after := <-started
					require.NoError(t, os.Remove(config))
					if !missing {
						require.NoError(t, os.Rename(config+".original", config))
					}
					close(after)
					assert.Equal(t, []string{"/b"}, <-result)
					go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
					if !missing {
						close(<-started)
						close(<-started)
					}
					assert.Equal(t, want, <-result)
					return
				}
				if mode == "canceled fill" {
					config, _ := envValue(runner.Env, "GIT_CONFIG_GLOBAL")
					require.NoError(t, os.WriteFile(config, []byte(strings.Repeat("# comment\n", 1<<15)+"[safe]\n directory = /trusted\n"), 0o600))
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() { result <- runner.trust.read(ctx, runner.Env, "") }()
				<-started
				cancel()
				assert.Empty(t, <-result)
				assert.Nil(t, runner.trust.current.scopes)
				go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
				close(<-started)
				if mode == "canceled discovery" {
					close(<-started)
				}
				assert.Equal(t, []string{"/trusted"}, <-result)
				return
			}
			other := runner
			config := filepath.Join(t.TempDir(), "gitconfig")
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /other\n"), 0o600))
			other.Env = append(append([]string(nil), runner.Env...), "GIT_CONFIG_GLOBAL="+config)
			one, two := make(chan []string, 1), make(chan []string, 1)
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			close(<-started)
			assert.Equal(t, []string{"/trusted"}, <-one)
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			first := <-started
			go func() { two <- other.trust.read(t.Context(), other.Env, "") }()
			close(first)
			assert.Equal(t, []string{"/trusted"}, <-one)
			second := <-started
			close(second)
			assert.Equal(t, []string{"/other"}, <-two)
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			assert.Equal(t, []string{"/trusted"}, <-one)
		})
	}
}
