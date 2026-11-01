package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/report"
)

var commandGroupCases = map[string]string{
	"add": "document add", "cat": "document cat", "edit": "document edit",
	"get": "document get", "ls": "document list", "mkdir": "document mkdir",
	"mv": "document move", "provenance": "document provenance", "put": "document put",
	"refs": "document refs", "restore": "document restore", "revert": "document revert",
	"rm": "document remove", "stat": "document stat", "tree": "document tree",
	"versions": "document versions", "info": "vault info", "gc": "vault gc",
	"verify": "vault verify", "email-documents": "email documents",
	"email-pdf": "email pdf", "mailbox": "email mailbox", "search-export": "report",
}

func TestCommandGroupsTree(t *testing.T) {
	for legacy, canonical := range commandGroupCases {
		t.Run(legacy, func(t *testing.T) {
			cmd, remaining, err := rootCmd.Find(strings.Fields(canonical))
			require.NoError(t, err)
			require.Empty(t, remaining)
			assert.Equal(t, "docbank "+canonical, cmd.CommandPath())
			assert.False(t, cmd.Hidden)
			assert.Empty(t, cmd.Deprecated)
			old, remaining, err := rootCmd.Find([]string{legacy})
			require.NoError(t, err)
			require.Empty(t, remaining)
			assert.True(t, old.Hidden)
			assert.Equal(t, "docbank "+canonical, old.Annotations[legacyCommandReplacement])
		})
	}
}

func TestCommandGroupsCompletion(t *testing.T) {
	for _, tc := range []struct {
		args            []string
		present, absent string
	}{
		{[]string{""}, "document", "email-documents"},
		{[]string{"document", ""}, "list", "ls"},
		{[]string{"vault", ""}, "verify", "document"},
		{[]string{"email", ""}, "mailbox", "email-pdf"},
		{[]string{"document", "versions", ""}, "prune", "versions"},
	} {
		t.Run(strings.Join(tc.args, "/"), func(t *testing.T) {
			out, err := runCLI(t, append([]string{"__complete"}, tc.args...)...)
			require.NoError(t, err)
			var names []string
			for line := range strings.SplitSeq(out, "\n") {
				name, _, _ := strings.Cut(line, "\t")
				names = append(names, name)
			}
			assert.Contains(t, names, tc.present)
			assert.NotContains(t, names, tc.absent)
		})
	}
	for name, generate := range map[string]func(*bytes.Buffer) error{
		"bash":       func(b *bytes.Buffer) error { return rootCmd.GenBashCompletionV2(b, true) },
		"zsh":        func(b *bytes.Buffer) error { return rootCmd.GenZshCompletion(b) },
		"fish":       func(b *bytes.Buffer) error { return rootCmd.GenFishCompletion(b, true) },
		"powershell": func(b *bytes.Buffer) error { return rootCmd.GenPowerShellCompletionWithDesc(b) },
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, generate(&out))
			assert.NotEmpty(t, out.String())
		})
	}
}

func TestCommandGroupsHTTPCompatibility(t *testing.T) {
	var requests []string
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		assert.Equal(t, "/api/v1/info", r.URL.Path)
		_, _ = w.Write([]byte(`{"vault_id":"synthetic-vault","vault_path":"/synthetic/vault"}`))
	})
	defer resetFlags(rootCmd)
	var canonical, legacy bytes.Buffer
	var canonicalErr, legacyErr bytes.Buffer
	resetFlags(rootCmd)
	assert.Equal(t, 0, runProcess([]string{"vault", "info", "--json"}, &canonical, &canonicalErr))
	resetFlags(rootCmd)
	assert.Equal(t, 0, runProcess([]string{"info", "--json"}, &legacy, &legacyErr))
	assert.Equal(t, canonical.String(), legacy.String())
	assert.NotEmpty(t, canonical.String())
	assert.Empty(t, canonicalErr.String())
	assert.Contains(t, legacyErr.String(), "deprecated, use 'docbank vault info' instead")
	require.Len(t, requests, 2)
	assert.Equal(t, requests[0], requests[1])
}

func TestCommandGroupsUsageAndNestedDeprecation(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	defer resetFlags(rootCmd)
	for _, args := range [][]string{
		{"document", "bogus"}, {"vault", "bogus"}, {"email", "bogus"},
		{"document", "get", "id:12"}, {"get", "id:12"},
		{"email", "documents", "release", "synthetic-operation"},
		{"email-documents", "release", "synthetic-operation"},
	} {
		resetFlags(rootCmd)
		var out, stderr bytes.Buffer
		assert.Equal(t, 2, runProcess(args, &out, &stderr), "%v: %s", args, stderr.String())
		assert.Empty(t, out.String())
	}
	resetFlags(rootCmd)
	var out, stderr bytes.Buffer
	assert.Equal(t, 2, runProcess([]string{"versions", "list", "id:12", "--limit", "-1"}, &out, &stderr))
	assert.Contains(t, stderr.String(), "deprecated, use 'docbank document versions list' instead")
}

