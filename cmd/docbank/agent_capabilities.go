package main

import (
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

func init() {
	var asJSON bool
	root := &cobra.Command{Use: "agent", Short: "Inspect available daemon agent operations"}
	read := &cobra.Command{
		Use: "capabilities", Short: "Read live, credential-scoped agent capabilities",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			capabilities, err := connection.AgentCapabilities(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				if err := json.MarshalWrite(cmd.OutOrStdout(), capabilities); err != nil {
					return fmt.Errorf("write agent capabilities: %w", err)
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout())
				if err != nil {
					return fmt.Errorf("write agent capabilities newline: %w", err)
				}
				return nil
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s | %d available operation(s)\n",
				capabilities.Schema, len(capabilities.Server.Operations)); err != nil {
				return fmt.Errorf("write agent capabilities summary: %w", err)
			}
			for _, operation := range capabilities.Server.Operations {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), operation.ID); err != nil {
					return fmt.Errorf("write agent operation: %w", err)
				}
			}
			return nil
		},
	}
	read.Flags().BoolVar(&asJSON, "json", false, "Print the complete capability response as JSON")
	root.AddCommand(read)
	rootCmd.AddCommand(root)
}
