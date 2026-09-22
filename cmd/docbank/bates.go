package main

import (
	"errors"
	"fmt"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
)

var batesCmd = &cobra.Command{Use: "bates", Short: "Plan and reserve Bates labels for exported PDFs"}

func newBatesNamespacesCommand() *cobra.Command {
	var create, asJSON bool
	var prefix, suffix, cursor string
	var padding, limit int
	cmd := &cobra.Command{Use: "namespaces", Short: "List or create a Bates namespace", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if create {
				if padding < 1 || padding > 10 {
					return usageError(errors.New("--padding must be between 1 and 10 when creating a namespace"))
				}
			} else {
				if prefix != "" || suffix != "" || padding != 0 {
					return usageError(errors.New("--prefix, --suffix and --padding require --create"))
				}
				if limit < 1 || limit > 250 {
					return usageError(errors.New("--limit must be between 1 and 250"))
				}
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if create {
				result, err := c.API().CreateBatesNamespace(cmd.Context(), &apiclient.CreateBatesNamespaceRequestOptions{
					Body: &api.BatesNamespaceRequest{Prefix: prefix, Suffix: suffix, Padding: padding}})
				if err != nil {
					return err
				}
				if asJSON {
					return writeCLIJSON(cmd.OutOrStdout(), result)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s padding=%d\n", result.NamespaceID, result.Prefix, result.Padding)
				if err != nil {
					return fmt.Errorf("writing Bates namespace: %w", err)
				}
				return nil
			}
			pageLimit := int64(limit)
			page, err := c.API().ListBatesNamespaces(cmd.Context(), &apiclient.ListBatesNamespacesRequestOptions{
				Query: &apiclient.ListBatesNamespacesQuery{Cursor: &cursor, Limit: &pageLimit}})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			for _, item := range page.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s padding=%d\n", item.NamespaceID, item.Prefix, item.Padding); err != nil {
					return fmt.Errorf("writing Bates namespaces: %w", err)
				}
			}
			if page.NextCursor != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
				if err != nil {
					return fmt.Errorf("writing Bates namespace cursor: %w", err)
				}
			}
			return nil
		}}
	cmd.Flags().BoolVar(&create, "create", false, "create or find the exact prefix and suffix namespace")
	cmd.Flags().StringVar(&prefix, "prefix", "", "Bates label prefix")
	cmd.Flags().StringVar(&suffix, "suffix", "", "Bates label suffix")
	cmd.Flags().IntVar(&padding, "padding", 0, "digits in each Bates number")
	cmd.Flags().StringVar(&cursor, "cursor", "", "continue listing namespaces after this cursor")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum namespaces to list (1-250)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newBatesPlanCommand(reserve bool) *cobra.Command {
	var namespace, recipe, operation string
	var startAt int64
	var asJSON bool
	name := "plan"
	short := "Preview Bates labels without reserving them"
	if reserve {
		name, short = "reserve", "Reserve Bates labels for exported PDFs"
	}
	cmd := &cobra.Command{Use: name + " <snapshot-id>", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if namespace == "" {
				return usageError(errors.New("--namespace is required"))
			}
			if recipe == "" {
				return usageError(errors.New("--recipe-sha256 is required"))
			}
			if !canonical.IsSHA256Hex(recipe) {
				return usageError(errors.New("--recipe-sha256 must be a lowercase SHA-256 digest"))
			}
			if startAt < 0 {
				return usageError(errors.New("--start-at must be nonnegative"))
			}
			if _, err := uuid.Parse(namespace); err != nil {
				return usageError(fmt.Errorf("--namespace must be a UUID: %w", err))
			}
			if _, err := uuid.Parse(args[0]); err != nil {
				return usageError(fmt.Errorf("snapshot ID must be a UUID: %w", err))
			}
			operationID := operation
			if operationID == "" {
				operationID = uuid.NewV4().String()
			}
			if _, err := uuid.Parse(operationID); err != nil {
				return usageError(fmt.Errorf("--operation-id must be a UUID: %w", err))
			}
			request := api.BatesPlanRequest{OperationID: operationID, NamespaceID: namespace,
				SnapshotID: args[0], RecipeSHA256: recipe, StartAt: startAt}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if reserve {
				allocation, err := c.API().ReserveBatesRange(cmd.Context(), &apiclient.ReserveBatesRangeRequestOptions{Body: &request})
				if err != nil {
					return err
				}
				if asJSON {
					return writeCLIJSON(cmd.OutOrStdout(), allocation)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "reserved %s: %d-%d (%d pages)\n", allocation.AllocationID,
					allocation.StartSequence, allocation.EndSequence, len(allocation.Labels))
				if err != nil {
					return fmt.Errorf("writing Bates reservation: %w", err)
				}
				return nil
			}
			plan, err := c.API().PlanBatesStamp(cmd.Context(), &apiclient.PlanBatesStampRequestOptions{Body: &request})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), plan)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "tentative %d-%d (%d pages); nothing stamped or reserved\n",
				plan.StartSequence, plan.EndSequence, len(plan.Labels))
			if err != nil {
				return fmt.Errorf("writing Bates preview: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&namespace, "namespace", "", "Bates namespace ID for the exported PDFs")
	cmd.Flags().StringVar(&recipe, "recipe-sha256", "", "stamp recipe SHA-256; add a Bates stamp to the exported PDFs")
	cmd.Flags().Int64Var(&startAt, "start-at", 0, "first Bates number; zero continues the namespace cursor")
	cmd.Flags().StringVar(&operation, "operation-id", "", "idempotency UUID for a reservation; generated when omitted")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newBatesShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show <allocation-id>", Short: "Show a reserved Bates allocation", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[0])
			if err != nil {
				return usageError(fmt.Errorf("allocation ID must be a UUID: %w", err))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			allocation, err := c.API().ReadBatesAllocation(cmd.Context(), &apiclient.ReadBatesAllocationRequestOptions{
				PathParams: &apiclient.ReadBatesAllocationPath{ID: id}})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), allocation)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s %d-%d (%d pages)\n", allocation.AllocationID,
				allocation.State, allocation.StartSequence, allocation.EndSequence, len(allocation.Labels))
			if err != nil {
				return fmt.Errorf("writing Bates allocation: %w", err)
			}
			return nil
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func init() {
	batesCmd.AddCommand(newBatesNamespacesCommand(), newBatesPlanCommand(false), newBatesPlanCommand(true), newBatesShowCommand(), newBatesExportCommand())
	rootCmd.AddCommand(batesCmd)
}
