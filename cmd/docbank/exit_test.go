package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

func TestCommandExitCode(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		started bool
		want    int
	}{
		{name: "success", want: exitSuccess},
		{name: "general", err: errors.New("failed"), started: true, want: exitGeneral},
		{name: "cobra usage", err: errors.New("unknown flag"), want: exitUsage},
		{name: "search query required", err: store.ErrSearchQueryRequired, started: true, want: exitUsage},
		{name: "semantic usage", err: usageError(errors.New("bad limit")), started: true, want: exitUsage},
		{name: "not found", err: fmt.Errorf("lookup: %w", store.ErrNotFound), started: true, want: exitNotFound},
		{name: "stale revision", err: store.ErrStaleRevision, started: true, want: exitStale},
		{name: "stale preview", err: store.ErrAuditPreviewStale, started: true, want: exitStale},
		{name: "vault busy", err: home.ErrVaultLocked, started: true, want: exitBusy},
		{name: "repository busy", err: backup.ErrRepoLocked, started: true, want: exitBusy},
		{name: "retirement busy", err: packstore.ErrPackRetirementDeferred, started: true, want: exitBusy},
		{name: "maintenance busy", err: daemonconn.ErrMaintenanceBusy, started: true, want: exitBusy},
		{name: "content integrity", err: daemonconn.ErrIntegrity, started: true, want: exitIntegrity},
		{name: "reported integrity", err: integrityError(errors.New("problems")), started: true, want: exitIntegrity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, commandExitCode(tt.err, tt.started))
		})
	}
}

func TestRunProcessDistinguishesUsageAndMissingNodes(t *testing.T) {
	_ = setupVaultHome(t)

	var stdout, stderr bytes.Buffer
	run := func(args ...string) int {
		resetFlags(rootCmd)
		stdout.Reset()
		stderr.Reset()
		return runProcess(args, &stdout, &stderr)
	}

	code := run("search", "term", "--limit", "0")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "--limit must be between")

	code = run("search")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "search query is required")

	code = run("search", "--under", "/")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "search query is required")

	code = run("search", "--mime-type", "text/plain")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "search query is required")

	code = run("search", "term", "--mime-type", "text/plain; charset=utf-8")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "must not include parameters")

	code = run("search", "term", "--under", "relative")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "absolute virtual path or id:")

	code = run("search", "term", "--modified-since", "yesterday")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "absolute RFC3339 timestamp")

	code = run("storage", "pack", "--max-bytes", "-1")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "validation")

	code = run("ls", "/missing")
	assert.Equal(t, exitNotFound, code)
	assert.Contains(t, stderr.String(), "not found")

	code = run("backup", "restore")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "--target is required")

	code = run("audit", "status", "/", "--node-id", "0")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "either a path or --node-id")

	code = run("audit", "status", "--node-id", "0")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "--node-id must be positive")

	code = run("audit", "enable", "--node-id", "0")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "--node-id must be positive")

	code = run("audit", "enable", "--run", "--node-id", "0")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "uses only the reviewed preview token")

	code = run("audit", "history", "--node-id", "0")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "--node-id must be positive")

	code = run("tag", "assign", "work", "relative/path")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "absolute virtual path or id:<positive-decimal>")

	code = run("revert", "/document", "not-a-version-id")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "must be a canonical UUIDv4")

	code = run("versions", "show", "not-a-version-id")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "must be a canonical UUIDv4")

	code = run("versions", "cat", "not-a-version-id")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "must be a canonical UUIDv4")

	invalidSource := string([]byte{'b', 'a', 'd', 0xff})
	code = run("put", invalidSource, "/document")
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "is not valid UTF-8")

	code = run("--not-a-real-flag")
	require.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "unknown flag")

	code = run("--version")
	require.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "unknown flag: --version")
}

func TestRunProcessRequiredFlagErrorsAreUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	resetFlags(rootCmd)

	code := runProcess([]string{"people", "rename", "id:5", "New Name"}, &stdout, &stderr)

	assert.Equal(t, exitUsage, code, "stderr:\n%s", stderr.String())
	assert.Contains(t, stderr.String(), `error: required flag(s) "revision" not set`)
	assert.Contains(t, stderr.String(), "hint: usage: docbank people rename <person-id> <display-name>")
	assert.Empty(t, stdout.String())
}

