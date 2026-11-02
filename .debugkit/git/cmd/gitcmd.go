// Package gitcmd runs git subprocesses with defensive defaults.
//
// The default Runner strips inherited GIT_* environment variables, disables
// interactive prompts, ignores global and system git config, and injects
// temporary config through GIT_CONFIG_* variables. This prevents child git
// commands from accidentally binding to a parent repository or writing into a
// developer's global config during automation and tests. The user's
// safe.directory entries are forwarded into the sanitized environment so
// config isolation does not make git reject repositories the user already
// trusts (for example root-squashed or container-mounted checkouts owned by
// another user).
package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	gitenv "go.kenn.io/kit/git/env"
	"go.kenn.io/kit/git/internal/shellquote"
)

// Config is one temporary git config entry injected through GIT_CONFIG_* env.
type Config struct {
	// Key is the git config key, for example "gc.auto".
	Key string
	// Value is the git config value.
	Value string
}

// Runner builds git commands with a sanitized environment.
type Runner struct {
	// Env is the base process environment. When nil, os.Environ is used.
	Env []string
	// Config is appended to the default temporary git config entries.
	Config []Config
	// StripEnv removes inherited GIT_* variables before running git.
	StripEnv bool
	// TerminalPrompt allows interactive git prompts when true.
	TerminalPrompt bool
	// NullGlobalConfig makes git read no global config and refuse global
	// config writes when true, by pointing GIT_CONFIG_GLOBAL at a path that
	// cannot exist.
	NullGlobalConfig bool
	// NoSystemConfig sets GIT_CONFIG_NOSYSTEM=1 when true.
	NoSystemConfig bool
	// DisableSafeDirectoryForward turns off re-injecting the user's
	// safe.directory entries from system and global git config as
	// command-scope config. Forwarding is the default because NullGlobalConfig
	// and NoSystemConfig hide those entries and git then refuses to operate on
	// repositories owned by another user ("detected dubious ownership"), even
	// though plain git works for the same user.
	DisableSafeDirectoryForward bool
	// WaitDelay bounds pipe draining after Git exits or is canceled. Zero
	// preserves the default of one second.
	WaitDelay time.Duration
	// StdoutLimit and StderrLimit bound captured output in Run and Output.
	// Nonpositive values are unlimited. Excess bytes are drained and discarded;
	// the result includes the corresponding limit error even if Git succeeds.
	StdoutLimit int
	StderrLimit int
	// AcceptSuccessfulWaitDelay accepts a successful Git exit whose inherited
	// output pipes did not close before WaitDelay. Cancellation and nonzero
	// process exits still fail. This does not affect Command's caller-owned Wait.
	AcceptSuccessfulWaitDelay bool

	basicAuth *basicAuth
	trust     *safeDirectoryCache
}

// New returns a Runner with safe automation defaults.
// Trust reuse is pinned to the first observed environment and Git executable
// for the lifetime of this runner and its copies. Other identities always read
// fresh config. Edits affect the next command in either case.
func New() Runner {
	return Runner{
		Env:              os.Environ(),
		StripEnv:         true,
		NullGlobalConfig: true,
		NoSystemConfig:   true,
		trust:            &safeDirectoryCache{},
	}
}

// WithEnv returns a copy of r with a snapshot of env and an independent trust
// cache. Use it when changing the environment of an already-used runner so the
// new environment can reuse trust reads without affecting the original runner.
// A nil env retains the default of using os.Environ when each command runs.
func (r Runner) WithEnv(env []string) Runner {
	r.Env = slices.Clone(env)
	r.trust = &safeDirectoryCache{}
	return r
}

// WithConfig returns a copy of r with an additional temporary git config value.
func (r Runner) WithConfig(key, value string) Runner {
	r.Config = append(append([]Config(nil), r.Config...), Config{Key: key, Value: value})
	return r
}

// WithBasicAuth returns a copy of r with credentials supplied through a
// short-lived git credential helper that reads them from a user-only,
// non-executable response file, cleaned up when the command finishes.
func (r Runner) WithBasicAuth(username, password string) Runner {
	r.basicAuth = &basicAuth{username: username, password: password}
	return r
}

