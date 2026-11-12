package main

import "github.com/spf13/cobra"

var contractCmd = &cobra.Command{Use: "contract", Short: "Manage contracts", GroupID: groupDocuments}

func init() { rootCmd.AddCommand(contractCmd) }
