package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const maxProductionPrivilegeRowsFileBytes = 64 << 20

var productionPrivilegeCmd = &cobra.Command{Use: "privilege-log", Short: "Edit, validate and freeze drafts, then read or export public privilege logs"}

func readProductionPrivilegeRowsFile(path string) ([]documentproduction.PrivilegeRow, error) {
	reader, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening privilege rows file: %w", err)
	}
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, maxProductionPrivilegeRowsFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading privilege rows file: %w", err)
	}
	if len(raw) > maxProductionPrivilegeRowsFileBytes {
		return nil, usageError(errors.New("privilege rows file exceeds 64 MiB"))
	}
	var rows []documentproduction.PrivilegeRow
	if err := json.Unmarshal(raw, &rows, json.RejectUnknownMembers(true)); err != nil || len(rows) == 0 ||
		len(rows) > documentproduction.MaxPrivilegeRows {
		return nil, usageError(errors.New("invalid privilege rows JSON"))
	}
	return rows, nil
}

func submitProductionPrivilegeDraft(cmd *cobra.Command, connection *daemonconn.Connection,
	logID string, revision int64, request api.ProductionPrivilegeDraftCreateRequest, asJSON bool) error {
	created, err := connection.CreateProductionPrivilegeLogDraft(cmd.Context(), logID, revision, request)
	if err != nil {
		return err
	}
	if asJSON {
		return writeCLIJSON(cmd.OutOrStdout(), created)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d generation=%d\n",
		created.LogID, created.Revision, created.Generation)
	if err != nil {
		return fmt.Errorf("writing privilege draft receipt: %w", err)
	}
	return nil
}

func newProductionPrivilegeDraftCommand() *cobra.Command {
	return newProductionPrivilegeDraftCommandWithEnsure(daemonconn.Ensure)
}

func newProductionPrivilegeDraftCommandWithEnsure(
	ensure func(context.Context) (*daemonconn.Connection, error)) *cobra.Command {
	var operationID, setID, playersSHA256, file string
	var predecessorLogID, predecessorReceiptSHA256 string
	var setRevision int64
	var asJSON bool
	cmd := &cobra.Command{Use: "draft <log-id> <revision>", Short: "Create a private privilege-log draft from sealed production authority",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 || operationID == "" || setID == "" || setRevision < 1 ||
				playersSHA256 == "" || file == "" {
				return usageError(errors.New("log revision, --operation-id, --set-id, --set-revision, --players-sha256, and --file are required"))
			}
			if (predecessorLogID == "") != (predecessorReceiptSHA256 == "") {
				return usageError(errors.New("--predecessor-log-id and --predecessor-receipt-sha256 must be supplied together"))
			}
			rows, err := readProductionPrivilegeRowsFile(file)
			if err != nil {
				return err
			}
			connection, err := ensure(cmd.Context())
			if err != nil {
				return err
			}
			return submitProductionPrivilegeDraft(cmd, connection, args[0], revision,
				api.ProductionPrivilegeDraftCreateRequest{
					OperationID: operationID, SetID: setID, SetRevision: setRevision,
					PlayersSHA256: playersSHA256, Rows: rows,
					PredecessorLogID:         predecessorLogID,
					PredecessorReceiptSHA256: predecessorReceiptSHA256,
				}, asJSON)
		}}
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for exact retries")
	cmd.Flags().StringVar(&setID, "set-id", "", "sealed production set UUID")
	cmd.Flags().Int64Var(&setRevision, "set-revision", 0, "sealed production revision")
	cmd.Flags().StringVar(&playersSHA256, "players-sha256", "", "stored player snapshot SHA-256")
	cmd.Flags().StringVar(&file, "file", "", "JSON file containing the complete private row array")
	cmd.Flags().StringVar(&predecessorLogID, "predecessor-log-id", "", "frozen predecessor log UUID for a correction")
	cmd.Flags().StringVar(&predecessorReceiptSHA256, "predecessor-receipt-sha256", "", "frozen predecessor receipt SHA-256")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable generation receipt JSON")
	return cmd
}

func newProductionPrivilegeRowsReplaceCommand() *cobra.Command {
	var operationID, file string
	var generation int64
	var asJSON bool
	cmd := &cobra.Command{Use: "replace <log-id> <revision>", Short: "Replace all private draft rows at an exact generation",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 || operationID == "" || generation < 1 || file == "" {
				return usageError(errors.New("log revision, --operation-id, --generation, and --file are required"))
			}
			rows, err := readProductionPrivilegeRowsFile(file)
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			replaced, err := connection.ReplaceProductionPrivilegeLogRows(cmd.Context(), args[0], revision,
				api.ProductionPrivilegeRowsReplaceRequest{
					OperationID: operationID, ExpectedGeneration: generation, Rows: rows,
				})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), replaced)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d generation=%d\n",
				replaced.LogID, replaced.Revision, replaced.Generation)
			if err != nil {
				return fmt.Errorf("writing privilege row receipt: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for exact retries")
	cmd.Flags().Int64Var(&generation, "generation", 0, "expected draft generation")
	cmd.Flags().StringVar(&file, "file", "", "JSON file containing the complete private row array")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable receipt JSON")
	return cmd
}

