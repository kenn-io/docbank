package gitcmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitenv "go.kenn.io/kit/git/env"
	"go.kenn.io/kit/git/internal/shellquote"
)

func TestRunnerCommandUsesDefensiveEnvironment(t *testing.T) {
	require := require.New(t)
	runner := New()
	runner.Env = []string{
		"PATH=/bin",
		"GIT_DIR=/parent/.git",
		"GIT_SSL_NO_VERIFY=1",
		"SSH_ASKPASS=/tmp/askpass",
	}

	cmd := runner.Command(t.Context(), "", "status")

	if slices.Contains(cmd.Env, "GIT_DIR=/parent/.git") {
		require.FailNow(fmt.Sprintf("GIT_DIR should have been stripped: %#v", cmd.Env))
	}
	if slices.Contains(cmd.Env, "GIT_SSL_NO_VERIFY=1") {
		require.FailNow(fmt.Sprintf("GIT_SSL_NO_VERIFY should have been stripped: %#v", cmd.Env))
	}
	if !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		require.FailNow(fmt.Sprintf("terminal prompts should be disabled: %#v", cmd.Env))
	}
	if !slices.Contains(cmd.Env, "GIT_CONFIG_GLOBAL="+nullGlobalConfigPath()) {
		require.FailNow(fmt.Sprintf("global config should be nulled: %#v", cmd.Env))
	}
	if !containsPrefix(cmd.Env, "GIT_CONFIG_COUNT=") {
		require.FailNow(fmt.Sprintf("temporary git config should be injected: %#v", cmd.Env))
	}
}

func TestRunnerPreservesInheritedCommandScopeConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	runner := New()
	runner.StripEnv = false
	runner.DisableSafeDirectoryForward = true
	runner.Env = append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=filter.inherited.smudge",
		"GIT_CONFIG_VALUE_0=inherited",
	)
	runner = runner.WithConfig("filter.added.clean", "added")

	inherited, err := runner.Output(
		t.Context(), "", "config", "--get", "filter.inherited.smudge",
	)
	require.NoError(err)
	assert.Equal("inherited", strings.TrimSpace(string(inherited)))

	added, err := runner.Output(
		t.Context(), "", "config", "--get", "filter.added.clean",
	)
	require.NoError(err)
	assert.Equal("added", strings.TrimSpace(string(added)))
}

func TestRunnerCancellationStopsGitHTTPSubprocesses(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "git-trace.json")
	runner := New()
	runner.DisableSafeDirectoryForward = true
	runner.StripEnv = false
	runner.Env = append(gitenv.StripAll(os.Environ()), "GIT_TRACE2_EVENT="+tracePath)
	t.Cleanup(func() {
		if t.Failed() {
			trace, err := os.ReadFile(tracePath)
			t.Logf("Git process trace (read error: %v):\n%s", err, trace)
		}
	})
	requestStarted := make(chan struct{})
	requestDone := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "001e# service=git-upload-pack\n00000040")
		w.(http.Flusher).Flush()
		close(requestStarted)
		select {
		case <-r.Context().Done():
			close(requestDone)
		case <-release:
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	defer func() {
		cancel()
		close(release)
		<-result
	}()
	go func() {
		defer close(result)
		_, _, err := runner.Run(ctx, dir, nil, "ls-remote", server.URL+"/repo.git")
		result <- err
	}()

	// Git's HTTP helper can take several seconds to start on busy Windows runners.
	select {
	case <-requestStarted:
	case err := <-result:
		require.FailNow("git exited before starting its HTTP request", "git result: %v", err)
	case <-time.After(30 * time.Second):
		require.FailNow("git did not start its HTTP request")
	}
	canceledAt := time.Now()
	cancel()

	select {
	case err := <-result:
		t.Logf("Git returned %s after cancellation: %v", time.Since(canceledAt), err)
		require.ErrorIs(err, context.Canceled)
	case <-time.After(3 * time.Second):
		require.FailNow("git did not stop after context cancellation")
	}

	select {
	case <-requestDone:
	case <-time.After(time.Second):
		require.FailNow("git HTTP subprocess remained connected after cancellation")
	}
}

