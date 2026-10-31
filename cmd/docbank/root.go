package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/docbank/internal/version"
)

const (
	groupDocuments   = "docs"
	groupSearch      = "find"
	groupSources     = "sources"
	groupProductions = "legal"
	groupOperations  = "ops"
	groupInterfaces  = "ui"
	listCommand      = "list"
)

var rootCmd = &cobra.Command{
	Use:   "docbank",
	Short: "Keep, find, and verify documents in a local vault",
	Long: `Docbank keeps documents in a local vault: a virtual folder tree (/cases/a.pdf)
over immutable, hash-verified content versions. A background daemon owns the
vault; data commands start it automatically ("docbank daemon stop" ends it).

DOCBANK_HOME selects the vault (default ~/.docbank); the first data command
creates it. "docbank info --json" confirms its path and identity. Local data
commands use that vault; push --to selects an explicit remote daemon instead.

Agent workflow (replace example paths and ids with your own):
  docbank info --json                         # 1. confirm the selected vault
  docbank add ./scans --dest /cases/acme       # 2. import (creates folders)
  docbank tree /cases --json                  # 3. browse paths and ids
  docbank search invoice 2026 --agent         # 4. find by name or text
  docbank stat id:12 --json                   # 5. read current_version_id
  docbank cat /cases/acme/notes.txt            # 6. read original bytes
  docbank get id:12 ./invoice.pdf             # 7. save a verified copy
  docbank backup init --repo ~/backup         # 8. initialize a separate repo
  docbank backup create --repo ~/backup       # 9. capture a snapshot

Concepts:
  path-or-id  a node is "/abs/path" or "id:N"; id:N survives mv and rename
  version     put/edit add an immutable version (UUID); revert restores one
  text        search reads text/*, JSON and email bodies; PDFs and Office
              files need a processing profile (see "docbank processing")
  rendition   derived Markdown text made by a processing profile
  deletion    rm -> trash empty -> gc -> storage repack; each step is explicit

Output: results on stdout; errors, recovery hints and progress on stderr.
Use --json where listed in command help; search --agent gives compact hit rows.
With parsed --json, failures end with a JSON stderr record containing
error.message, error.code, error.exit and error.hint (when present).
Existing previews/progress may precede it. Flag parsing can fail before --json
is read; those errors stay human text. Human hints use hint:; agent uses Hint:.

Exit codes: 0 ok, 1 operational/internal error, 2 usage or request validation,
3 not found, 4 stale revision, 5 busy/locked, 6 integrity, 7 claim required.

Environment: DOCBANK_HOME selects vault data and config.toml.
Daemon startup overrides (restart the daemon after changing them):
  DOCBANK_BIND_ADDR bind IP (default loopback); DOCBANK_API_PORT API port
  DOCBANK_API_KEY or DOCBANK_API_KEY_FILE configured key (choose only one)
  DOCBANK_ALLOWED_HOSTS comma-separated HTTP Host allowlist
Without a configured key, the daemon generates a private per-run key.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Cobra checks required flags and flag groups after PersistentPreRun, so
	// validate them here before the process boundary marks command execution.
	PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
		if err := cmd.ValidateRequiredFlags(); err != nil {
			return usageError(err)
		}
		if err := cmd.ValidateFlagGroups(); err != nil {
			return usageError(err)
		}
		commandStarted = true
		return nil
	},
}

var programVersionCmd = &cobra.Command{
	Example: `  docbank version`,
	GroupID: groupOperations,
	Use:     "version",
	Short:   "Print the installed Docbank version",
	Long:    "Print this binary's version and source commit when reporting a problem.",
	Args:    cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "docbank version %s (%s)\n",
			version.Version, version.Commit)
	},
}

var commandStarted bool

func Execute() error {
	_, err := executeCommand()
	return err
}

func executeCommand() (*cobra.Command, error) {
	commandStarted = false
	configureCommandGroups(rootCmd)
	return rootCmd.ExecuteC() //nolint:wrapcheck // process boundary preserves user-facing errors
}

// Initialize Cobra's own groups before validating them too. A group without
// an action otherwise accepts an unknown child by showing help and succeeding.
func configureCommandGroups(cmd *cobra.Command) {
	if cmd.Parent() == nil {
		cmd.InitDefaultHelpCmd()
		cmd.InitDefaultCompletionCmd()
	}
	for _, flags := range []*pflag.FlagSet{cmd.Flags(), cmd.PersistentFlags()} {
		for _, name := range []string{"json", "agent"} {
			if flag := flags.Lookup(name); flag != nil && flag.Value.Type() == "bool" {
				if _, wrapped := flag.Value.(*outputModeValue); !wrapped {
					flag.Value = &outputModeValue{Value: flag.Value}
				}
			}
		}
	}
	for _, child := range cmd.Commands() {
		if child.HasSubCommands() && !child.Runnable() {
			child.Args = cobra.NoArgs
			child.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
		}
		configureCommandGroups(child)
	}
}

type outputModeValue struct{ pflag.Value }

// Pflag's boolean setter writes false before returning an invalid-value error.
// Keep the last successfully parsed mode available to the error renderer.
func (v *outputModeValue) Set(value string) error {
	underlying := v.Value
	previous := underlying.String()
	if err := underlying.Set(value); err != nil {
		_ = underlying.Set(previous)
		return err //nolint:wrapcheck // pflag supplies the flag name and invalid value
	}
	return nil
}

func init() {
	rootCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		hint := fmt.Sprintf("run %q for flags and examples", cmd.CommandPath()+" --help")
		if cmd == rootCmd && err.Error() == "unknown flag: --version" {
			hint = `print the installed version with "docbank version"`
		}
		return withHint(usageError(err), hint)
	})
	rootCmd.AddGroup(
		&cobra.Group{ID: groupDocuments, Title: "Documents:"},
		&cobra.Group{ID: groupSearch, Title: "Search and text:"},
		&cobra.Group{ID: groupSources, Title: "Email, media, photos, people:"},
		&cobra.Group{ID: groupProductions, Title: "Exports and productions:"},
		&cobra.Group{ID: groupOperations, Title: "Vault operations:"},
		&cobra.Group{ID: groupInterfaces, Title: "Interfaces:"},
	)
	// Keep root discovery compact without changing subcommand help. Cobra's
	// name padding includes hidden workers and wastes space in the grouped list.
	usage := rootCmd.UsageTemplate()
	compact := strings.ReplaceAll(usage, "{{rpad .Name .NamePadding }} ", "{{.Name}}  ")
	rootCmd.SetUsageTemplate(`{{if eq .CommandPath "docbank"}}` + compact + `{{else}}` + usage + `{{end}}`)
	// Keep the root marker active if a child later adds its own persistent hook.
	cobra.EnableTraverseRunHooks = true
	rootCmd.SetHelpCommandGroupID(groupOperations)
	rootCmd.SetCompletionCommandGroupID(groupOperations)
	rootCmd.AddCommand(programVersionCmd)
}
