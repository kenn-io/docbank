package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const missingBackupRepositoryMessage = "no backup repository configured; pass repo or set [backup] repo in config.toml"

type processErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Hint    string `json:"hint"`
		Exit    int    `json:"exit"`
	} `json:"error"`
}

func helpOnlyGroups() []*cobra.Command {
	var groups []*cobra.Command
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd != rootCmd && !cmd.Hidden && cmd.HasSubCommands() && !workParents[cmd.CommandPath()] && cmd.Name() != "help" {
			groups = append(groups, cmd)
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(rootCmd)
	return groups
}

func TestGroupParentsRejectUnknownSubcommands(t *testing.T) {
	for _, cmd := range helpOnlyGroups() {
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			resetFlags(rootCmd)
			var out, diag bytes.Buffer
			args := append(strings.Fields(strings.TrimPrefix(cmd.CommandPath(), "docbank ")), "unknown-child-")
			assert.Equal(t, exitUsage, runProcess(args, &out, &diag))
			assert.Empty(t, out.String())
			assert.Contains(t, diag.String(), "unknown command")
			assert.Contains(t, diag.String(), cmd.CommandPath()+" --help")
		})
	}
}

func TestGroupParentsBareHelp(t *testing.T) {
	for _, cmd := range helpOnlyGroups() {
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			resetFlags(rootCmd)
			var out, diag bytes.Buffer
			args := strings.Fields(strings.TrimPrefix(cmd.CommandPath(), "docbank "))
			assert.Equal(t, exitSuccess, runProcess(args, &out, &diag))
			assert.Contains(t, out.String(), "Available Commands:")
			assert.Empty(t, diag.String())
		})
	}
}

func TestRunProcessErrorModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		json bool
		hint string
	}{
		{"json", []string{"search", "words", "--limit", "0", "--json"}, true, "docbank search --help"},
		{"json before subcommand", []string{"mv", "--json", "batch"}, true, "docbank mv batch --help"},
		{"json before arguments", []string{"search", "--json", "words", "--limit", "0"}, true, "docbank search --help"},
		{"mutually exclusive modes", []string{"search", "words", "--agent", "--json"}, true, "docbank search --help"},
		{"json false", []string{"search", "words", "--limit", "0", "--json=false"}, false, "hint:"},
		{"json separator", []string{"get", "--", "--json"}, false, "hint:"},
		{"agent", []string{"search", "words", "--limit", "0", "--agent"}, false, "Hint:"},
		{"flag", []string{"tree", "--bogus"}, false, "docbank tree --help"},
		{"nested flag", []string{"backup", "schedule", "show", "--bogus"}, false, "docbank backup schedule show --help"},
		{"root version", []string{"--version"}, false, "docbank version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(rootCmd)
			var out, diag bytes.Buffer
			require.Equal(t, exitUsage, runProcess(tc.args, &out, &diag))
			require.Empty(t, out.String())
			if tc.json {
				var env processErrorEnvelope
				require.NoError(t, json.Unmarshal(diag.Bytes(), &env))
				assert.Equal(t, "usage", env.Error.Code)
				assert.Equal(t, 2, env.Error.Exit)
				assert.NotEmpty(t, env.Error.Message)
				assert.Contains(t, env.Error.Hint, tc.hint)
			} else {
				assert.True(t, strings.HasPrefix(diag.String(), "error: "))
				assert.Contains(t, diag.String(), tc.hint)
			}
		})
	}
}

func TestRunProcessRecoveryHints(t *testing.T) {
	_ = setupVaultHome(t)
	for _, tc := range []struct {
		args []string
		code int
		hint string
	}{
		{[]string{"search"}, 2, "--tag"},
		{[]string{"search", "words", "--mode", "invalid"}, 2, "lexical, semantic, hybrid, auto"},
		{[]string{"cat", "relative"}, 2, "docbank tree /"},
		{[]string{"stat", "id:Did you mean"}, 2, "docbank tree /"},
		{[]string{"tree", "id:abc"}, 2, "docbank tree /"},
		{[]string{"stat", "id:999"}, 3, "docbank trash list"},
		{[]string{"restore", "999"}, 3, "docbank trash list"},
		{[]string{"rm", "/missing"}, 3, "docbank tree"},
		{[]string{"mv", "/missing", "/dest"}, 3, "docbank tree"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			resetFlags(rootCmd)
			var out, diag bytes.Buffer
			assert.Equal(t, tc.code, runProcess(tc.args, &out, &diag))
			assert.Empty(t, out.String())
			assert.Contains(t, diag.String(), tc.hint)
			assert.Equal(t, 1, strings.Count(diag.String(), "hint:"))
			assert.NotContains(t, diag.String(), "error executing request:")
			var hint string
			for _, line := range strings.Split(diag.String(), "\n") {
				if strings.HasPrefix(line, "hint: ") {
					hint = strings.TrimPrefix(line, "hint: ")
					break
				}
			}
			require.NotEmpty(t, hint)
			assertHelpHintCommandsParse(t, hint)
		})
	}
	resetFlags(rootCmd)
	var out, diag bytes.Buffer
	require.Equal(t, 3, runProcess([]string{"stat", "/missing", "--json"}, &out, &diag))
	assert.Empty(t, out.String())
	var env processErrorEnvelope
	require.NoError(t, json.Unmarshal(diag.Bytes(), &env))
	assert.Equal(t, "not_found", env.Error.Code)
	assert.Equal(t, 3, env.Error.Exit)
	assert.Contains(t, env.Error.Hint, "docbank tree")
	assertHelpHintCommandsParse(t, env.Error.Hint)
	assert.True(t, strings.HasSuffix(diag.String(), "\n"))
	assert.Equal(t, 1, strings.Count(diag.String(), "\n"))
}

