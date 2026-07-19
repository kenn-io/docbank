package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/client"
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
		{name: "semantic usage", err: usageError(errors.New("bad limit")), started: true, want: exitUsage},
		{name: "not found", err: fmt.Errorf("lookup: %w", store.ErrNotFound), started: true, want: exitNotFound},
		{name: "stale revision", err: store.ErrStaleRevision, started: true, want: exitStale},
		{name: "stale preview", err: store.ErrAuditPreviewStale, started: true, want: exitStale},
		{name: "vault busy", err: home.ErrVaultLocked, started: true, want: exitBusy},
		{name: "repository busy", err: backup.ErrRepoLocked, started: true, want: exitBusy},
		{name: "retirement busy", err: packstore.ErrPackRetirementDeferred, started: true, want: exitBusy},
		{name: "content integrity", err: client.ErrIntegrity, started: true, want: exitIntegrity},
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

	code = run("--not-a-real-flag")
	require.Equal(t, exitUsage, code)
	assert.Contains(t, stderr.String(), "unknown flag")
}
