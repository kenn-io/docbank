package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func productionDraftRevision(raw string) (int64, error) {
	revision, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || revision < 1 {
		return 0, usageError(errors.New("revision must be a positive integer"))
	}
	return revision, nil
}

func newProductionDraftMembersCommand() *cobra.Command {
	var cursor string
	var limit int
	var asJSON bool
	command := &cobra.Command{Use: "members <set-id> <revision>", Short: "List one bounded production member page", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > redaction.MaxProductionPage || len(cursor) > 4096 {
				return usageError(errors.New("--limit must be 1-200 and --cursor at most 4096 bytes"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := connection.ProductionMembers(cmd.Context(), args[0], revision, cursor, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			for _, member := range page.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s ordinal=%d mode=%s source_version=%s\n",
					member.ID, member.Ordinal, member.Mode, member.SourceVersionID); err != nil {
					return fmt.Errorf("writing production members: %w", err)
				}
			}
			if page.NextCursor != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor); err != nil {
					return fmt.Errorf("writing production member cursor: %w", err)
				}
			}
			return nil
		}}
	command.Flags().StringVar(&cursor, "cursor", "", "continue after a production member page cursor")
	command.Flags().IntVar(&limit, "limit", 100, "maximum members to list (1-200)")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionDraftDecisionsCommand() *cobra.Command {
	var cursor, uncertain string
	var limit int
	var asJSON bool
	command := &cobra.Command{Use: "decisions <set-id> <revision>", Short: "List one bounded production decision page", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > redaction.MaxProductionDecisionPage || len(cursor) > 2048 {
				return usageError(errors.New("--limit must be 1-500 and --cursor at most 2048 bytes"))
			}
			var filter *bool
			switch uncertain {
			case "all":
			case "true":
				value := true
				filter = &value
			case "false":
				value := false
				filter = &value
			default:
				return usageError(errors.New("--uncertain must be all, true, or false"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := connection.ProductionDecisionsFiltered(cmd.Context(), args[0], revision, cursor, limit, filter)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			for _, decision := range page.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s member=%s action=%s uncertain=%t\n",
					decision.ID, decision.MemberID, decision.Action, decision.Uncertain); err != nil {
					return fmt.Errorf("writing production decisions: %w", err)
				}
			}
			if page.NextCursor != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor); err != nil {
					return fmt.Errorf("writing production decision cursor: %w", err)
				}
			}
			return nil
		}}
	command.Flags().StringVar(&cursor, "cursor", "", "continue after a production decision page cursor")
	command.Flags().IntVar(&limit, "limit", 100, "maximum decisions to list (1-500)")
	command.Flags().StringVar(&uncertain, "uncertain", "all", "show all, uncertain, or settled decisions")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionDraftInstructionsCommand() *cobra.Command {
	var instructionsFile, operationID string
	var etag int64
	var asJSON bool
	command := &cobra.Command{Use: "instructions <set-id> <revision>", Short: "Replace revision-bound production instructions", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			if instructionsFile == "" {
				return usageError(errors.New("--instructions-file is required"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			instructions, err := readProductionInstructions(cmd, instructionsFile)
			if err != nil {
				return err
			}
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			request := api.ProductionInstructionsRequest{OperationID: effectiveOperationID,
				Instructions: instructions}
			if err := redaction.ValidateInstructionsEditRequest(request.Domain(etag)); err != nil {
				return usageError(err)
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.EditProductionInstructions(cmd.Context(), args[0], revision, etag, request)
			if err != nil {
				return fmt.Errorf("editing production instructions (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d operation_id=%s\n",
				receipt.SetID, receipt.Revision, receipt.ETag, receipt.OperationID)
			if err != nil {
				return fmt.Errorf("writing production instruction receipt: %w", err)
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "current draft ETag required for revision fencing")
	command.Flags().StringVar(&instructionsFile, "instructions-file", "", "UTF-8 instructions file; use - for stdin")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}