// Command constructs a git command in dir.
//
// Command cannot be used with WithBasicAuth because callers would not have a
// way to clean up the temporary credential helper. Use Run or Output instead.
func (r Runner) Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	if r.basicAuth != nil {
		panic("gitcmd: Command cannot be used with WithBasicAuth; use Run or Output so credentials can be cleaned up")
	}
	cmd := gitCommand(ctx, !r.TerminalPrompt, args...)
	if r.WaitDelay != 0 {
		cmd.WaitDelay = r.WaitDelay
	}
	cmd.Dir = dir
	cmd.Env, _ = r.commandEnv(ctx, dir)
	return cmd
}

// Output runs git and returns stdout.
func (r Runner) Output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	stdout, _, err := r.Run(ctx, dir, nil, args...)
	return stdout, err
}

// Run runs git and returns stdout, stderr, and a *GitError on failure.
func (r Runner) Run(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	cmd := gitCommand(ctx, !r.TerminalPrompt, args...)
	if r.WaitDelay != 0 {
		cmd.WaitDelay = r.WaitDelay
	}
	cmd.Dir = dir
	var cleanup func()
	cmd.Env, cleanup = r.commandEnv(ctx, dir)
	defer cleanup()
	cmd.Stdin = stdin

	stdout := outputBuffer{limit: r.StdoutLimit}
	stderr := outputBuffer{limit: r.StderrLimit}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// bytes.Buffer.ReadFrom avoids io.Copy's 32 KiB scratch allocation for
	// each pipe. Only use it when there is no capture limit to enforce.
	if r.StdoutLimit <= 0 {
		cmd.Stdout = &stdout.buffer
	}
	if r.StderrLimit <= 0 {
		cmd.Stderr = &stderr.buffer
	}

	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	} else if r.AcceptSuccessfulWaitDelay && errors.Is(err, exec.ErrWaitDelay) &&
		cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	if stdout.exceeded {
		err = errors.Join(err, ErrStdoutLimitExceeded)
	}
	if stderr.exceeded {
		err = errors.Join(err, ErrStderrLimitExceeded)
	}
	if err != nil {
		return stdout.buffer.Bytes(), stderr.buffer.Bytes(), &GitError{
			Dir:          dir,
			Args:         append([]string(nil), args...),
			Stderr:       strings.TrimSpace(stderr.buffer.String()),
			Err:          err,
			processState: cmd.ProcessState,
		}
	}
	return stdout.buffer.Bytes(), stderr.buffer.Bytes(), nil
}

var (
	// ErrStdoutLimitExceeded reports truncated captured standard output.
	ErrStdoutLimitExceeded = errors.New("git stdout limit exceeded")
	// ErrStderrLimitExceeded reports truncated captured standard error.
	ErrStderrLimitExceeded = errors.New("git stderr limit exceeded")
)

// outputBuffer keeps a prefix while continuing to drain a subprocess pipe.
// Do not embed bytes.Buffer: its promoted ReadFrom would bypass the limit.
type outputBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit > 0 && n > b.limit-b.buffer.Len() {
		b.exceeded = true
		p = p[:b.limit-b.buffer.Len()]
	}
	_, err := b.buffer.Write(p)
	return n, err
}

func gitCommand(ctx context.Context, hideConsoleWindow bool, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = gitCommandWaitDelay
	prepareGitCommand(cmd, hideConsoleWindow)
	return cmd
}

const gitCommandWaitDelay = time.Second

type basicAuth struct {
	username string
	password string
}

func validateCredentialValue(field, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s contains a character unsupported by the git credential protocol", field)
	}
	return nil
}

func (a basicAuth) credentialResponse() (string, func(), error) {
	noCleanup := func() {}
	if err := validateCredentialValue("username", a.username); err != nil {
		return "", noCleanup, err
	}
	if err := validateCredentialValue("password", a.password); err != nil {
		return "", noCleanup, err
	}

	file, err := os.CreateTemp("", "gitcmd-credential-response-*")
	if err != nil {
		return "", noCleanup, err
	}
	path := file.Name()
	cleanup := func() {
		_ = os.Remove(path)
	}

	_, writeErr := fmt.Fprintf(file, "username=%s\npassword=%s\n", a.username, a.password)
	closeErr := file.Close()
	if writeErr != nil {
		cleanup()
		return "", noCleanup, writeErr
	}
	if closeErr != nil {
		cleanup()
		return "", noCleanup, closeErr
	}
	return path, cleanup, nil
}

