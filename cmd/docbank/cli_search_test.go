package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/clisearch"
)

var cliSearchTasks = []struct{ query, command string }{
	{"import a folder of PDFs", "docbank document add"},
	{"show the virtual directory tree", "docbank document tree"},
	{"download a verified copy of a document", "docbank document get"},
	{"read original document bytes", "docbank document cat"},
	{"inspect a document by its id", "docbank document stat"},
	{"move a document to a different folder", "docbank document move"},
	{"restore a document from trash", "docbank document restore"},
	{"restore a backup snapshot into a separate vault", "docbank backup restore"},
	{"list immutable versions of a file", "docbank document versions list"},
	{"find documents by text and tag", "docbank search"},
	{"create a backup snapshot", "docbank backup create"},
	{"verify a backup repository", "docbank backup verify"},
	{"import an email mailbox archive", "docbank email mailbox import"},
	{"show where a document came from", "docbank document provenance"},
	{"assign a tag to a document", "docbank tag assign"},
	{"pack loose blobs", "docbank storage pack"},
	{"read a recording transcript", "docbank media transcript"},
	{"list browser sign-in sessions", "docbank web sessions"},
	{"revoke a browser sign-in session", "docbank web sessions revoke"},
	{"print MCP client configuration", "docbank mcp config"},
}

func TestCLISearchTasks(t *testing.T) {
	bin := buildDocbank(t)
	hits1, hits3 := 0, 0
	for _, task := range cliSearchTasks {
		t.Run(task.query, func(t *testing.T) {
			out, diag := runCommandDiscoveryProcess(t, bin, task.query, "--json", "--limit", "20")
			require.Empty(t, diag)
			var response clisearch.Response
			require.NoError(t, json.Unmarshal([]byte(out), &response))
			require.NotEmpty(t, response.Results)
			rank := 0
			for i, result := range response.Results {
				if result.Command == task.command {
					rank = i + 1
					break
				}
			}
			if rank == 1 {
				hits1++
			}
			if rank > 0 && rank <= 3 {
				hits3++
			}
			t.Logf("expected rank %d: %s", rank, task.command)
			assert.Equal(t, task.command, response.Results[0].Command)
			for _, result := range response.Results {
				cmd, remaining, err := rootCmd.Find(strings.Fields(strings.TrimPrefix(result.Command, "docbank ")))
				require.NoError(t, err)
				assert.Empty(t, remaining)
				assert.True(t, cmd.Runnable())
				for ancestor := cmd; ancestor != nil; ancestor = ancestor.Parent() {
					assert.False(t, ancestor.Hidden)
					assert.Empty(t, ancestor.Deprecated)
				}
				assert.NotContains(t, []string{"docbank tui", "docbank web", "docbank mcp", "docbank daemon run"}, result.Command)
				if cmd.HasSubCommands() {
					assert.True(t, workParents[result.Command], "recommended a help-only group: %s", result.Command)
				}
			}
		})
	}
	t.Logf("task evaluation: hit@1=%d/%d hit@3=%d/%d", hits1, len(cliSearchTasks), hits3, len(cliSearchTasks))
}

func TestCLISearchSkips(t *testing.T) {
	for _, path := range []string{"tui", "web", "mcp", "daemon run", "document", "photos hidden", "processing", "document versions", "email mailbox", "completion", "cli search"} {
		t.Run(path, func(t *testing.T) {
			code, out, diag := runCommandDiscovery(t, path, "--json", "--limit", "20")
			require.Equal(t, exitSuccess, code, diag)
			var response clisearch.Response
			require.NoError(t, json.Unmarshal([]byte(out), &response))
			for _, result := range response.Results {
				assert.NotEqual(t, "docbank "+path, result.Command)
			}
		})
	}
}

func TestCLISearchLiveTree(t *testing.T) {
	called := func(*cobra.Command, []string) error {
		t.Fatal("command discovery invoked a candidate callback")
		return nil
	}
	parent := &cobra.Command{Use: "sentinel-parent", Short: "Quasar calibration", GroupID: groupOperations, Args: called, PreRunE: called, RunE: called}
	parent.ValidArgsFunction = func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		t.Fatal("command discovery invoked completion")
		return nil, cobra.ShellCompDirectiveDefault
	}
	child := &cobra.Command{Use: "sentinel-child", Short: "Pulsar calibration", RunE: called}
	parent.AddCommand(child)
	leaf := &cobra.Command{Use: "sentinel-leaf", Short: "Neutrino calibration", GroupID: groupOperations, RunE: called}
	group := &cobra.Command{Use: "sentinel-group", Short: "Nebula calibration", GroupID: groupOperations}
	group.AddCommand(&cobra.Command{Use: "sentinel-action", Short: "Meteor calibration", RunE: called})
	hidden := &cobra.Command{Use: "sentinel-hidden", Hidden: true, RunE: called}
	hidden.AddCommand(&cobra.Command{Use: "sentinel-descendant", Short: "Supernova calibration", RunE: called})
	rootCmd.AddCommand(parent, leaf, group, hidden)
	t.Cleanup(func() { rootCmd.RemoveCommand(parent, leaf, group, hidden) })
	for query, expected := range map[string]string{
		"quasar": "docbank sentinel-parent", "pulsar": "docbank sentinel-parent sentinel-child",
		"neutrino": "docbank sentinel-leaf", "meteor": "docbank sentinel-group sentinel-action",
		"nebula": "", "supernova": "",
	} {
		code, out, diag := runCommandDiscovery(t, query, "--json")
		require.Equal(t, exitSuccess, code, diag)
		var response clisearch.Response
		require.NoError(t, json.Unmarshal([]byte(out), &response))
		if expected == "" {
			assert.Empty(t, response.Results, query)
		} else {
			require.NotEmpty(t, response.Results, query)
			assert.Equal(t, expected, response.Results[0].Command, query)
		}
	}
}