func newProductionPrivilegeExportCommand() *cobra.Command {
	var overwrite bool
	cmd := &cobra.Command{Use: "export <log-id> <revision> <format> <local-file>",
		Short: "Download a verified public JSON, CSV, XLSX, or PDF privilege log",
		Args:  cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 {
				return usageError(errors.New("revision must be a positive integer"))
			}
			destination, err := prepareGetDestination(args[3], overwrite)
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			exported, err := connection.ExportProductionPrivilegeLog(cmd.Context(), args[0], revision, args[2])
			if err != nil {
				return err
			}
			staging, err := makePrivateStagingDirAt(filepath.Dir(destination), "docbank-privilege-export-")
			if err != nil {
				return err
			}
			defer func() { retErr = errors.Join(retErr, staging.removeAll()) }()
			file, path, err := staging.createFile(filepath.Base(destination))
			if err != nil {
				return err
			}
			_, writeErr := file.Write(exported.Content)
			if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
				return err
			}
			if err := publishGetFile(path, destination, overwrite); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · SHA-256 %s · receipt %s\n",
				destination, exported.ContentSHA256, exported.ReceiptSHA256)
			if err != nil {
				return fmt.Errorf("writing privilege export receipt: %w", err)
			}
			return nil
		}}
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing destination")
	return cmd
}

func newProductionPrivilegeValidateCommand() *cobra.Command {
	var operationID string
	var generation int64
	var validatedAt string
	var asJSON bool
	cmd := &cobra.Command{Use: "validate <log-id> <revision>", Short: "Validate stored privilege-log draft rows",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 || operationID == "" || generation < 1 || validatedAt == "" {
				return usageError(errors.New("log revision, --operation-id, --generation, and --validated-at are required"))
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			result, err := c.ValidateProductionPrivilegeLog(cmd.Context(), args[0], revision,
				api.ProductionPrivilegeValidationRequest{OperationID: operationID,
					ExpectedGeneration: generation, ValidatedAt: validatedAt})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d generation=%d inputs_sha256=%s rows_sha256=%s\n",
				result.Validation.Inputs.LogID, result.Validation.Inputs.Revision,
				result.DraftGeneration, result.Validation.InputsSHA256, result.Validation.RowsSHA256)
			if err != nil {
				return fmt.Errorf("writing privilege validation: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for exact retries")
	cmd.Flags().Int64Var(&generation, "generation", 0, "expected draft generation")
	cmd.Flags().StringVar(&validatedAt, "validated-at", "", "canonical UTC validation timestamp")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newProductionPrivilegeFreezeCommand() *cobra.Command {
	var operationID, inputsSHA256, approvalEvaluationSHA256, frozenAt string
	var generation int64
	var asJSON bool
	cmd := &cobra.Command{Use: "freeze <log-id> <revision>", Short: "Freeze a validated stored privilege log after approval recheck",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 || operationID == "" || generation < 1 ||
				inputsSHA256 == "" || frozenAt == "" {
				return usageError(errors.New("log revision, --operation-id, --generation, --inputs-sha256, and --frozen-at are required"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.FreezeProductionPrivilegeLog(cmd.Context(), args[0], revision,
				api.ProductionPrivilegeFreezeRequest{
					OperationID: operationID, ExpectedGeneration: generation,
					ExpectedInputsSHA256:             inputsSHA256,
					ExpectedApprovalEvaluationSHA256: approvalEvaluationSHA256,
					FrozenAt:                         frozenAt,
				})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d sha256=%s rows=%d\n",
				receipt.LogID, receipt.Revision, receipt.SHA256, receipt.RowCount)
			if err != nil {
				return fmt.Errorf("writing privilege freeze receipt: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&operationID, "operation-id", "", "stable UUID for exact retries")
	cmd.Flags().Int64Var(&generation, "generation", 0, "expected validated draft generation")
	cmd.Flags().StringVar(&inputsSHA256, "inputs-sha256", "", "stored validation inputs SHA-256")
	cmd.Flags().StringVar(&approvalEvaluationSHA256, "approval-evaluation-sha256", "", "required approval evaluation SHA-256, when policy requires approval")
	cmd.Flags().StringVar(&frozenAt, "frozen-at", "", "canonical UTC freeze timestamp")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable receipt JSON")
	return cmd
}

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
	rowsCmd := &cobra.Command{Use: "rows", Short: "Edit private privilege draft rows"}
	rowsCmd.AddCommand(newProductionPrivilegeRowsReplaceCommand())
	productionPrivilegeCmd.AddCommand(newProductionPrivilegeDraftCommand())
	productionPrivilegeCmd.AddCommand(rowsCmd)
	productionPrivilegeCmd.AddCommand(newProductionPrivilegeValidateCommand())
	productionPrivilegeCmd.AddCommand(newProductionPrivilegeFreezeCommand())
	productionPrivilegeCmd.AddCommand(newProductionPrivilegeShowCommand())
	productionPrivilegeCmd.AddCommand(newProductionPrivilegeExportCommand())
	productionCmd.AddCommand(productionPrivilegeCmd)
}
