package main

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFreshCompletionGroupRejectsUnknownChild(t *testing.T) {
	fresh := &cobra.Command{Use: "docbank", SilenceErrors: true, SilenceUsage: true}
	fresh.AddCommand(&cobra.Command{Use: "probe", Run: func(*cobra.Command, []string) {}})
	configureCommandGroups(fresh)
	var out bytes.Buffer
	fresh.SetOut(&out)
	fresh.SetErr(&out)
	fresh.SetArgs([]string{"completion", "unknown-child-"})
	cmd, err := fresh.ExecuteC()
	require.ErrorContains(t, err, `unknown command "unknown-child-" for "docbank completion"`)
	assert.Empty(t, out.String())
	assert.Contains(t, commandErrorHint(cmd, err, 2, false), "docbank completion --help")
	fresh.SetArgs([]string{"completion"})
	_, err = fresh.ExecuteC()
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Available Commands:")
}

func TestMalformedOutputFlagsKeepLastValidMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		flags       []string
		json, agent bool
	}{
		{"json enabled then malformed", []string{"--json", "--json=invalid-"}, true, false},
		{"json false then malformed", []string{"--json=false", "--json=invalid-"}, false, false},
		{"json malformed first", []string{"--json=invalid-"}, false, false},
		{"agent enabled then malformed", []string{"--agent", "--agent=invalid-"}, false, true},
		{"agent false then malformed", []string{"--agent=false", "--agent=invalid-"}, false, false},
		{"agent malformed first", []string{"--agent=invalid-"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(rootCmd)
			var out, diag bytes.Buffer
			args := append([]string{"search"}, tc.flags...)
			assert.Equal(t, 2, runProcess(args, &out, &diag))
			assert.Empty(t, out.String())
			if tc.json {
				var env processErrorEnvelope
				require.NoError(t, json.Unmarshal(diag.Bytes(), &env))
				assert.Equal(t, 2, env.Error.Exit)
				assert.Contains(t, env.Error.Hint, "docbank search --help")
			} else {
				assert.True(t, strings.HasPrefix(diag.String(), "error: "))
				prefix := "hint:"
				if tc.agent {
					prefix = "Hint:"
				}
				assert.Contains(t, diag.String(), "\n"+prefix+" ")
			}
		})
	}
}

func TestInheritedOutputModeFlagKeepsLastValidValue(t *testing.T) {
	original := rootCmd
	fresh := &cobra.Command{Use: "docbank", SilenceErrors: true, SilenceUsage: true}
	export := &cobra.Command{Use: "export", Args: cobra.NoArgs}
	export.PersistentFlags().Bool("json", false, "print JSON errors")
	export.AddCommand(&cobra.Command{Use: "preview", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {}})
	fresh.AddCommand(export)
	rootCmd = fresh
	t.Cleanup(func() { rootCmd = original })

	var out, diag bytes.Buffer
	require.Equal(t, exitUsage, runProcess([]string{"export", "preview", "--json", "--json=invalid"}, &out, &diag))
	require.Empty(t, out.String())
	var env processErrorEnvelope
	require.NoError(t, json.Unmarshal(diag.Bytes(), &env))
	assert.Equal(t, 2, env.Error.Exit)
	assert.Equal(t, "usage", env.Error.Code)
}