func runCommandDiscovery(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	resetDiscoveryFlags()
	t.Cleanup(func() { resetDiscoveryFlags() })
	var out, diag bytes.Buffer
	code := runProcess(append([]string{"cli", "search"}, args...), &out, &diag)
	return code, out.String(), diag.String()
}

func runCommandDiscoveryProcess(t *testing.T, bin string, args ...string) (string, string) {
	t.Helper()
	home := t.TempDir()
	vault := filepath.Join(home, "vault")
	cmd := exec.CommandContext(t.Context(), bin, append([]string{"cli", "search"}, args...)...)
	cmd.Env = cliSearchChildEnv(home, vault)
	var out, diag bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diag
	require.NoError(t, cmd.Run(), diag.String())
	return out.String(), diag.String()
}

func cliSearchChildEnv(home, vault string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "DOCBANK_") || strings.HasPrefix(key, "XDG_") ||
			key == "HOME" || key == "USERPROFILE" || key == "CODEX_HOME" ||
			key == "CLAUDE_CONFIG_DIR" || key == "PI_CODING_AGENT_DIR" {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+home,
		"USERPROFILE="+home,
		"DOCBANK_HOME="+vault,
		"XDG_CONFIG_HOME="+home,
		"XDG_DATA_HOME="+home,
		"XDG_CACHE_HOME="+home,
		"XDG_RUNTIME_DIR="+home,
		"DOCBANK_API_PORT=invalid",
		"DOCBANK_API_KEY_FILE="+filepath.Join(home, "absent-key"),
	)
}

// Catch adapter mistakes in stdout/stderr selection and native usage errors.
func TestCLISearchOutputAndUsage(t *testing.T) {
	t.Setenv("DOCBANK_HOME", filepath.Join(t.TempDir(), "absent-vault"))
	code, out, diag := runCommandDiscovery(t, "pack", "loose", "blobs", "--json", "--limit", "1")
	require.Equal(t, exitSuccess, code, diag)
	require.Empty(t, diag)
	assert.JSONEq(t,
		`{"query":"pack loose blobs","results":[{"command":"docbank storage pack","summary":"Pack authorized loose blobs into immutable pack files","usage":"docbank storage pack"}]}`,
		out,
	)

	code, out, diag = runCommandDiscovery(t, "pack loose blobs", "--json=false", "--limit", "1")
	require.Equal(t, exitSuccess, code, diag)
	assert.Equal(t, "docbank storage pack  Pack authorized loose blobs into immutable pack files\nRun 'docbank storage pack --help' for flags and examples.\n", out)
	assert.Empty(t, diag)

	for _, args := range [][]string{{}, {" "}, {"pack", "--limit", "0"}, {"pack", "--limit", "21"}, {"pack", "--limit", "invalid"}, {"pack", "--unknown"}} {
		for _, mode := range []string{"--json", "--json=false"} {
			t.Run(strings.Join(append(args, mode), "/"), func(t *testing.T) {
				code, out, diag := runCommandDiscovery(t, append([]string{mode}, args...)...)
				assert.Equal(t, exitUsage, code)
				assert.Empty(t, out)
				if mode == "--json" {
					var envelope processErrorEnvelope
					require.NoError(t, json.Unmarshal([]byte(diag), &envelope))
					assert.Equal(t, "usage", envelope.Error.Code)
					assert.Equal(t, exitUsage, envelope.Error.Exit)
					assert.Contains(t, envelope.Error.Hint, "docbank cli search --help")
					if strings.Contains(strings.Join(args, " "), "--unknown") || strings.Contains(strings.Join(args, " "), "invalid") {
						assert.Equal(t, `run "docbank cli search --help" for flags and examples`, envelope.Error.Hint)
					}
				} else {
					assert.Contains(t, diag, "error: ")
					assert.Contains(t, diag, "hint:")
				}
			})
		}
	}

	code, out, diag = runCommandDiscovery(t, "xqzvnevermatches", "--json")
	require.Equal(t, exitSuccess, code, diag)
	assert.JSONEq(t, `{"query":"xqzvnevermatches","results":[]}`, out)
	code, out, diag = runCommandDiscovery(t, "xqzvnevermatches")
	require.Equal(t, exitSuccess, code, diag)
	assert.Equal(t, "Run 'docbank --help' for flags and examples.\n", out)
}

