package main

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestRenditionWindowPreflight(t *testing.T) {
	home := filepath.Join(t.TempDir(), "absent")
	t.Setenv("DOCBANK_HOME", home)
	for _, flags := range [][]string{
		{"--version", "bad"}, {"--profile", ""}, {"--attachment", ""},
		{"--attachment", strings.Repeat("A", 64)}, {"--offset", "1"},
		{"--offset", "-1"}, {"--offset", "2147483648"},
		{"--max-chars", "0"}, {"--max-chars", "16001"},
	} {
		args := []string{"rendition", "window", "id:42", "--version", processingTestVersionID,
			"--profile", "archive"}
		_, err := runCLI(t, append(args, flags...)...)
		require.Error(t, err)
		require.Equal(t, exitUsage, commandExitCode(err, true), flags)
		require.NoDirExists(t, home)
	}
	_, err := runCLI(t, "rendition", "window", "relative", "--version", processingTestVersionID,
		"--profile", "archive")
	require.Equal(t, exitUsage, commandExitCode(err, true))
	require.NoDirExists(t, home)
}

func TestRenditionWindowCatalogBounds(t *testing.T) {
	for _, test := range []struct {
		name, path, filename string
		bad                  bool
	}{
		{"depth boundary", "/" + strings.Repeat("d/", 255) + "f", "f", false},
		{"depth exceeded", "/" + strings.Repeat("d/", 256) + "f", "f", true},
		{"path boundary", "/" + strings.Repeat("d", 16381) + "/f", "f", false},
		{"path exceeded", "/" + strings.Repeat("d", 16382) + "/f", "f", true},
		{"unicode name boundary", "/" + strings.Repeat("界", 255), strings.Repeat("界", 255), false},
		{"unicode name exceeded", "/" + strings.Repeat("界", 256), strings.Repeat("界", 256), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkWindowCatalogBounds(api.Node{Path: test.path, Name: test.filename})
			if !test.bad {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "catalog")
			require.NotContains(t, err.Error(), "refresh")
			require.NotContains(t, err.Error(), "stale")
			require.Equal(t, exitGeneral, commandExitCode(err, true))
		})
	}
}