func TestRunProcessErrorHints(t *testing.T) {
	_ = setupVaultHome(t)
	var stdout, stderr bytes.Buffer
	run := func(args ...string) int {
		resetFlags(rootCmd)
		stdout.Reset()
		stderr.Reset()
		return runProcess(args, &stdout, &stderr)
	}
	assert.Equal(t, exitUsage, run("get", "id:1"))
	assert.Equal(t, "error: accepts 2 arg(s), received 1", strings.SplitN(stderr.String(), "\n", 2)[0])
	assert.Contains(t, stderr.String(), "hint: usage: docbank get <path-or-id> <local-file>")
	assert.Equal(t, 1, strings.Count(stderr.String(), "hint:"))
	assert.Empty(t, stdout.String())
	assert.Equal(t, exitNotFound, run("ls", "/missing"))
	assert.True(t, strings.HasPrefix(stderr.String(), "error: "))
	assert.Contains(t, stderr.String(), `hint: list paths with "docbank tree" or find by name with "docbank search <name>"`)
	assert.Equal(t, exitUsage, run("processing", "status", "abc"))
	assert.Contains(t, stderr.String(), `job ID must be lowercase SHA-256; the job ID is printed by "docbank processing build"`)
	for _, args := range [][]string{{"rendition", "window", "id:1", "--version", "abc", "--profile", "supplied-captions"}, {"processing", "coverage", "abc", "--profile", "supplied-captions"}} {
		assert.Equal(t, exitUsage, run(args...))
		assert.Contains(t, stderr.String(), `version must be a canonical lowercase UUIDv4; use current_version_id from "docbank stat <path-or-id> --json"`)
	}
	assert.Equal(t, exitUsage, run("processing", "plan", "id:1"))
	assert.Contains(t, stderr.String(), `list profiles with "docbank processing profiles"`)
	assert.Equal(t, exitUsage, run("search", "term", "--mode", "lexical"))
	assert.Contains(t, stderr.String(), `list profiles with "docbank processing profiles"`)
	assert.Equal(t, exitUsage, run("search", "term", "--mode", "lexical", "--profile", "supplied-captions"))
	assert.Contains(t, stderr.String(), `use current_version_id from "docbank stat <path-or-id> --json"`)
	assert.Equal(t, exitUsage, run("stta"))
	assert.Contains(t, stderr.String(), "Did you mean")
	assert.NotContains(t, stderr.String(), "hint:")
}

func TestCommandErrorHint(t *testing.T) {
	multiline := &cobra.Command{Use: "sample <value>\n<other>", Run: func(*cobra.Command, []string) {}}
	tests := []struct {
		name    string
		cmd     *cobra.Command
		err     error
		code    int
		started bool
		want    string
	}{
		{"arity", getCmd, errors.New("accepts 2 arg(s), received 1"), exitUsage, false, "hint: usage: docbank get <path-or-id> <local-file> [flags]"},
		{"root flag", rootCmd, errors.New("unknown flag"), exitUsage, false, `hint: run "docbank --help"`},
		{"unknown command", nil, errors.New("unknown command"), exitUsage, false, `hint: run "docbank --help"`},
		{"suggestion", rootCmd, errors.New("unknown command; Did you mean stat?"), exitUsage, false, ""},
		{"multiline", multiline, errors.New("bad argument"), exitUsage, false, `hint: run "sample --help"`},
		{"started usage", searchCmd, usageError(errors.New("bad limit")), exitUsage, true, ""},
		{"missing path", lsCmd, fmt.Errorf(`resolving "/missing": %w`, store.ErrNotFound), exitNotFound, true, `hint: list paths with "docbank tree" or find by name with "docbank search <name>"`},
		{"missing node ID", statCmd, fmt.Errorf(`resolving "id:999": %w`, store.ErrNotFound), exitNotFound, true, ""},
		{"trashed node", statCmd, fmt.Errorf(`resolving "/gone": node is trashed: %w`, store.ErrNotFound), exitNotFound, true, `hint: list restorable nodes with "docbank trash list"`},
		{"missing tag", tagShowCmd, fmt.Errorf(`resolving tag "urgent": %w`, store.ErrNotFound), exitNotFound, true, `hint: list tags with "docbank tag list"`},
		{"missing job", jobsShowCmd, fmt.Errorf(`showing operation "unknown": %w`, store.ErrNotFound), exitNotFound, true, ""},
		{"unclassified missing resource", lsCmd, store.ErrNotFound, exitNotFound, true, ""},
		{"busy", jobsCmd, home.ErrVaultLocked, exitBusy, true, `hint: wait and retry; "docbank jobs" shows active work`},
		{"backup repository locked", backupCreateCmd, backup.ErrRepoLocked, exitBusy, true, `hint: wait for the backup repository owner; use --force-unlock only when its owner is known to be gone`},
		{"pack retirement deferred", storageRepackCmd, packstore.ErrPackRetirementDeferred, exitBusy, true, ""},
		{"maintenance busy", jobsCmd, daemonconn.ErrMaintenanceBusy, exitBusy, true, `hint: wait and retry; "docbank jobs" shows active work`},
		{"stale", rmCmd, store.ErrStaleRevision, exitStale, true, ""},
		{"integrity", verifyCmd, daemonconn.ErrIntegrity, exitIntegrity, true, ""},
		{"general", verifyCmd, errors.New("failed"), exitGeneral, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, commandErrorHint(tt.cmd, tt.err, tt.code, tt.started)) })
	}
}
