package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/docbank/internal/agentguide"
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
	Use:           "docbank",
	Short:         "Keep, find, and verify documents in a local vault",
	Long:          agentguide.Workflow,
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
		hint := fmt.Sprintf("run %q for flags and examples", canonicalCommandPath(cmd)+" --help")
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
