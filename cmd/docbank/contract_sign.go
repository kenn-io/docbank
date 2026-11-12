package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newContractSignCommand() *cobra.Command {
	sign := &cobra.Command{Use: "sign", Short: "Connect to DocuSeal", Long: "Enable [integrations.docuseal] and restart the daemon before using these commands."}
	var checkJSON, templatesJSON bool
	var after int64
	check := &cobra.Command{Use: "check", Short: "Check the DocuSeal connection", Example: "  docbank contract sign check", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		status, err := c.DocuSealStatus(cmd.Context())
		if err != nil {
			return err
		}
		if checkJSON {
			return writeCLIJSON(cmd.OutOrStdout(), status)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Connected to %s (%s)\n", status.Endpoint, status.Capability)
		return err
	}}
	check.Flags().BoolVar(&checkJSON, "json", false, "print JSON")
	templates := &cobra.Command{Use: "templates", Short: "List DocuSeal templates", Example: "  docbank contract sign templates --json", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.DocuSealTemplates(cmd.Context(), after)
		if err != nil {
			return err
		}
		if templatesJSON {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		for _, item := range page.Templates {
			roles := make([]string, len(item.Roles))
			for i, role := range item.Roles {
				roles[i] = strconv.Quote(role)
			}
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\n", item.ID, item.Name, strings.Join(roles, ", ")); err != nil {
				return err
			}
		}
		if page.Next != 0 {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Continue with --after %d\n", page.Next)
		}
		return err
	}}
	templates.Flags().Int64Var(&after, "after", 0, "continue after this template ID")
	templates.Flags().BoolVar(&templatesJSON, "json", false, "print JSON")
	sign.AddCommand(check, templates)
	return sign
}
func init() { contractCmd.AddCommand(newContractSignCommand()) }
