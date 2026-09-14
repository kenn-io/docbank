package main

import "github.com/spf13/cobra"

var transferCmd = &cobra.Command{
	Use:   "transfer",
	Short: "Inspect portable transfer packages",
	Args:  cobra.NoArgs,
}

func init() {
	transferCmd.AddCommand(transferVerifyCmd)
	rootCmd.AddCommand(transferCmd)
}