func TestRunnerInheritedSafeDirectoryReset(t *testing.T) {
	for _, tc := range []struct {
		name, global, inherited, want string
	}{
		{"inherited reset", "[safe]\n directory = /forwarded/repository\n", "", "\x00/forwarded/repository\x00\x00"},
		{"lower scope reset", "[safe]\n directory =\n directory = /lower/repository\n", "/command/repository", "/command/repository\x00\x00/lower/repository\x00/command/repository\x00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalConfig := filepath.Join(t.TempDir(), "gitconfig")
			require.NoError(t, os.WriteFile(globalConfig, []byte(tc.global), 0o600))
			runner := New()
			runner.StripEnv = false
			runner.Env = append(safeDirectoryTestEnv(t, globalConfig),
				"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0="+tc.inherited)
			out, err := runner.Output(t.Context(), "", "config", "--null", "--get-all", "safe.directory")
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.want), out, "inherited trust must follow the forwarded lower-scope entries")
		})
	}
}

func TestEnvValueForGOOSHonorsPlatformKeyCasing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := []string{
		"git_config_count=1",
		"GIT_CONFIG_COUNT=2",
	}

	value, ok := envValueForGOOS(env, "GIT_CONFIG_COUNT", "windows")
	require.True(ok)
	assert.Equal("2", value)

	value, ok = envValueForGOOS(
		[]string{"git_config_count=1"}, "GIT_CONFIG_COUNT", "windows",
	)
	require.True(ok)
	assert.Equal("1", value)

	_, ok = envValueForGOOS(
		[]string{"git_config_count=1"}, "GIT_CONFIG_COUNT", "linux",
	)
	assert.False(ok)
}

func TestNullGlobalConfigHidesGlobalConfig(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[fixture]\n\tkey = from-global\n"), 0o600))
	runner := Runner{Env: safeDirectoryTestEnv(t, globalConfig), DisableSafeDirectoryForward: true}
	dir := t.TempDir()

	out, err := runner.Output(t.Context(), dir, "config", "--get", "fixture.key")
	require.NoError(t, err, "the fixture global config must be visible without NullGlobalConfig")
	require.Equal(t, "from-global", strings.TrimSpace(string(out)))

	runner.NullGlobalConfig = true
	out, err = runner.Output(t.Context(), dir, "config", "--list")
	require.NoError(t, err)
	assert.NotContains(t, string(out), "fixture.key")
}

func TestNullGlobalConfigRejectsGlobalWrites(t *testing.T) {
	path := nullGlobalConfigPath()
	// git writes config by renaming "<path>.lock" over "<path>". Only attempt
	// the write when the parent cannot hold either file.
	info, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	require.False(t, info.IsDir(), "GIT_CONFIG_GLOBAL %q must not be creatable", path)

	runner := New()
	runner.DisableSafeDirectoryForward = true
	dir := t.TempDir()
	_, _, err = runner.Run(t.Context(), dir, nil, "config", "--global", "fixture.key", "written")
	require.Error(t, err)
	out, _ := runner.Output(t.Context(), dir, "config", "--list")
	assert.NotContains(t, string(out), "fixture.key")
}

func TestNullGlobalConfigLeavesNoTempFilePerProcess(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	tempDir := t.TempDir()
	env := append(os.Environ(),
		"GITCMD_NULL_GLOBAL_HELPER=1", "TMPDIR="+tempDir, "TMP="+tempDir, "TEMP="+tempDir,
	)
	for range 3 {
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestNullGlobalConfigHelper$")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "each process must not leave a global config file behind")
}

func TestNullGlobalConfigHelper(t *testing.T) {
	if os.Getenv("GITCMD_NULL_GLOBAL_HELPER") == "" {
		return
	}
	runner := New()
	runner.DisableSafeDirectoryForward = true
	_, err := runner.Output(t.Context(), "", "version")
	require.NoError(t, err)
}

// safeDirectoryTestEnv builds a hermetic environment for safe.directory
// reads: global config comes from the given file and the system scope is
// pinned to an empty file so entries baked into the host's real system config
// (for example "safe.directory = *" on GitHub-hosted runners) cannot leak in.
func safeDirectoryTestEnv(t *testing.T, globalConfig string) []string {
	t.Helper()
	emptySystemConfig := filepath.Join(t.TempDir(), "system-gitconfig")
	require.NoError(t, os.WriteFile(emptySystemConfig, nil, 0o600))
	return append(gitenv.StripAll(os.Environ()),
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_CONFIG_SYSTEM="+emptySystemConfig,
		"GIT_CONFIG_NOSYSTEM=0",
	)
}

func TestReadSafeDirectories(t *testing.T) {
	for _, tc := range []struct {
		name, global, noSystem string
		want                   []string
	}{
		{"global", "[safe]\n directory = *\n directory = /srv/repo\n", "1", []string{"*", "/srv/repo"}},
		{"unset", "", "1", nil},
		{"system scope", "[safe]\n directory = /home/repo\n", "0", []string{"/etc/repo", "/home/repo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalConfig := filepath.Join(t.TempDir(), "gitconfig")
			require.NoError(t, os.WriteFile(globalConfig, []byte(tc.global), 0o600))
			env := safeDirectoryTestEnv(t, globalConfig)
			systemConfig, _ := envValue(env, "GIT_CONFIG_SYSTEM")
			require.NoError(t, os.WriteFile(systemConfig, []byte("[safe]\n directory = /etc/repo\n"), 0o600))
			env = append(env, "GIT_CONFIG_NOSYSTEM="+tc.noSystem)
			assert.Equal(t, tc.want, readSafeDirectories(t.Context(), env, ""))
		})
	}
}

func TestReadSafeDirectoriesBoundsProbeRuntime(t *testing.T) {
	origTimeout := safeDirectoryProbeTimeout
	safeDirectoryProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { safeDirectoryProbeTimeout = origTimeout })

	binDir := buildSleepingGit(t)
	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	env := append(os.Environ(), "PATH="+pathEnv, "GIT_CONFIG_NOSYSTEM=0")

	start := time.Now()
	got := readSafeDirectories(t.Context(), env, "")

	assert.Empty(t, got)
	// Windows process teardown can take more than a second on a busy CI runner.
	// This remains well below the sleeping fixture's ten-second runtime.
	assert.Less(t, time.Since(start), 5*time.Second, "safe.directory probes are best-effort and must not stall git commands")
}