func TestCommandGroupsRemoveGuidanceMatchesInvokedSpelling(t *testing.T) {
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST /api/v1/path/trash", r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"id":12,"path":"/synthetic/note.txt"}`))
	})
	for _, tc := range []struct {
		args    []string
		restore string
	}{
		{[]string{"rm", "/synthetic/note.txt"}, "docbank restore"},
		{[]string{"document", "remove", "/synthetic/note.txt"}, "docbank document restore"},
	} {
		out, err := runCLI(t, tc.args...)
		require.NoError(t, err)
		assert.Equal(t, "trashed [id:12] /synthetic/note.txt (restore with: "+tc.restore+" id:12)\n", out)
	}
}

func TestCommandGroupsDeprecationBeforeParsingAndHelp(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	defer resetFlags(rootCmd)
	for _, tc := range []struct {
		args []string
		code int
		path string
	}{
		{[]string{"get", "id:12"}, 2, "document get"},
		{[]string{"get", "--not-a-flag"}, 2, "document get"},
		{[]string{"get", "--help"}, 0, "document get"},
		{[]string{"search-export", "--help"}, 0, "report"},
		{[]string{"versions", "list", "--help"}, 0, "document versions list"},
	} {
		t.Run(strings.Join(tc.args, "/"), func(t *testing.T) {
			resetFlags(rootCmd)
			var out, stderr bytes.Buffer
			assert.Equal(t, tc.code, runProcess(tc.args, &out, &stderr))
			assert.Equal(t, 1, strings.Count(stderr.String(), "is deprecated"))
			assert.Contains(t, stderr.String(), "use 'docbank "+tc.path+"' instead")
			assert.NotContains(t, out.String(), "is deprecated")
		})
	}
}

// Unknown flags must fail before any daemon access, regardless of their value,
// and compatibility warnings must never enter stdout or change the exit code.
func FuzzCommandGroupsLegacyParsing(f *testing.F) {
	for legacy := range commandGroupCases {
		f.Add(legacy, "synthetic")
	}
	f.Fuzz(func(t *testing.T, legacy, value string) {
		canonical, ok := commandGroupCases[legacy]
		if !ok {
			t.Skip()
		}
		t.Setenv("DOCBANK_HOME", t.TempDir())
		defer resetFlags(rootCmd)
		flag := "--not-a-real-flag=" + value
		var out, stderr bytes.Buffer
		resetFlags(rootCmd)
		assert.Equal(t, exitUsage, runProcess([]string{legacy, flag}, &out, &stderr))
		assert.Empty(t, out.String())
		assert.Equal(t, 1, strings.Count(stderr.String(), "is deprecated"))
		assert.Contains(t, stderr.String(), "use 'docbank "+canonical+"' instead")
		out.Reset()
		stderr.Reset()
		resetFlags(rootCmd)
		assert.Equal(t, exitUsage, runProcess(append(strings.Fields(canonical), flag), &out, &stderr))
		assert.Empty(t, out.String())
		assert.NotContains(t, stderr.String(), "is deprecated")
	})
}

func TestCommandGroupsReportGuidanceMatchesInvokedSpelling(t *testing.T) {
	id := strings.Repeat("a", 48)
	request := report.Request{Version: 1, AllDocuments: true, Timezone: "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "alpha", Syntax: "simple", Dates: report.DateRange{Start: "2024-01-01", End: "2026-12-31"}}},
	}
	observed := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	summary := report.Summary{ID: id, State: "needs_review", UnresolvedDates: 1,
		ObservedAt: observed, ExpiresAt: observed.Add(time.Hour), Terms: request.Terms}
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/search-exports" {
			assert.NoError(t, json.MarshalWrite(w, store.TermReportHistoryPage{
				Total: 2, Items: []store.TermReportHistory{{Summary: summary}},
			}))
			return
		}
		if r.URL.Path == "/api/v1/search-exports/"+id+"/revisions" {
			child := summary
			child.ID, child.ParentID = strings.Repeat("b", 48), id
			assert.NoError(t, json.MarshalWrite(w, child))
			return
		}
		assert.NoError(t, json.MarshalWrite(w, summary))
	})
	input := filepath.Join(t.TempDir(), "request.json")
	requestJSON, err := json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(input, requestJSON, 0o600))
	choices := filepath.Join(t.TempDir(), "choices.json")
	require.NoError(t, os.WriteFile(choices, []byte("[]"), 0o600))
	for _, args := range [][]string{
		{"show", id}, {"history", "--limit", "1"},
		{"create", "--input", input, "--output", filepath.Join(t.TempDir(), "report.zip")},
		{"revise", id, "--choices", choices, "--output", filepath.Join(t.TempDir(), "revised.zip")},
	} {
		t.Run(args[0], func(t *testing.T) {
			var canonical string
			for _, spelling := range []string{"report", "search-export"} {
				out, err := runCLI(t, append([]string{spelling}, args...)...)
				require.NoError(t, err)
				assert.Contains(t, out, "docbank "+spelling+" ")
				if spelling == "report" {
					assert.NotContains(t, out, "search-export")
					canonical = out
				} else {
					assert.Equal(t, strings.ReplaceAll(canonical, "docbank report ", "docbank search-export "), out)
				}
			}
		})
	}
}

