package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var productionNumbersCmd = &cobra.Command{Use: "numbers",
	Short: "Find verified published production numbers"}

func validProductionNumberText(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func writeProductionNumberReference(cmd *cobra.Command, item api.ProductionNumberReference) error {
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s job=%s source=%s artifact=%s volume=%s\n",
		item.Label, item.JobID, item.SourceVersionID, item.ArtifactID, item.Volume)
	if err != nil {
		return fmt.Errorf("writing production number: %w", err)
	}
	return nil
}

func writeProductionNumberPage(cmd *cobra.Command, page api.ProductionNumberPage, asJSON bool) error {
	if asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), page)
	}
	for _, item := range page.Items {
		if err := writeProductionNumberReference(cmd, item); err != nil {
			return err
		}
	}
	if page.NextSequence != 0 {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "next_sequence: %d\n", page.NextSequence)
		if err != nil {
			return fmt.Errorf("writing production number cursor: %w", err)
		}
	}
	return nil
}

func newProductionNumberFindCommand() *cobra.Command {
	return newProductionNumberFindCommandWithEnsure(daemonconn.Ensure)
}

func newProductionNumberFindCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "find <label>", Short: "Resolve one exact published production number",
		Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if !validProductionNumberText(args[0]) {
				return usageError(errors.New("label must be 1-256 trimmed UTF-8 bytes"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := connection.ProductionNumbers(cmd.Context(), daemonconn.ProductionNumberQuery{Label: args[0]})
			if err != nil {
				return err
			}
			return writeProductionNumberPage(cmd, page, asJSON)
		}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable reference JSON")
	return cmd
}

func newProductionNumberRangeCommand() *cobra.Command {
	return newProductionNumberRangeCommandWithEnsure(daemonconn.Ensure)
}

func newProductionNumberRangeCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var after int64
	var limit int
	var asJSON bool
	cmd := &cobra.Command{Use: "range <namespace-id> <start-sequence> <end-sequence>",
		Short: "Page published numbers in one namespace and numeric range", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			start, startErr := strconv.ParseInt(args[1], 10, 64)
			end, endErr := strconv.ParseInt(args[2], 10, 64)
			if !daemonconn.IsCanonicalUUIDv4(args[0]) || startErr != nil || endErr != nil ||
				start < 1 || end < start || after < 0 || after > end || limit < 1 || limit > 25 {
				return usageError(errors.New("namespace UUIDv4, valid positive range, and limit 1-25 are required"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := connection.ProductionNumbers(cmd.Context(), daemonconn.ProductionNumberQuery{
				NamespaceID: args[0], StartSequence: start, EndSequence: end,
				AfterSequence: after, Limit: limit,
			})
			if err != nil {
				return err
			}
			return writeProductionNumberPage(cmd, page, asJSON)
		}}
	cmd.Flags().Int64Var(&after, "after", 0, "continue after this sequence")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum references to return (1-25)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable reference JSON")
	return cmd
}

func newProductionNumberCandidatesCommand() *cobra.Command {
	return newProductionNumberCandidatesCommandWithEnsure(daemonconn.Ensure)
}

func newProductionNumberCandidatesCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var limit int
	var asJSON bool
	cmd := &cobra.Command{Use: "candidates <text>",
		Short: "Find bounded exact, prefix, or substring number candidates", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validProductionNumberText(args[0]) || limit < 1 || limit > 25 {
				return usageError(errors.New("candidate text must be 1-256 trimmed UTF-8 bytes and limit 1-25"))
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			result, err := connection.ProductionNumberCandidates(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "match=%s ambiguous=%t truncated=%t\n",
				result.MatchKind, result.Ambiguous, result.Truncated); err != nil {
				return fmt.Errorf("writing production number candidates: %w", err)
			}
			for _, item := range result.Items {
				if err := writeProductionNumberReference(cmd, item); err != nil {
					return err
				}
			}
			return nil
		}}
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum candidates to return (1-25)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable candidates JSON")
	return cmd
}

func init() {
	productionNumbersCmd.AddCommand(newProductionNumberFindCommand(),
		newProductionNumberRangeCommand(), newProductionNumberCandidatesCommand())
	productionCmd.AddCommand(productionNumbersCmd)
}