func credentialHelper(path string) string {
	return `!f() { ` +
		`while IFS= read -r line && [ -n "$line" ]; do :; done; ` +
		`if [ "$1" = get ]; then ` +
		`while IFS= read -r line; do printf '%s\n' "$line"; done < ` + shellquote.Single(path) + `; ` +
		`fi; ` +
		`}; f`
}

var (
	emptyGlobalConfigOnce sync.Once
	emptyGlobalConfigPath string
)

var safeDirectoryProbeTimeout = 2 * time.Second

// nullGlobalConfigPath returns a GIT_CONFIG_GLOBAL value that makes git read
// no global config and refuse to write one.
//
// The value is a path beneath a file that is not a directory, so the path can
// never exist. git treats a global config it cannot find as absent, and a
// "git config --global" write fails because git cannot create its lock file
// there. No file is created, so short-lived processes leave nothing behind.
//
// Pointing at the null device itself is not safe: git writes config by
// renaming "<path>.lock" over "<path>", so a write run as root would replace
// /dev/null. Outside Windows the parent is os.DevNull. On Windows os.DevNull is
// "NUL", and some Git for Windows builds — notably on ARM64 — fail with
// "fatal: unable to access 'NUL': Invalid argument", so the parent is the
// running executable instead. If the executable path is unknown, a
// process-private empty temporary file is used.
func nullGlobalConfigPath() string {
	if runtime.GOOS != "windows" {
		return filepath.Join(os.DevNull, "gitconfig")
	}
	emptyGlobalConfigOnce.Do(func() {
		if executable, err := os.Executable(); err == nil {
			emptyGlobalConfigPath = filepath.Join(executable, "gitconfig")
			return
		}
		emptyGlobalConfigPath = os.DevNull
		f, err := os.CreateTemp("", "gitcmd-empty-global-*.gitconfig")
		if err != nil {
			return
		}
		emptyGlobalConfigPath = f.Name()
		_ = f.Close()
	})
	return emptyGlobalConfigPath
}

// readSafeDirectories reads safe.directory entries from system and global git
// config using env, in git's evaluation order. git only honors safe.directory
// from protected configuration (system, global, and command scope), so these
// are the entries the sanitized environment would otherwise hide. Entries are
// read fresh on every call. Unset or unreadable scopes contribute nothing.
// Empty values are kept because an empty safe.directory resets the list.
// Replaying entries in order preserves that semantic at command scope.
//
// The probes run in dir, the same directory as the git command being built,
// so conditional includes (includeIf "gitdir:...") resolve exactly as they
// would for that command rather than against the calling process's working
// directory.
//
// "git config --system" reads the system file even when GIT_CONFIG_NOSYSTEM
// is set (the variable only affects git's default config sequence), so the
// system scope is skipped here explicitly: git running with this env would
// not honor those entries, and they must not be forwarded on its behalf.
func readSafeDirectories(ctx context.Context, env []string, dir string) []string {
	var dirs []string
	for _, scope := range safeDirectoryScopes(env) {
		entries, _, err := readSafeDirectoryScope(ctx, env, dir, scope)
		if err != nil {
			continue
		}
		dirs = append(dirs, entries...)
	}
	return dirs
}

// gitEnvBool reports whether env sets key to a value git's boolean parsing
// treats as true. An empty value is false, matching git's handling of boolean
// environment variables (unlike valueless config keys, which are true).
func gitEnvBool(env []string, key string) bool {
	value, ok := envValue(env, key)
	if !ok {
		return false
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true
	case "", "false", "no", "off":
		return false
	}
	n, err := strconv.Atoi(value)
	return err == nil && n != 0
}

// envValue returns the value of key in env, honoring exec.Cmd semantics where
// the last duplicate entry wins.
func envValue(env []string, key string) (string, bool) {
	return envValueForGOOS(env, key, runtime.GOOS)
}