func TestReadSafeDirectoriesConditionalInclude(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Regression test: the probes must run in the command's directory with
	// --includes so includeIf "gitdir:..." entries resolve for the repository
	// the command targets, not for the calling process's working directory.
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	// Git exports repository-local variables to hooks. The fixture environment
	// must discard them before it starts or probes its own repository.
	t.Setenv("GIT_DIR", filepath.Join(dir, "hook-repository.git"))
	require.NoError(os.Mkdir(repo, 0o755))
	// git matches gitdir patterns against resolved paths, so the pattern must
	// use the symlink-free form (t.TempDir is a symlink on macOS).
	realRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(err)

	included := filepath.Join(dir, "trusted-gitconfig")
	require.NoError(os.WriteFile(included, []byte("[safe]\n\tdirectory = /srv/conditional\n"), 0o600))
	globalConfig := filepath.Join(dir, "gitconfig")
	require.NoError(os.WriteFile(globalConfig, []byte(
		"[includeIf \"gitdir:"+filepath.ToSlash(realRepo)+"/\"]\n\tpath = "+filepath.ToSlash(included)+"\n"), 0o600))
	env := safeDirectoryTestEnv(t, globalConfig)

	runner := New()
	runner.Env = env
	_, _, err = runner.Run(t.Context(), repo, nil, "init")
	require.NoError(err)

	assert.Equal([]string{"/srv/conditional"}, readSafeDirectories(t.Context(), env, repo),
		"include conditional on the target repo must apply")
	assert.Empty(readSafeDirectories(t.Context(), env, dir),
		"include conditional on another repo must not apply")
}

func TestCommandEnvForwardsSafeDirectory(t *testing.T) {
	for _, tc := range []struct {
		name              string
		literal, disabled bool
		want              string
	}{
		{"forwarded", false, false, "*"},
		{"runner literal", true, false, "*"},
		{"disabled", false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			globalConfig := filepath.Join(t.TempDir(), "gitconfig")
			require.NoError(t, os.WriteFile(globalConfig, []byte("[safe]\n directory = *\n"), 0o600))
			runner := New()
			if tc.literal {
				runner = Runner{StripEnv: true, NullGlobalConfig: true, NoSystemConfig: true}
			}
			runner.Env = safeDirectoryTestEnv(t, globalConfig)
			runner.DisableSafeDirectoryForward = tc.disabled
			cmd := runner.Command(t.Context(), "", "status")
			assert.Equal(t, tc.want, gitConfigValue(strings.Join(cmd.Env, "\n"), "safe.directory"))
			assert.Contains(t, cmd.Env, "GIT_CONFIG_GLOBAL="+nullGlobalConfigPath(),
				"the sanitized environment must still hide the user's global config")
		})
	}
}

