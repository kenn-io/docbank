package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
)

var openapiJSON bool

var openapiCmd = &cobra.Command{
	Use:   "openapi",
	Short: "Print the HTTP API's OpenAPI document (no daemon needed)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if openapiJSON {
			out, err := api.NewOfflineServer().API().OpenAPI().MarshalJSON()
			if err != nil {
				return fmt.Errorf("rendering OpenAPI document: %w", err)
			}
			_, _ = cmd.OutOrStdout().Write(out)
			return nil
		}
		out, err := api.OpenAPIYAML()
		if err != nil {
			return err
		}
		_, _ = cmd.OutOrStdout().Write(out)
		return nil
	},
}

func init() {
	openapiCmd.Flags().BoolVar(&openapiJSON, "json", false, "emit JSON instead of YAML")
	rootCmd.AddCommand(openapiCmd)
}