func TestRenditionWindowCLI(t *testing.T) {
	for _, scenario := range []string{"discovery", "pinned zero", "pinned continuation",
		"wrong build", "wrong profile", "pinned wrong profile", "missing profile", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			profile, attachment, build := sha256Hex("profile"), sha256Hex("attachment"), sha256Hex("build")
			options := renditionWindowOptions{selector: nodeSelector{id: 42},
				version: uuid.MustParse(processingTestVersionID), profile: "archive", maxChars: 3, asJSON: true}
			if strings.HasPrefix(scenario, "pinned") {
				options.attachment = attachment
			}
			if scenario == "pinned continuation" {
				options.offset = 1
			}
			var summaryCalls, windowCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/nodes/42":
					assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Kind: "file",
						Name: "notes.txt", Path: "/notes.txt", CurrentVersionID: processingTestVersionID}))
				case "GET /api/v1/processing/profiles":
					profiles := []api.ProcessingProfileSummary{}
					if scenario != "missing profile" {
						profiles = append(profiles, api.ProcessingProfileSummary{Name: "archive", Fingerprint: profile})
					}
					assert.NoError(t, json.MarshalWrite(w, profiles))
				case "GET /api/v1/audit/status":
					assert.NoError(t, json.MarshalWrite(w, api.AuditStatus{VaultID: inspectionTestVaultID}))
				case "POST /api/v1/documents/resolve":
					summaryCalls++
					var request api.DocumentSummaryResolveRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, []api.DocumentIdentity{{NodeID: 42,
						ContentVersionID: processingTestVersionID, Path: "/notes.txt"}}, request.Identities)
					if scenario == "stale" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"status":409,"code":"stale_version"}`))
						return
					}
					assert.NoError(t, json.MarshalWrite(w, api.DocumentSummaryResolveResponse{
						Items: []api.DocumentSummary{{NodeID: 42, ContentVersionID: processingTestVersionID,
							Name: "notes.txt", Path: "/notes.txt", ModifiedAt: "2026-10-04T12:00:00Z",
							ActiveRenditions: []api.DocumentRenditionIdentity{
								{ProfileFingerprint: sha256Hex("other"), AttachmentID: sha256Hex("other"), BuildID: build},
								{ProfileFingerprint: profile, AttachmentID: attachment, BuildID: build},
							}}}}))
				case "POST /api/v1/renditions/windows":
					windowCalls++
					var request api.RenditionWindowRequest
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, api.RenditionWindowRequest{VaultID: inspectionTestVaultID,
						NodeID: 42, ContentVersionID: processingTestVersionID, AttachmentID: attachment,
						Offset: options.offset, MaxChars: 3}, request)
					result := api.RenditionTextWindow{VaultID: inspectionTestVaultID, NodeID: 42,
						ContentVersionID: processingTestVersionID, AttachmentID: attachment,
						BuildID: build, ProfileFingerprint: profile, Checksum: sha256Hex("artifact"),
						MediaType: "text/markdown", Text: "é界🙂", RequestedOffset: options.offset,
						ActualStart: options.offset, ActualEnd: options.offset + 3,
						NextOffset: options.offset + 3, ResponseBytes: 9}
					if scenario == "wrong build" {
						result.BuildID = sha256Hex("wrong")
					}
					if strings.Contains(scenario, "wrong profile") {
						result.ProfileFingerprint = sha256Hex("wrong")
					}
					assert.NoError(t, json.MarshalWrite(w, result))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			c := daemonconn.New(server.URL, "test-key")
			t.Cleanup(func() { require.NoError(t, c.Close()) })
			cmd, output := processingTestCommand()
			err := runRenditionWindow(cmd, c, options)
			if strings.Contains(scenario, "wrong") || scenario == "stale" || scenario == "missing profile" {
				require.Error(t, err)
				require.Empty(t, output.String())
				if scenario == "pinned wrong profile" {
					require.Equal(t, exitNotFound, commandExitCode(err, true))
				}
				if scenario == "stale" {
					require.ErrorContains(t, err, "refresh")
					require.Zero(t, windowCalls)
				}
				if scenario == "missing profile" {
					require.ErrorContains(t, err, "executable")
					require.Zero(t, summaryCalls)
				}
				return
			}
			require.NoError(t, err)
			var result api.RenditionTextWindow
			require.NoError(t, json.Unmarshal(output.Bytes(), &result))
			require.Equal(t, "é界🙂", result.Text)
			require.Equal(t, options.offset+3, result.NextOffset)
			if options.attachment != "" {
				require.Zero(t, summaryCalls)
			} else {
				require.Equal(t, 1, summaryCalls)
			}
		})
	}
}

func TestRenditionWindowOutput(t *testing.T) {
	options := renditionWindowOptions{profile: "archive", maxChars: 3}
	window := api.RenditionTextWindow{NodeID: 42, ContentVersionID: processingTestVersionID,
		AttachmentID: sha256Hex("attachment"), Text: "é界🙂", ActualStart: 1, ActualEnd: 4, NextOffset: 4}
	cmd, output := processingTestCommand()
	require.NoError(t, writeRenditionWindow(cmd.OutOrStdout(), options, window))
	require.Contains(t, output.String(), "[1, 4)")
	require.Contains(t, output.String(), "é界🙂")
	require.Contains(t, output.String(), "docbank rendition window id:42 --version "+
		processingTestVersionID+" --profile archive --attachment "+window.AttachmentID+
		" --offset 4 --max-chars 3")
	output.Reset()
	window.EOF = true
	require.NoError(t, writeRenditionWindow(cmd.OutOrStdout(), options, window))
	require.NotContains(t, output.String(), "Next window:")
	failure := errors.New("closed output")
	w := reportTestWriter(func([]byte) (int, error) { return 0, failure })
	require.ErrorIs(t, writeRenditionWindow(w, options, window), failure)
	require.ErrorIs(t, writeProcessingCoverage(w, processingTestVersionID, "archive", api.CoverageReport{}),
		failure)
}
