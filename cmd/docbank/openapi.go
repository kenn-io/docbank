package main

import (
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
)

var openapiCmd = &cobra.Command{
	Use:   "openapi",
	Short: "Print the HTTP API's OpenAPI YAML document (no daemon needed)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		out, err := api.OpenAPIYAML()
		if err != nil {
			return err
		}
		_, _ = cmd.OutOrStdout().Write(out)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(openapiCmd)
}
