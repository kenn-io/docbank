package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

const inspectionTestVaultID = "123e4567-e89b-42d3-a456-426614174001"

func TestProcessingCoveragePreflight(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	t.Setenv("DOCBANK_HOME", home)
	for _, args := range [][]string{
		{},
		{"--all-live=false", "--profile", "archive"},
		{processingTestVersionID, "--all-live", "--profile", "archive"},
		{"--all-live"},
		{"--all-live", "--profile", "Archive"},
		{"bad", "--profile", "archive"},
		{strings.ToUpper(processingTestVersionID), "--profile", "archive"},
		{processingTestVersionID},
		{processingTestVersionID, "--profile", "Archive"},
		{processingTestVersionID, "--profile", strings.Repeat("a", 129)},
		{processingTestVersionID, "--profile", "a\nb"},
	} {
		_, err := runCLI(t, append([]string{"processing", "coverage"}, args...)...)
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true), args)
		require.NoDirExists(t, home)
	}
}

func TestProcessingCoverageCLI(t *testing.T) {
	coverage := api.CoverageReport{VaultUID: inspectionTestVaultID,
		ProfileFingerprint: strings.Repeat("b", 64), State: "partial",
		Renditions: api.CoverageClass{Name: "rendition", Required: true, State: "rebuilding",
			Total: 1, Complete: 1, Rebuilding: 1, PreviousGenerationServing: 1},
		Embeddings: []api.CoverageClass{{Name: "semantic\nquoted", State: "unavailable",
			Required: true, Total: 1, Unavailable: 1}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/audit/status":
			assert.Empty(t, r.URL.RawQuery)
			assert.NoError(t, json.MarshalWrite(w, api.AuditStatus{VaultID: inspectionTestVaultID}))
		case "GET /api/v1/coverage":
			assert.Equal(t, "archive", r.URL.Query().Get("profile"))
			assert.Equal(t, inspectionTestVaultID, r.URL.Query().Get("vault_uid"))
			assert.Equal(t, []string{processingTestVersionID}, r.URL.Query()["content_version_id"])
			assert.NoError(t, json.MarshalWrite(w, coverage))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := daemonconn.New(server.URL, "test-key")
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	cmd, output := processingTestCommand()
	require.NoError(t, runProcessingCoverage(cmd, c, processingTestVersionID, "archive", true))
	var got struct {
		ContentVersionID string             `json:"content_version_id"`
		Coverage         api.CoverageReport `json:"coverage"`
	}
	require.NoError(t, json.Unmarshal(output.Bytes(), &got))
	require.Equal(t, processingTestVersionID, got.ContentVersionID)
	require.Equal(t, coverage, got.Coverage)
	for _, field := range []string{"ineligible", "stale", "previous_generation_serving"} {
		require.Contains(t, output.String(), `"`+field+`":`)
	}
	output.Reset()
	require.NoError(t, runProcessingCoverage(cmd, c, processingTestVersionID, "archive", false))
	for _, want := range []string{processingTestVersionID, inspectionTestVaultID,
		coverage.ProfileFingerprint, `"semantic\nquoted"`, "previous-generation-serving", "partial"} {
		require.Contains(t, output.String(), want)
	}
}

func TestProcessingCoverageRefusesInvalidResponses(t *testing.T) {
	for _, scenario := range []string{"vault", "nil audit", "bad audit", "profile"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/audit/status" {
					switch scenario {
					case "nil audit":
						_, _ = w.Write([]byte("null"))
					case "bad audit":
						assert.NoError(t, json.MarshalWrite(w, api.AuditStatus{VaultID: "bad"}))
					default:
						assert.NoError(t, json.MarshalWrite(w, api.AuditStatus{VaultID: inspectionTestVaultID}))
					}
					return
				}
				if scenario == "profile" {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = w.Write([]byte(`{"status":422,"code":"processing_profile_unavailable"}`))
					return
				}
				assert.NoError(t, json.MarshalWrite(w, api.CoverageReport{VaultUID: processingTestVersionID}))
			}))
			t.Cleanup(server.Close)
			c := daemonconn.New(server.URL, "test-key")
			t.Cleanup(func() { require.NoError(t, c.Close()) })
			cmd, output := processingTestCommand()
			err := runProcessingCoverage(cmd, c, processingTestVersionID, "archive", true)
			require.Error(t, err)
			require.Equal(t, exitGeneral, commandExitCode(err, true))
			require.Empty(t, output.String())
			if scenario == "profile" {
				require.ErrorContains(t, err, "executable")
				require.ErrorContains(t, err, "processing profiles --json")
				code, ok := daemonconn.ProblemCode(err)
				require.True(t, ok)
				require.Equal(t, "processing_profile_unavailable", code)
			}
		})
	}
}

func TestProcessingCoverageAllLiveCLI(t *testing.T) {
	count := 5001
	coverage := api.CoverageReport{VaultUID: inspectionTestVaultID, ProfileFingerprint: strings.Repeat("b", 64), State: "partial",
		Scope: &api.DocumentSearchScope{Kind: "all_live"}, ObservedScopeCount: &count,
		Renditions: api.CoverageClass{Name: "rendition", Required: true, State: "partial", Total: count, Unavailable: count}, Embeddings: []api.CoverageClass{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/audit/status":
			assert.NoError(t, json.MarshalWrite(w, api.AuditStatus{VaultID: inspectionTestVaultID}))
		case "POST /api/v1/coverage":
			assert.Empty(t, r.URL.RawQuery)
			var body api.DocumentCoverageRequest
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
				return
			}
			assert.Equal(t, "archive", body.Profile)
			assert.Equal(t, inspectionTestVaultID, body.Fence.VaultUID)
			assert.Empty(t, body.Fence.ContentVersionIDs)
			if !assert.NotNil(t, body.Scope) {
				return
			}
			assert.Equal(t, "all_live", body.Scope.Kind)
			assert.NoError(t, json.MarshalWrite(w, coverage))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := daemonconn.New(server.URL, "test-key")
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	cmd, output := processingTestCommand()
	require.NoError(t, runProcessingCoverage(cmd, c, "", "archive", true))
	var got map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &got))
	require.NotContains(t, got, "content_version_id")
	report, ok := got["coverage"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 5001, report["observed_scope_count"])
	assert.Equal(t, map[string]any{"kind": "all_live"}, report["scope"])
	output.Reset()
	require.NoError(t, runProcessingCoverage(cmd, c, "", "archive", false))
	require.Contains(t, output.String(), "scope: all_live\nobserved-scope-count: 5001\n")
	require.NotContains(t, output.String(), "version:")
}

// A missing XOR check could start the daemon for an ambiguous selection.
func FuzzProcessingCoverageSelection(f *testing.F) {
	for _, count := range []uint8{0, 1, 2, 3} {
		f.Add(count, false)
		f.Add(count, true)
	}
	f.Fuzz(func(t *testing.T, count uint8, allLive bool) {
		cmd := newProcessingCoverageCommand()
		args := []string{"--all-live=false"}
		if allLive {
			args[0] = "--all-live=true"
		}
		for range int(count) {
			args = append(args, processingTestVersionID)
		}
		require.NoError(t, cmd.ParseFlags(args))
		err := cmd.Args(cmd, cmd.Flags().Args())
		valid := count == 0 && allLive || count == 1 && !allLive
		if valid {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
			require.Equal(t, exitUsage, commandExitCode(err, true))
		}
	})
}
