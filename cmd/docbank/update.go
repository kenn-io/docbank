package main

import (
	"github.com/spf13/cobra"
)

var (
	updateCheck bool
	updateYes   bool
	updateForce bool
)

var updateCmd = &cobra.Command{
	Example: `  docbank update --check`,
	GroupID: groupOperations,
	Use:     "update",
	Short:   "Update docbank to the latest GitHub release",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runUpdate(cmd.Context(), cmd.OutOrStdout(), updateOptions{
			CheckOnly: updateCheck,
			Yes:       updateYes,
			Force:     updateForce,
			Confirm: func(prompt string) (bool, error) {
				return confirmPrompt(cmd, cmd.OutOrStdout(), prompt)
			},
		})
	},
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheck, "check", false, "check only; do not install")
	updateCmd.Flags().BoolVar(&updateYes, "yes", false, "install without confirmation")
	updateCmd.Flags().BoolVar(&updateForce, "force", false, "refresh update metadata and allow replacing dev builds")
	rootCmd.AddCommand(updateCmd)
}