func TestRunProcessJSONInvalidUTF8(t *testing.T) {
	resetFlags(rootCmd)
	var out, diag bytes.Buffer
	code := runProcess([]string{"search", "--json", "--bad-" + string([]byte{0xff})}, &out, &diag)
	require.Equal(t, 2, code)
	require.Empty(t, out.String())
	var env processErrorEnvelope
	require.NoError(t, json.Unmarshal(diag.Bytes(), &env))
	assert.Equal(t, 2, env.Error.Exit)
	assert.Contains(t, env.Error.Hint, "docbank search --help")
}

func TestRunProcessBackupRepositoryRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		code         int
	}{
		{"unconfigured", "no backup repository configured; pass repo or set [backup] repo in config.toml", 2},
		{"io failure", "opening repository: permission denied", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.MarshalWrite(w, api.Error{Status: 422, Code: "backup_repository", Detail: tc.detail})
			})
			resetFlags(rootCmd)
			var out, diag bytes.Buffer
			assert.Equal(t, tc.code, runProcess([]string{"backup", "create", "--json"}, &out, &diag))
			assert.Empty(t, out.String())
			var env processErrorEnvelope
			require.NoError(t, json.Unmarshal(diag.Bytes(), &env))
			assert.Contains(t, env.Error.Hint, "--repo")
			assert.Contains(t, env.Error.Hint, "docbank backup init")
			assertHelpHintCommandsParse(t, env.Error.Hint)
			if tc.code == 2 {
				assert.NotContains(t, env.Error.Message, "pass repo")
			}
		})
	}
}

func TestRunProcessAddPartialFailureKeepsReport(t *testing.T) {
	_ = setupVaultHome(t)
	resetFlags(rootCmd)
	var out, diag bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.pdf")
	assert.Equal(t, 1, runProcess([]string{"add", missing, "--json"}, &out, &diag))
	var report struct {
		Failed []jsontext.Value `json:"failed"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	require.Len(t, report.Failed, 1)
	var env processErrorEnvelope
	require.NoError(t, json.Unmarshal(diag.Bytes(), &env))
	assert.Contains(t, env.Error.Hint, "failed paths")
	assert.Contains(t, env.Error.Hint, "identical content")
}

func TestCLIErrorMessagePreservesLiteralTransportText(t *testing.T) {
	literal := errors.New("error executing request: ")
	assert.Equal(t, literal.Error(), cliErrorMessage(literal))
	wrapped := fmt.Errorf("error executing request: %w", literal)
	assert.Equal(t, literal.Error(), cliErrorMessage(wrapped))
	assert.ErrorIs(t, wrapped, literal)
}

func processProbe(t *testing.T, run func(*cobra.Command, []string) error) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "agent-help-probe", Hidden: true, GroupID: groupOperations, RunE: run}
	cmd.Flags().Bool("json", false, "print JSON to stdout")
	cmd.Flags().Bool("agent", false, "print agent output")
	rootCmd.AddCommand(cmd)
	t.Cleanup(func() { rootCmd.RemoveCommand(cmd) })
	return cmd
}

func missingBackupProblem(t *testing.T) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.MarshalWrite(w, api.Error{Status: http.StatusUnprocessableEntity, Code: "backup_repository", Detail: missingBackupRepositoryMessage})
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.New(server.URL, "synthetic-key").API().CreateBackupSnapshot(t.Context(), &apiclient.CreateBackupSnapshotRequestOptions{Body: &apiclient.CreateBackupSnapshotBody{}})
	require.Error(t, err)
	return err
}

func TestMissingBackupContextPreservesClassification(t *testing.T) {
	original := missingBackupProblem(t)
	for _, err := range []error{fmt.Errorf("%w (context)", original), errors.Join(original, errors.New("secondary"))} {
		assert.Equal(t, 2, commandExitCode(err, true))
		assert.Contains(t, cliErrorMessage(err), "pass --repo")
		assert.NotContains(t, cliErrorMessage(err), "pass repo")
	}
}

func TestRunProcessJSONAfterUnterminatedDiagnostic(t *testing.T) {
	probe := processProbe(t, func(cmd *cobra.Command, _ []string) error {
		_, _ = cmd.ErrOrStderr().Write([]byte("0"))
		return usageError(errors.New(""))
	})
	resetFlags(rootCmd)
	var out, diag bytes.Buffer
	require.Equal(t, 2, runProcess([]string{probe.Name(), "--json"}, &out, &diag))
	lines := strings.Split(diag.String(), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, "0", lines[0])
	var env processErrorEnvelope
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &env))
	assert.Equal(t, 2, env.Error.Exit)
	assert.Empty(t, lines[2])
}
