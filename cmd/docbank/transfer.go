package main

import "github.com/spf13/cobra"

var transferCmd = &cobra.Command{
	Long: `Verify portable transfer packages offline before importing them through their
owning workflow. verify prints text or JSON findings for the local package.`,
	GroupID: groupProductions,
	Use:     "transfer",
	Short:   "Inspect portable transfer packages",
	Args:    cobra.NoArgs,
}

func init() {
	transferCmd.AddCommand(transferVerifyCmd)
	rootCmd.AddCommand(transferCmd)
}
