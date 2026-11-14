package main

import (
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestReportCLIRelease(t *testing.T) {
	setupVaultHome(t)
	summary, request := createCLIReport(t)
	packet := filepath.Join(t.TempDir(), "report.zip")
	_, err := runCLI(t, "search-export", "download", summary.ID, "--output", packet)
	require.NoError(t, err)
	before, err := os.ReadFile(packet)
	require.NoError(t, err)
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	for range 7 {
		_, err := connection.CreateTermReport(t.Context(), request)
		require.NoError(t, err)
	}
	_, err = connection.CreateTermReport(t.Context(), request)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "report_capacity", code)
	history, err := runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	output, err := runCLI(t, "search-export", "release", summary.ID, "--json")
	require.NoError(t, err)
	require.Equal(t, exitSuccess, commandExitCode(err, true))
	var result map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	require.Equal(t, map[string]any{"report_id": summary.ID, "released": true}, result)
	for _, operation := range []string{"show", "release"} {
		_, err := runCLI(t, "search-export", operation, summary.ID)
		require.Error(t, err)
		require.Equal(t, exitGeneral, commandExitCode(err, true))
		code, ok := daemonconn.ProblemCode(err)
		require.True(t, ok)
		require.Equal(t, "report_unavailable", code)
	}
	retainedHistory, err := runCLI(t, "search-export", "history", "--json")
	require.NoError(t, err)
	require.Equal(t, history, retainedHistory)
	_, err = connection.CreateTermReport(t.Context(), request)
	require.NoError(t, err, "release must free one shared owner slot")
	after, err := os.ReadFile(packet)
	require.NoError(t, err)
	require.Equal(t, before, after)
	verified, err := verifyReportFile(t.Context(), packet)
	require.NoError(t, err)
	require.True(t, verified.InternallyConsistent)
}

func TestReportReleasePreflight(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "absent-vault")
	t.Setenv("DOCBANK_HOME", vault)
	for _, args := range [][]string{
		nil, {"bad"}, {strings.Repeat("A", 48)}, {strings.Repeat("a", 47)},
		{strings.Repeat("a", 48), "extra"},
	} {
		_, err := runCLI(t, append([]string{"search-export", "release"}, args...)...)
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true), args)
		require.NoDirExists(t, vault)
	}
}

func TestReportReleaseOutputFailure(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonOutput], func(t *testing.T) {
			setupVaultHome(t)
			summary, _ := createCLIReport(t)
			reader, writer := io.Pipe()
			require.NoError(t, reader.Close())
			t.Cleanup(func() { require.NoError(t, writer.Close()) })
			cmd := newReportReleaseCommand()
			cmd.SetOut(writer)
			cmd.SetErr(io.Discard)
			args := []string{summary.ID}
			if jsonOutput {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(t.Context())
			require.ErrorIs(t, err, io.ErrClosedPipe)
			require.ErrorContains(t, err, "was released")
			require.ErrorContains(t, err, summary.ID)
			_, err = runCLI(t, "search-export", "show", summary.ID)
			code, ok := daemonconn.ProblemCode(err)
			require.True(t, ok)
			require.Equal(t, "report_unavailable", code)
		})
	}
}
