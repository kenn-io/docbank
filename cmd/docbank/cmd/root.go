package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/version"
)

var rootCmd = &cobra.Command{
	Use:           "docbank",
	Short:         "Personal document archive with a virtual tree over content-addressed storage",
	Version:       fmt.Sprintf("%s (%s)", version.Version, version.Commit),
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() error {
	return rootCmd.Execute() //nolint:wrapcheck // error is user-facing CLI output; wrapping adds noise
}