func envValueForGOOS(env []string, key, goos string) (string, bool) {
	for _, entry := range slices.Backward(env) {
		k, v, ok := strings.Cut(entry, "=")
		if ok && (k == key || goos == "windows" && strings.EqualFold(k, key)) {
			return v, true
		}
	}
	return "", false
}

func (r Runner) commandEnv(ctx context.Context, dir string) ([]string, func()) {
	cleanup := func() {}
	base := r.Env
	if base == nil {
		base = os.Environ()
	}
	env := base
	if r.StripEnv {
		env = gitenv.StripAll(env)
	} else {
		env = append([]string(nil), env...)
	}
	if !r.TerminalPrompt {
		env = append(env, "GIT_TERMINAL_PROMPT=0")
	}
	if r.NoSystemConfig {
		env = append(env, "GIT_CONFIG_NOSYSTEM=1")
	}
	if r.NullGlobalConfig {
		env = append(env, "GIT_CONFIG_GLOBAL="+nullGlobalConfigPath())
	}
	config := []Config{{Key: "gc.auto", Value: "0"}, {Key: "maintenance.auto", Value: "false"}}
	inheritedSafeDirectories := inheritedSafeDirectoryValues(env, r.StripEnv)
	if !r.DisableSafeDirectoryForward {
		// Read from the runner's base env before stripping, so the entries come
		// from the configuration this runner's environment would see, not from
		// the process environment.
		for _, trusted := range r.trust.read(ctx, base, dir) {
			config = append(config, Config{Key: "safe.directory", Value: trusted})
		}
	}
	for _, trusted := range inheritedSafeDirectories {
		config = append(config, Config{
			Key: "safe.directory", Value: trusted,
		})
	}
	config = append(config, r.Config...)
	if r.basicAuth != nil {
		responsePath, cleanupResponse, err := r.basicAuth.credentialResponse()
		var helper string
		if err != nil {
			helper = `!f() { echo "gitcmd basic auth helper setup failed" >&2; exit 1; }; f`
		} else {
			helper = credentialHelper(responsePath)
			cleanup = cleanupResponse
		}
		config = append(config, Config{Key: "credential.helper", Value: helper})
	}
	configOffset := 0
	if !r.StripEnv {
		if inheritedCount, ok := envValue(
			env, "GIT_CONFIG_COUNT",
		); ok {
			if count, err := strconv.Atoi(inheritedCount); err == nil && count >= 0 {
				configOffset = count
			}
		}
	}
	for i, c := range config {
		i += configOffset
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, c.Key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, c.Value),
		)
	}
	env = append(env, fmt.Sprintf(
		"GIT_CONFIG_COUNT=%d", configOffset+len(config),
	))
	return env, cleanup
}

func inheritedSafeDirectoryValues(
	env []string, stripEnv bool,
) []string {
	if stripEnv {
		return nil
	}
	rawCount, ok := envValue(env, "GIT_CONFIG_COUNT")
	if !ok {
		return nil
	}
	count, err := strconv.Atoi(rawCount)
	if err != nil || count < 0 {
		return nil
	}
	var directories []string
	for i := range count {
		key, ok := envValue(env, fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
		if !ok || !strings.EqualFold(key, "safe.directory") {
			continue
		}
		value, _ := envValue(env, fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
		directories = append(directories, value)
	}
	return directories
}

// GitError wraps a failed git command with stderr.
type GitError struct {
	// Dir is the working directory used for the git command.
	Dir string
	// Args are the git arguments without the leading "git" executable.
	Args []string
	// Stderr is the trimmed stderr captured from git.
	Stderr string
	// Err is the underlying execution or output-capture error.
	Err error

	processState *os.ProcessState
}

func (e *GitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *GitError) Unwrap() error {
	return e.Err
}

// ExitCode returns git's process exit code when available, including zero when
// Git succeeded but output capture reported an error.
func (e *GitError) ExitCode() (int, bool) {
	if e.processState != nil {
		return e.processState.ExitCode(), true
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](e.Err); ok {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// IsExitCode reports whether err is a GitError with code.
func IsExitCode(err error, code int) bool {
	gitErr, ok := errors.AsType[*GitError](err)
	return ok && func() bool {
		got, ok := gitErr.ExitCode()
		return ok && got == code
	}()
}