func TestCLISearchWriterFailure(t *testing.T) {
	for _, mode := range []string{"--json", "--json=false"} {
		resetDiscoveryFlags()
		var diag bytes.Buffer
		code := runProcess([]string{"cli", "search", "pack loose blobs", mode}, reportTestWriter(func([]byte) (int, error) {
			return 0, io.ErrClosedPipe
		}), &diag)
		assert.Equal(t, exitGeneral, code, diag.String())
		assert.Contains(t, diag.String(), io.ErrClosedPipe.Error())
	}
	t.Cleanup(func() { resetDiscoveryFlags() })
}

// A fresh process must not resolve a vault, read config or start a daemon.
func TestCLISearchFreshHome(t *testing.T) {
	bin := buildDocbank(t)
	for _, poisoned := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent vault", true: "invalid config"}[poisoned], func(t *testing.T) {
			home, vault := t.TempDir(), filepath.Join(t.TempDir(), "vault")
			if poisoned {
				require.NoError(t, os.Mkdir(vault, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(vault, "config.toml"), []byte("[invalid TOML"), 0o600))
			}
			cmd := exec.CommandContext(t.Context(), bin, "cli", "search", "pack loose blobs", "--json")
			cmd.Env = cliSearchChildEnv(home, vault)
			var out, diag bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &diag
			require.NoError(t, cmd.Run(), diag.String())
			require.Empty(t, diag.String())
			var response clisearch.Response
			require.NoError(t, json.Unmarshal(out.Bytes(), &response))
			require.NotEmpty(t, response.Results)
			entries, err := os.ReadDir(home)
			require.NoError(t, err)
			assert.Empty(t, entries)
			entries, err = os.ReadDir(vault)
			if poisoned {
				require.NoError(t, err)
				require.Len(t, entries, 1)
				assert.Equal(t, "config.toml", entries[0].Name())
			} else {
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

// pflag writes zero on malformed integers before setting Changed. Reset this
// adapter-owned value explicitly after parse failures in an in-process test.
func resetDiscoveryFlags() {
	resetFlags(rootCmd)
	cmd, _, err := rootCmd.Find([]string{"cli", "search"})
	if err == nil {
		_ = cmd.Flags().Lookup("limit").Value.Set("5")
	}
}

func TestCLISearchBeforeAndAfterGroupConfiguration(t *testing.T) {
	root := &cobra.Command{Use: "docbank"}
	group := &cobra.Command{Use: "group", Short: "nebula"}
	child := &cobra.Command{Use: "child", Short: "quasar", RunE: func(*cobra.Command, []string) error { t.Fatal("executed callback"); return nil }}
	group.AddCommand(child)
	root.AddCommand(group)
	before := clisearch.Search(root, "quasar", 20, cliSearchOptions())
	configureCommandGroups(root)
	assert.Equal(t, before, clisearch.Search(root, "quasar", 20, cliSearchOptions()))
	assert.Empty(t, clisearch.Search(root, "nebula", 20, cliSearchOptions()))
}

func BenchmarkCLISearch(b *testing.B) {
	configureCommandGroups(rootCmd)
	opts := cliSearchOptions()
	for b.Loop() {
		clisearch.Search(rootCmd, "import a folder of PDFs", 5, opts)
	}
}
func BenchmarkCLISearchStartup(b *testing.B) {
	bin := buildDocbank(b)
	home := b.TempDir()
	b.Setenv("HOME", home)
	b.Setenv("USERPROFILE", home)
	b.Setenv("DOCBANK_HOME", filepath.Join(home, "absent"))
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		b.Setenv(key, home)
	}
	for b.Loop() {
		out, err := exec.CommandContext(b.Context(), bin, "cli", "search", "import a folder of PDFs").CombinedOutput()
		require.NoError(b, err, string(out))
	}
}

func TestCLISearchCompactOutput(t *testing.T) {
	code, text, diag := runCommandDiscovery(t, "import a folder of PDFs")
	require.Equal(t, exitSuccess, code, diag)
	require.Empty(t, diag)
	assert.LessOrEqual(t, len(text), 1024, "typical task output should fit in 1 KiB")
	_, out, diag := runCommandDiscovery(t, "document", "--json")
	require.Empty(t, diag)
	var response clisearch.Response
	require.NoError(t, json.Unmarshal([]byte(out), &response))
	assert.Len(t, response.Results, 5, "default limit")
	documentHelp, err := runCLI(t, "document", "--help")
	require.NoError(t, err)
	t.Logf("typical search text: %d bytes; unchanged document help: %d bytes", len(text), len(documentHelp))
}
