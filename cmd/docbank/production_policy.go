package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const maxProductionPolicyFileBytes = 1 << 20

var productionCmd = &cobra.Command{Use: "production", Short: "Manage verified production workflows"}
var productionPolicyCmd = &cobra.Command{Use: "policy", Short: "Manage immutable production policy versions"}

func newProductionPolicyCreateCommand() *cobra.Command {
	var file, operationID string
	var asJSON bool
	cmd := &cobra.Command{Use: "create", Short: "Save an immutable policy version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" || operationID == "" {
				return usageError(errors.New("--file and --operation-id are required"))
			}
			reader, err := os.Open(file)
			if err != nil {
				return fmt.Errorf("opening policy file: %w", err)
			}
			defer func() { _ = reader.Close() }()
			raw, err := io.ReadAll(io.LimitReader(reader, maxProductionPolicyFileBytes+1))
			if err != nil {
				return fmt.Errorf("reading policy file: %w", err)
			}
			if len(raw) > maxProductionPolicyFileBytes {
				return usageError(errors.New("policy file exceeds 1 MiB"))
			}
			var policy documentproduction.PolicyVersion
			if err := json.Unmarshal(raw, &policy, json.RejectUnknownMembers(true)); err != nil {
				return usageError(fmt.Errorf("invalid policy JSON: %w", err))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			created, err := c.CreateProductionPolicyVersion(cmd.Context(), api.ProductionPolicyCreateRequest{
				OperationID: operationID, Policy: policy})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), created)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s version=%d sha256=%s\n",
				created.ID, created.Version, created.SHA256)
			if err != nil {
				return fmt.Errorf("writing created policy: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&file, "file", "", "JSON file containing one policy version")
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for replay-safe creation")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newProductionPolicyListCommand() *cobra.Command {
	var cursor string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{Use: "list", Short: "List bounded policy versions", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > store.MaxProductionPolicyPage || len(cursor) > 60 {
				return usageError(errors.New("--limit must be 1-100 and --cursor at most 60 bytes"))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := c.ProductionPolicyVersions(cmd.Context(), cursor, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			for _, item := range page.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s version=%d %s\n",
					item.ID, item.Version, item.Name); err != nil {
					return fmt.Errorf("writing policy list: %w", err)
				}
			}
			if page.NextCursor != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
				if err != nil {
					return fmt.Errorf("writing policy cursor: %w", err)
				}
			}
			return nil
		}}
	cmd.Flags().StringVar(&cursor, "cursor", "", "continue after this policy cursor")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum versions to list (1-100)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newProductionPolicyShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <policy-id> <version>", Short: "Read an exact policy version", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || version < 1 {
				return usageError(errors.New("version must be a positive integer"))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			policy, err := c.ProductionPolicyVersion(cmd.Context(), args[0], version)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), policy)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s version=%d sha256=%s %s\n",
				policy.ID, policy.Version, policy.SHA256, policy.Name)
			if err != nil {
				return fmt.Errorf("writing policy: %w", err)
			}
			return nil
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func init() {
	productionPolicyCmd.AddCommand(newProductionPolicyCreateCommand(),
		newProductionPolicyListCommand(), newProductionPolicyShowCommand())
	productionCmd.AddCommand(productionPolicyCmd)
	rootCmd.AddCommand(productionCmd)
}
