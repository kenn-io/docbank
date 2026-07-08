package cmd

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/update"
)

var (
	updateCheck bool
	updateYes   bool
	updateForce bool
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update docbank to the latest GitHub release",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return update.Run(cmd.Context(), cmd.OutOrStdout(), update.Options{
			CheckOnly: updateCheck,
			Yes:       updateYes,
			Force:     updateForce,
			Confirm: func(prompt string) (bool, error) {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil {
					return false, fmt.Errorf("reading confirmation: %w", err)
				}
				return strings.EqualFold(strings.TrimSpace(line), "y"), nil
			},
		})
	},
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheck, "check", false, "check only; do not install")
	updateCmd.Flags().BoolVar(&updateYes, "yes", false, "install without confirmation")
	updateCmd.Flags().BoolVar(&updateForce, "force", false, "install even if up to date or a dev build")
	rootCmd.AddCommand(updateCmd)
}