func TestCommandEnvReadsSafeDirectoryFromRunnerEnv(t *testing.T) {
	dir := t.TempDir()
	trusted := filepath.Join(dir, "trusted-gitconfig")
	require.NoError(t, os.WriteFile(trusted, []byte("[safe]\n directory = /trusted/repo\n"), 0o600))
	empty := filepath.Join(dir, "empty-gitconfig")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	trustedRunner := New()
	trustedRunner.Env = safeDirectoryTestEnv(t, trusted)
	emptyRunner := New()
	emptyRunner.Env = safeDirectoryTestEnv(t, empty)
	forwarded := func(runner Runner) string {
		cmd := runner.Command(t.Context(), "", "status")
		return gitConfigValue(strings.Join(cmd.Env, "\n"), "safe.directory")
	}
	// Repeat past cache warmup so reused entries cannot leak between runners.
	for range 3 {
		assert.Equal(t, "/trusted/repo", forwarded(trustedRunner))
		assert.Empty(t, forwarded(emptyRunner))
	}
}

func TestCredentialResponseIsPrivateDataAndCleanupIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	path, cleanup, err := (basicAuth{
		username: "alice",
		password: "secret-token",
	}).credentialResponse()
	require.NoError(err)
	t.Cleanup(cleanup)

	info, err := os.Stat(path)
	require.NoError(err)
	assert.True(info.Mode().IsRegular())
	assert.Zero(info.Mode() & 0o111)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
	contents, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal("username=alice\npassword=secret-token\n", string(contents))

	cleanup()
	cleanup()
	_, err = os.Stat(path)
	assert.ErrorIs(err, os.ErrNotExist)
}

func TestCredentialResponseRejectsProtocolDelimitersBeforeCreatingFile(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		field    string
		secret   string
	}{
		{name: "username LF", username: "ali\nce", password: "token", field: "username", secret: "ali\nce"},
		{name: "username CR", username: "ali\rce", password: "token", field: "username", secret: "ali\rce"},
		{name: "username NUL", username: "ali\x00ce", password: "token", field: "username", secret: "ali\x00ce"},
		{name: "password LF", username: "alice", password: "token\nquit=1", field: "password", secret: "token\nquit=1"},
		{name: "password CR", username: "alice", password: "token\rquit=1", field: "password", secret: "token\rquit=1"},
		{name: "password NUL", username: "alice", password: "token\x00quit=1", field: "password", secret: "token\x00quit=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			tempDir := t.TempDir()
			t.Setenv("TMPDIR", tempDir)
			t.Setenv("TMP", tempDir)
			t.Setenv("TEMP", tempDir)

			path, cleanup, err := (basicAuth{
				username: tt.username,
				password: tt.password,
			}).credentialResponse()
			require.Error(err)
			assert.Empty(path)
			assert.Contains(err.Error(), tt.field)
			assert.NotContains(err.Error(), tt.secret)
			assert.NotPanics(cleanup)

			entries, readErr := os.ReadDir(tempDir)
			require.NoError(readErr)
			assert.Empty(entries)
		})
	}
}

func TestWithBasicAuthKeepsSecretOutOfCommandEnvironment(t *testing.T) {
	assert := assert.New(t)
	env := captureGitEnv(t, New().WithBasicAuth("alice", "secret-token"))

	for _, secret := range []string{
		"alice",
		"secret-token",
		base64.StdEncoding.EncodeToString([]byte("alice:secret-token")),
		"Authorization: Basic",
		"http.extraHeader",
	} {
		assert.NotContains(env, secret)
	}
	helper := gitConfigValue(env, "credential.helper")
	assert.NotEmpty(helper)
	assert.True(strings.HasPrefix(helper, "!"), "credential helper must be an inline shell snippet, got %q", helper)
}

func TestWithBasicAuthRoundTripsCredentialProtocol(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	username := `al ice'$\"`
	password := `sec ret'\"$\\;|&()`
	request := "protocol=https\nhost=example.invalid\n\n"

	stdout, stderr, err := New().WithBasicAuth(username, password).Run(
		t.Context(), "", strings.NewReader(request), "credential", "fill",
	)

	require.NoError(err, string(stderr))
	assert.Empty(stderr)
	assert.Contains(string(stdout), "username="+username+"\n")
	assert.Contains(string(stdout), "password="+password+"\n")
}

func TestWithBasicAuthStoreAndEraseDoNotDiscloseCredentials(t *testing.T) {
	request := "protocol=https\nhost=example.invalid\nusername=alice\npassword=secret-token\n\n"
	for _, operation := range []string{"approve", "reject"} {
		t.Run(operation, func(t *testing.T) {
			stdout, stderr, err := New().WithBasicAuth("alice", "secret-token").Run(
				t.Context(), "", strings.NewReader(request), "credential", operation,
			)
			require.NoError(t, err, string(stderr))
			assert.Empty(t, stdout)
			assert.Empty(t, stderr)
		})
	}
}

