package main

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	doctui "go.kenn.io/docbank/internal/tui"
)

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Browse and search the vault interactively",
	Long: `Open a read-only terminal interface backed by the authenticated daemon API.

Navigation:
  Up/Down or j/k       Move between documents
  Enter or Right       Open a directory
  Left or Backspace    Return to the parent directory
  /                    Search names and extracted text
  r                    Refresh the current view
  ?                    Show keyboard help
  q                    Quit

The initial TUI is deliberately read-only. Use the ordinary CLI or HTTP API for
mutations, storage maintenance, backup, and permanent-audit enrollment.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		model, err := doctui.New(cmd.Context(), c)
		if err != nil {
			return err
		}
		if _, err := tea.NewProgram(model).Run(); err != nil {
			return fmt.Errorf("running docbank TUI: %w", err)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(tuiCmd)
}
