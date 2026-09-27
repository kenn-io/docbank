package main

import (
	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/pdfstamp"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{Use: "internal-pdfstamp-worker", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return pdfstamp.RunWorker(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		}})
}
