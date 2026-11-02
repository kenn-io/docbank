package gitcmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitenv "go.kenn.io/kit/git/env"
	"go.kenn.io/kit/git/internal/shellquote"
)

func TestRunnerWorktreeExecutionPolicy(t *testing.T) {
	t.Parallel()
	t.Run("bounded stdout drains the command", func(t *testing.T) {
		t.Parallel()
		runner := New()
		runner.DisableSafeDirectoryForward = true
		dir := t.TempDir()
		_, _, err := runner.Run(t.Context(), dir, nil, "init")
		require.NoError(t, err)
		contents := strings.Repeat("x", (1<<20)+1)
		oid, _, err := runner.Run(t.Context(), dir, strings.NewReader(contents), "hash-object", "-w", "--stdin")
		require.NoError(t, err)
		runner.StdoutLimit = 1 << 20
		stdout, stderr, err := runner.Run(t.Context(), dir, nil, "cat-file", "blob", strings.TrimSpace(string(oid)))
		require.ErrorIs(t, err, ErrStdoutLimitExceeded)
		assert.True(t, IsExitCode(err, 0), "truncation must retain the successful Git exit")
		assert.Equal(t, strings.Repeat("x", 1<<20), string(stdout))
		assert.Empty(t, stderr)
		runner.StdoutLimit = len(contents)
		stdout, _, err = runner.Run(t.Context(), dir, nil, "cat-file", "blob", strings.TrimSpace(string(oid)))
		require.NoError(t, err)
		assert.Equal(t, contents, string(stdout))
	})
	t.Run("independent limits preserve process failure", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name                     string
			stdoutLimit, stderrLimit int
			stdout, stderr           string
		}{
			{"both limited", 3, 4, "abc", "ghij"},
			{"stdout limited", 3, 0, "abc", "ghijkl"},
			{"stderr limited", 0, 4, "abcdef", "ghij"},
			{"zero is unlimited", 0, 0, "abcdef", "ghijkl"},
			{"negative is unlimited", -1, -1, "abcdef", "ghijkl"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				runner := New().WithConfig("alias.emit", "!printf abcdef; printf ghijkl >&2; exit 3")
				runner.DisableSafeDirectoryForward = true
				runner.StdoutLimit, runner.StderrLimit = tc.stdoutLimit, tc.stderrLimit
				stdout, stderr, err := runner.Run(t.Context(), t.TempDir(), nil, "emit")
				require.Error(t, err)
				assert.Equal(t, tc.stdoutLimit > 0, errors.Is(err, ErrStdoutLimitExceeded))
				assert.Equal(t, tc.stderrLimit > 0, errors.Is(err, ErrStderrLimitExceeded))
				assert.True(t, IsExitCode(err, 3))
				assert.Equal(t, tc.stdout, string(stdout))
				assert.Equal(t, tc.stderr, string(stderr))
				assert.Contains(t, err.Error(), tc.stderr)
			})
		}
	})
	t.Run("inherited config remains selectable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		global := filepath.Join(dir, "global")
		system := filepath.Join(dir, "system")
		require.NoError(t, os.WriteFile(global, []byte("[fixture]\nvalue = global\n"), 0o600))
		require.NoError(t, os.WriteFile(system, []byte("[fixture]\nvalue = system\n"), 0o600))
		runner := Runner{Env: append(gitenv.StripAll(os.Environ()), "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_SYSTEM="+system), DisableSafeDirectoryForward: true}
		out, err := runner.Output(t.Context(), dir, "config", "--get-all", "fixture.value")
		require.NoError(t, err)
		assert.Equal(t, "system\nglobal\n", string(out))
		runner.NoSystemConfig = true
		out, err = runner.Output(t.Context(), dir, "config", "--get-all", "fixture.value")
		require.NoError(t, err)
		assert.Equal(t, "global\n", string(out))
		runner.NullGlobalConfig = true
		_, err = runner.Output(t.Context(), dir, "config", "--get-all", "fixture.value")
		assert.True(t, IsExitCode(err, 1))
	})
	t.Run("canceled context is preserved", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		runner := New()
		runner.DisableSafeDirectoryForward = true
		_, _, err := runner.Run(ctx, t.TempDir(), nil, "version")
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestRunnerWaitDelayWithInheritedPipe(t *testing.T) {
	t.Parallel()
	for _, accept := range []bool{false, true} {
		t.Run(map[bool]string{false: "report", true: "accept successful exit"}[accept], func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(func() { close(release) })
			executable, err := os.Executable()
			require.NoError(t, err)
			runner := New().WithConfig("alias.hold-pipe", "!"+shellquote.Single(executable)+" -test.run=^TestRunnerHeldPipeHelper$ &")
			runner.DisableSafeDirectoryForward = true
			runner.Env = append(runner.Env, "GITCMD_PIPE_HELPER_URL="+server.URL)
			runner.WaitDelay = 100 * time.Millisecond //nolint:kennlint // deadlinetest: the held pipe must trigger exec.ErrWaitDelay before the helper is released
			runner.AcceptSuccessfulWaitDelay = accept
			require.Equal(t, 100*time.Millisecond, runner.Command(t.Context(), t.TempDir(), "version").WaitDelay)
			result := make(chan error, 1)
			// This command creates no files. Its background helper must not hold
			// a fixture directory open when Windows runs TempDir cleanup.
			dir := os.TempDir() //nolint:usetesting // The pipe holder outlives fixture cleanup and writes no files.
			go func() {
				_, _, runErr := runner.Run(t.Context(), dir, nil, "hold-pipe")
				result <- runErr
			}()
			select {
			case <-started:
			case <-t.Context().Done():
				require.FailNow(t, "pipe holder did not start")
			}
			if accept {
				require.NoError(t, <-result)
			} else {
				require.ErrorIs(t, <-result, exec.ErrWaitDelay)
			}
		})
	}
}

func TestRunnerHeldPipeHelper(t *testing.T) {
	url := os.Getenv("GITCMD_PIPE_HELPER_URL")
	if url == "" {
		return
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, errors.Join(err, response.Body.Close()))
}

func TestRunnerOutputLimitCleansCredentials(t *testing.T) {
	// The process-wide empty config must outlive this credential fixture.
	_ = nullGlobalConfigPath()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	runner := New().WithBasicAuth("fixture-user", "fixture-secret")
	runner.DisableSafeDirectoryForward = true
	runner.StdoutLimit = 3
	_, _, err := runner.Run(t.Context(), dir, strings.NewReader("protocol=https\nhost=example.invalid\n\n"), "credential", "fill")
	require.ErrorIs(t, err, ErrStdoutLimitExceeded)
	assert.NotContains(t, err.Error(), "fixture-secret")
	files, err := filepath.Glob(filepath.Join(dir, "gitcmd-credential-response-*"))
	require.NoError(t, err)
	assert.Empty(t, files)
}

func BenchmarkRunnerCapture(b *testing.B) {
	for _, limit := range []int{0, 4096} {
		name := "unlimited"
		if limit > 0 {
			name = "limited"
		}
		b.Run(name, func(b *testing.B) {
			runner := New()
			runner.DisableSafeDirectoryForward = true
			runner.StdoutLimit, runner.StderrLimit = limit, limit
			dir := b.TempDir()
			b.ReportAllocs()
			for b.Loop() {
				out, err := runner.Output(b.Context(), dir, "version")
				require.NoError(b, err)
				require.Contains(b, string(out), "git version")
			}
		})
	}
}
