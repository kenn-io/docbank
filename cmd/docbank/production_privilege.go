package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var productionPrivilegeCmd = &cobra.Command{Use: "privilege-log", Short: "Read frozen public privilege logs"}

func newProductionPrivilegeShowCommand() *cobra.Command {
	var cursor string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{Use: "show <log-id> <revision>", Short: "Page public rows from an exact frozen log",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 {
				return usageError(errors.New("revision must be a positive integer"))
			}
			if limit < 1 || limit > store.MaxProductionPrivilegePage || len(cursor) > 6 {
				return usageError(errors.New("--limit must be 1-100 and --cursor at most 6 bytes"))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := c.ProductionPrivilegeLog(cmd.Context(), args[0], revision, cursor, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d sha256=%s\n",
				page.Receipt.LogID, page.Receipt.Revision, page.Receipt.SHA256); err != nil {
				return fmt.Errorf("writing privilege receipt: %w", err)
			}
			for _, row := range page.Rows {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", row.ID, row.Basis,
					row.PublicDescription); err != nil {
					return fmt.Errorf("writing privilege row: %w", err)
				}
			}
			if page.NextCursor != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor); err != nil {
					return fmt.Errorf("writing privilege cursor: %w", err)
				}
			}
			return nil
		}}
	cmd.Flags().StringVar(&cursor, "cursor", "", "continue after this row cursor")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum public rows to list (1-100)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func init() {
	productionPrivilegeCmd.AddCommand(newProductionPrivilegeShowCommand())
	productionCmd.AddCommand(productionPrivilegeCmd)
}