func TestCommandGroupsReportValidationMatchesInvokedSpelling(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, spelling := range []string{"report", "search-export"} {
		for _, args := range [][]string{
			{"create"}, {"csv", "synthetic.zip"},
			{"revise", strings.Repeat("a", 48)}, {"download", strings.Repeat("a", 48)},
		} {
			out, err := runCLI(t, append([]string{spelling}, args...)...)
			require.ErrorContains(t, err, spelling+" "+args[0]+" requires --")
			assert.Empty(t, out)
			assert.Equal(t, exitUsage, commandExitCode(err, true))
		}
	}
}

func TestCommandGroupsSearchStillAcceptsActionWords(t *testing.T) {
	for _, word := range []string{"export", "report", "list", "document"} {
		cmd, args, err := rootCmd.Find([]string{"search", word})
		require.NoError(t, err)
		assert.Same(t, searchCmd, cmd)
		assert.Equal(t, []string{word}, args)
	}
}

func TestCommandGroupsDocumentWorkflow(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "notes.txt", "synthetic grouped content\n")
	_, err := runCLI(t, "document", "add", source, "--dest", "/grouped")
	require.NoError(t, err)
	out, err := runCLI(t, "document", "list", "/grouped")
	require.NoError(t, err)
	assert.Contains(t, out, "notes.txt")
	_, err = runCLI(t, "document", "move", "/grouped/notes.txt", "/grouped/moved.txt")
	require.NoError(t, err)
	out, err = runCLI(t, "document", "cat", "/grouped/moved.txt")
	require.NoError(t, err)
	assert.Equal(t, "synthetic grouped content\n", out)
	destination := filepath.Join(t.TempDir(), "download.txt")
	_, err = runCLI(t, "document", "get", "/grouped/moved.txt", destination, "--json")
	require.NoError(t, err)
	content, err := os.ReadFile(destination)
	require.NoError(t, err)
	assert.Equal(t, out, string(content))
	out, err = runCLI(t, "document", "stat", "/grouped/moved.txt", "--json")
	require.NoError(t, err)
	var node struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &node))
	for _, args := range [][]string{
		{"document", "versions", "list", "/grouped/moved.txt", "--json"},
		{"vault", "info", "--json"}, {"vault", "verify"},
		{"document", "remove", "/grouped/moved.txt"},
		{"document", "restore", formatNodeSelector(node.ID)},
	} {
		_, err = runCLI(t, args...)
		require.NoError(t, err, "%v", args)
	}
}

// Assert every nested legacy definition has the same public flag and argument
// contracts, including required flag annotations and local/persistent scope.
func TestCommandGroupsNestedContracts(t *testing.T) {
	for legacy, canonical := range commandGroupCases {
		old, _, err := rootCmd.Find([]string{legacy})
		require.NoError(t, err)
		current, _, err := rootCmd.Find(strings.Fields(canonical))
		require.NoError(t, err)
		var compare func(*cobra.Command, *cobra.Command)
		compare = func(a, b *cobra.Command) {
			compareFlags := func(left, right *pflag.FlagSet) {
				left.VisitAll(func(flag *pflag.Flag) {
					if flag.Name == "help" {
						return
					}
					other := right.Lookup(flag.Name)
					require.NotNil(t, other, "%s --%s", b.CommandPath(), flag.Name)
					assert.Equal(t, flag.Value.Type(), other.Value.Type())
					assert.Equal(t, flag.DefValue, other.DefValue)
					assert.Equal(t, flag.Annotations, other.Annotations)
					assert.Equal(t, flag.Shorthand, other.Shorthand)
				})
			}
			compareFlags(a.LocalNonPersistentFlags(), b.LocalNonPersistentFlags())
			compareFlags(a.PersistentFlags(), b.PersistentFlags())
			require.Len(t, b.Commands(), len(a.Commands()))
			for i, child := range a.Commands() {
				compare(child, b.Commands()[i])
			}
		}
		compare(old, current)
	}
}