func TestWithBasicAuthRejectsCredentialProtocolInjection(t *testing.T) {
	assert := assert.New(t)
	password := "secret-token\nusername=mallory"
	request := "protocol=https\nhost=example.invalid\n\n"

	stdout, stderr, err := New().WithBasicAuth("alice", password).Run(
		t.Context(), "", strings.NewReader(request), "credential", "fill",
	)

	require.Error(t, err)
	assert.Empty(stdout)
	assert.NotContains(string(stderr), password)
	assert.NotContains(err.Error(), password)
}

func TestWithBasicAuthRejectsCommand(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil {
			require.FailNow(t, "Command with basic auth did not panic")
		}
		message := got.(string)
		for _, secret := range []string{"alice", "secret-token", base64.StdEncoding.EncodeToString([]byte("alice:secret-token"))} {
			if strings.Contains(message, secret) {
				require.FailNow(t, fmt.Sprintf("panic leaked %q: %s", secret, message))
			}
		}
	}()

	New().WithBasicAuth("alice", "secret-token").Command(t.Context(), "", "status")
}

func TestWithBasicAuthRemovesCredentialResponseAfterRun(t *testing.T) {
	// The process-wide config must outlive this fixture's temporary directory.
	_ = nullGlobalConfigPath()
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)

	captureGitEnv(t, New().WithBasicAuth("alice", "secret-token"))

	responses, err := filepath.Glob(filepath.Join(tempDir, "gitcmd-credential-response-*"))
	require.NoError(t, err)
	assert.Empty(t, responses)
}

func TestWithBasicAuthRemovesCredentialResponseAfterGitFailure(t *testing.T) {
	_ = nullGlobalConfigPath()
	require := require.New(t)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)

	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nexit 1\n"
	if runtime.GOOS == "windows" {
		gitPath += ".bat"
		script = "@exit /b 1\r\n"
	}
	require.NoError(os.WriteFile(gitPath, []byte(script), 0o700))

	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	runner := New().WithBasicAuth("alice", "secret-token")
	runner.Env = []string{"PATH=" + pathEnv}
	_, _, err := runner.Run(t.Context(), "", nil, "version")
	require.Error(err)

	responses, globErr := filepath.Glob(filepath.Join(tempDir, "gitcmd-credential-response-*"))
	require.NoError(globErr)
	assert.Empty(t, responses)
}

func captureGitEnv(t *testing.T, runner Runner) string {
	t.Helper()
	binDir := t.TempDir()
	envPath := filepath.Join(t.TempDir(), "env")
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nenv > " + shellquote.Single(envPath) + "\n"
	if os.PathSeparator == '\\' {
		gitPath += ".bat"
		script = "@echo off\r\nset > " + shellDoubleQuote(envPath) + "\r\n"
	}
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		require.FailNow(t, err.Error())
	}

	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	runner.Env = []string{"PATH=" + pathEnv}
	if _, _, err := runner.Run(t.Context(), "", nil, "version"); err != nil {
		require.FailNow(t, err.Error())
	}
	envBytes, err := os.ReadFile(envPath)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	return string(envBytes)
}

func buildSleepingGit(t *testing.T) string {
	t.Helper()
	return buildTestGit(t, `package main
import "time"
func main() { time.Sleep(10 * time.Second) }
`)
}

func buildTestGit(t *testing.T, source string) string {
	t.Helper()
	binDir := t.TempDir()
	exeName := "git"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	exePath := filepath.Join(binDir, exeName)
	srcPath := filepath.Join(t.TempDir(), "main.go")
	require.NoError(t, os.WriteFile(srcPath, []byte(source), 0o600))
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", exePath, srcPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return binDir
}

func gitConfigValue(env, key string) string {
	values := map[string]string{}
	keys := map[string]string{}
	for line := range strings.SplitSeq(env, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSuffix(value, "\r")
		if index, ok := strings.CutPrefix(name, "GIT_CONFIG_KEY_"); ok {
			keys[index] = value
		}
		if index, ok := strings.CutPrefix(name, "GIT_CONFIG_VALUE_"); ok {
			values[index] = value
		}
	}
	for index, gotKey := range keys {
		if gotKey == key {
			return values[index]
		}
	}
	return ""
}

func containsPrefix(values []string, prefix string) bool {
	return slices.ContainsFunc(values, func(value string) bool {
		return strings.HasPrefix(value, prefix)
	})
}

func shellDoubleQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
