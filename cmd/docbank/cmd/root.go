package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Set via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
)

var rootCmd = &cobra.Command{
	Use:           "docbank",
	Short:         "Personal document archive with a virtual tree over content-addressed storage",
	Version:       fmt.Sprintf("%s (%s)", Version, Commit),
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() error {
	return rootCmd.Execute()
}
