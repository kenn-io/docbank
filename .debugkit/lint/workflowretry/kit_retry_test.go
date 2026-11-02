package workflowretry

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKitRetry(t *testing.T) {
	for _, tt := range []struct {
		name, attempts, delay, success, count, sleeps string
		status                                        int
		exec                                          bool
	}{
		{"success", "3", "5", "1", "1", "", 0, false},
		{"retry", "3", "5", "3", "3", "5\n10\n", 0, false},
		{"exhaustion", "3", "0", "4", "3", "0\n0\n", 17, false},
		{"exec", "2", "0", "2", "2", "0\n", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			windows := runtime.GOOS == "windows"
			if windows {
				tt.delay, tt.sleeps = "0", ""
			}
			dir := t.TempDir()
			script := filepath.Join(dir, "command")
			require.NoError(t, os.WriteFile(script, []byte("#!/usr/bin/env bash\nn=0\nif [ -f \"$STATE/attempts\" ]; then read -r n < \"$STATE/attempts\"; fi\nn=$((n+1))\nprintf '%s' \"$n\" > \"$STATE/attempts\"\nprintf '<%s>\\n' \"$@\"\n[ \"$n\" -ge \"$SUCCESS\" ] || exit 17\n"), 0o700))
			if !windows {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" >> \"$STATE/sleeps\"\n"), 0o700))
			}
			args := []string{"../../.github/actions/kit-retry/kit-retry", tt.attempts, tt.delay}
			if tt.exec {
				args = append(args, "exec")
			}
			args = append(args, "bash", filepath.ToSlash(script), "argument with spaces")
			wantArgs := "<argument with spaces>\n"
			if !windows {
				args = append(args, "*")
				wantArgs += "<*>\n"
			}
			args = append(args, "")
			wantArgs += "<>\n"
			cmd := exec.CommandContext(t.Context(), "bash", args...)
			cmd.Env = append(os.Environ(), "STATE="+filepath.ToSlash(dir), "SUCCESS="+tt.success)
			if !windows {
				cmd.Env = append(cmd.Env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			err := cmd.Run()
			if tt.status == 0 {
				require.NoError(t, err)
			} else {
				exit, ok := errors.AsType[*exec.ExitError](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, tt.status, exit.ExitCode())
			}
			count, err := os.ReadFile(filepath.Join(dir, "attempts"))
			require.NoError(t, err)
			assert.Equal(t, tt.count, string(count))
			assert.Equal(t, strings.Repeat(wantArgs, int(count[0]-'0')), stdout.String())
			sleeps, err := os.ReadFile(filepath.Join(dir, "sleeps"))
			if tt.sleeps != "" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			assert.Equal(t, tt.sleeps, string(sleeps))
		})
	}
}

func TestKitRetryInvalidUsage(t *testing.T) {
	for _, args := range [][]string{{"3", "0"}, {"0", "0", "echo", "command-ran"}, {"3", "-1", "echo", "command-ran"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", append([]string{"../../.github/actions/kit-retry/kit-retry"}, args...)...)
			out, err := cmd.CombinedOutput()
			exit, ok := errors.AsType[*exec.ExitError](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, 2, exit.ExitCode())
			assert.Contains(t, string(out), "usage: kit-retry")
			assert.NotContains(t, string(out), "command-ran")
		})
	}
}
