package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/client"
)

func init() {
	command := &cobra.Command{
		Use: "email-documents", Short: "Inspect and release email attachment relationships",
	}
	show := &cobra.Command{
		Use: "show <operation-id>", Short: "Read a publication receipt as JSON",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := document.ValidateEmailDocumentOperationID(args[0]); err != nil {
				return usageError(err)
			}
			c, err := client.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := c.EmailDocumentPublication(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), receipt)
		},
	}
	var query document.EmailDocumentRelationQuery
	relations := &cobra.Command{
		Use: "relations", Short: "Read one page of exact parent or child relationships as JSON",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := document.NormalizeEmailDocumentRelationQuery(query); err != nil {
				return usageError(err)
			}
			c, err := client.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := c.EmailDocumentRelations(cmd.Context(), query)
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), page)
		},
	}
	relations.Flags().StringVar(&query.ParentVersionID, "parent-version", "", "select an exact parent version")
	relations.Flags().StringVar(&query.ChildVersionID, "child-version", "", "select an exact child version")
	relations.Flags().IntVar(&query.Limit, "limit", 100, "maximum relationships to return (1-250)")
	relations.Flags().StringVar(&query.AfterOperationID, "after-operation", "", "next_operation_id from a previous page")
	relations.Flags().IntVar(&query.AfterOrder, "after-order", 0, "next_order from a previous page")
	var digest string
	release := &cobra.Command{
		Use: "release <operation-id>", Short: "Remove a receipt and its relationships, keeping the child documents",
		Long: "Remove a receipt and its relationships, keeping the child documents. " +
			"This releases deletion and purge blockers and relinquishes the operation's retry guarantee. " +
			"Use show to inspect the receipt and copy its request_digest before releasing it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := document.ValidateEmailDocumentOperationID(args[0]); err != nil {
				return usageError(err)
			}
			c, err := client.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if err := c.RemoveEmailDocumentPublication(cmd.Context(), args[0], digest); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "released email document publication %s; child documents remain\n", args[0])
			if err != nil {
				return fmt.Errorf("writing receipt release: %w", err)
			}
			return nil
		},
	}
	release.Flags().StringVar(&digest, "request-digest", "", "exact request_digest from the receipt")
	_ = release.MarkFlagRequired("request-digest")
	command.AddCommand(show, relations, release)
	rootCmd.AddCommand(command)
}
