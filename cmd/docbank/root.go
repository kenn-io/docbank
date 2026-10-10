package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/version"
)

const (
	groupDocuments   = "docs"
	groupSearch      = "find"
	groupSources     = "sources"
	groupProductions = "legal"
	groupOperations  = "ops"
	groupInterfaces  = "ui"
)

var rootCmd = &cobra.Command{
	Use:   "docbank",
	Short: "Keep, find, and verify documents in a local vault",
	Long: `Docbank keeps documents in a local vault: a virtual folder tree (/cases/a.pdf)
over immutable, hash-verified content versions. A background daemon owns the
vault; data commands start it automatically ("docbank daemon stop" ends it).

DOCBANK_HOME selects the vault (default ~/.docbank); the first data command
creates it. "docbank info" confirms the selected vault's path and identity.

Start here:
  docbank add ./scans --dest /cases/acme   import files/folders (creates dirs)
  docbank tree /cases                      browse; shows id:N selectors
  docbank search invoice 2026 --json       find by name or text content
  docbank cat /cases/acme/notes.txt        print a file's original bytes
  docbank get id:12 ./invoice.pdf          save a verified local copy
  docbank backup init --repo ~/backup      then: backup create --repo ~/backup

Concepts:
  path-or-id  a node is "/abs/path" or "id:N"; id:N survives mv and rename
  version     put/edit add an immutable version (UUID); revert restores one
  text        search reads text/*, JSON and email bodies; PDFs and Office
              files need a processing profile (see "docbank processing")
  rendition   derived Markdown text made by a processing profile
  deletion    rm -> trash empty -> gc -> storage repack; each step is explicit

Most read commands accept --json. Exit codes: 0 ok, 1 error, 2 usage,
3 not found, 4 stale revision, 5 busy/locked, 6 integrity failure.`,
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
	Args:    cobra.NoArgs,
	Run: func(cmd *cobra.Command, _ []string) {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "docbank version %s (%s)\n",
			version.Version, version.Commit)
	},
}

var commandStarted bool

func Execute() error {
	commandStarted = false
	return rootCmd.Execute() //nolint:wrapcheck // error is user-facing CLI output; wrapping adds noise
}

func init() {
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
	rootCmd.AddCommand(programVersionCmd)
}
