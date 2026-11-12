package main

import "github.com/spf13/cobra"

var contractCmd = &cobra.Command{Use: "contract", Short: "Manage contracts", Long: "Use the available subcommands to manage contracts or optional signing.", GroupID: groupDocuments}

func init() { rootCmd.AddCommand(contractCmd) }
