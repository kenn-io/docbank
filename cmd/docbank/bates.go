package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/pdfstamp"
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

func newBatesPlanCommand() *cobra.Command {
	var namespace, position, recipeOut string
	var startAt int64
	var margin int
	var asJSON bool
	cmd := &cobra.Command{Use: "plan <snapshot-id>", Short: "Preview Bates labels without reserving them",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if namespace == "" {
				return usageError(errors.New("--namespace is required"))
			}
			if _, err := uuid.Parse(namespace); err != nil {
				return usageError(fmt.Errorf("--namespace must be a UUID: %w", err))
			}
			if _, err := uuid.Parse(args[0]); err != nil {
				return usageError(fmt.Errorf("snapshot ID must be a UUID: %w", err))
			}
			if startAt < 0 {
				return usageError(errors.New("--start-at must be nonnegative"))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			request := api.BatesPlanRequest{NamespaceID: namespace, SnapshotID: args[0], StartAt: startAt}
			plan, err := c.API().PlanBatesStamp(cmd.Context(), &apiclient.PlanBatesStampRequestOptions{Body: &request})
			if err != nil {
				return err
			}
			if recipeOut != "" {
				recipe := batesRecipeForPlan(*plan, position, margin)
				if err := writeBatesRecipe(recipeOut, recipe); err != nil {
					return err
				}
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
	cmd.Flags().Int64Var(&startAt, "start-at", 0, "first Bates number; zero continues the namespace cursor")
	cmd.Flags().StringVar(&recipeOut, "recipe-out", "", "write the stamp recipe for this preview to a new file")
	cmd.Flags().StringVar(&position, "position", "bottom-right", "stamp position written to --recipe-out")
	cmd.Flags().IntVar(&margin, "margin", 24, "stamp margin in points written to --recipe-out")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// batesRecipeForPlan fixes the previewed first number in the recipe, so a
// reservation made from it either gets exactly that range or fails.
func batesRecipeForPlan(plan api.BatesPlan, position string, margin int) pdfstamp.Recipe {
	return pdfstamp.Recipe{Contract: pdfstamp.RecipeContractV1, NamespaceID: plan.Namespace.NamespaceID,
		Prefix: plan.Namespace.Prefix, Suffix: plan.Namespace.Suffix, Padding: plan.Namespace.Padding,
		StartAt: int(plan.StartSequence), Position: position, MarginPoints: margin, FontName: "Helvetica",
		FontSizePoints: 9, Color: "#000000", Opacity: 1, Units: "point", RotationPolicy: "follow_page",
		EngineIdentity: pdfstamp.EngineIdentity{Name: "pdfcpu", Version: "v0.15.0", API: "AddWatermarksMap",
			Options: []string{"onTop=true", "update=restamp"}}}
}

func writeBatesRecipe(path string, recipe pdfstamp.Recipe) error {
	if err := recipe.Validate(); err != nil {
		return usageError(err)
	}
	encoded, err := json.Marshal(recipe, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("encoding Bates stamp recipe: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating Bates stamp recipe %s: %w", path, err)
	}
	_, writeErr := file.Write(append(encoded, '\n'))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("writing Bates stamp recipe %s: %w", path, err)
	}
	return nil
}

func readBatesRecipe(path string) (pdfstamp.Recipe, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pdfstamp.Recipe{}, fmt.Errorf("reading Bates stamp recipe: %w", err)
	}
	var recipe pdfstamp.Recipe
	if err := json.Unmarshal(raw, &recipe, json.RejectUnknownMembers(true)); err != nil {
		return pdfstamp.Recipe{}, usageError(fmt.Errorf("decoding Bates stamp recipe: %w", err))
	}
	if err := recipe.Validate(); err != nil {
		return pdfstamp.Recipe{}, usageError(err)
	}
	return recipe, nil
}

func newBatesReserveCommand() *cobra.Command {
	var recipePath, operation string
	var asJSON bool
	cmd := &cobra.Command{Use: "reserve <snapshot-id>", Short: "Reserve the Bates labels named by a stamp recipe",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if recipePath == "" {
				return usageError(errors.New("--recipe is required; create one with bates plan --recipe-out"))
			}
			if _, err := uuid.Parse(args[0]); err != nil {
				return usageError(fmt.Errorf("snapshot ID must be a UUID: %w", err))
			}
			recipe, err := readBatesRecipe(recipePath)
			if err != nil {
				return err
			}
			operationID := operation
			if operationID == "" {
				operationID = uuid.NewV4().String()
				// Printed before the request so a retry after a lost response
				// can reuse the same idempotency key instead of burning a range.
				_, err = fmt.Fprintf(cmd.ErrOrStderr(), "operation ID %s (pass --operation-id %s to retry)\n",
					operationID, operationID)
				if err != nil {
					return fmt.Errorf("writing Bates operation ID: %w", err)
				}
			}
			if _, err := uuid.Parse(operationID); err != nil {
				return usageError(fmt.Errorf("--operation-id must be a UUID: %w", err))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			request := api.BatesReserveRequest{OperationID: operationID, SnapshotID: args[0], Recipe: recipe}
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
		}}
	cmd.Flags().StringVar(&recipePath, "recipe", "", "stamp recipe JSON from bates plan --recipe-out")
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
	batesCmd.AddCommand(newBatesNamespacesCommand(), newBatesPlanCommand(), newBatesReserveCommand(), newBatesShowCommand(), newBatesExportCommand())
	rootCmd.AddCommand(batesCmd)
}
