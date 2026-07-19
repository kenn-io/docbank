package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/version"
)

var rootCmd = &cobra.Command{
	Use:           "docbank",
	Short:         "Personal document archive with a virtual tree over content-addressed storage",
	SilenceUsage:  true,
	SilenceErrors: true,
	// Cobra runs this only after flag parsing and positional validation, which
	// lets the process boundary distinguish command syntax from RunE failures.
	PersistentPreRun: func(_ *cobra.Command, _ []string) {
		commandStarted = true
	},
}

var programVersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the installed Docbank version",
	Args:  cobra.NoArgs,
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
	// Keep the root marker active if a child later adds its own persistent hook.
	cobra.EnableTraverseRunHooks = true
	rootCmd.AddCommand(programVersionCmd)
}
