package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func newProductionDraftSealCommand() *cobra.Command {
	var memberHash, operationID string
	var etag int64
	var total int
	var asJSON bool
	command := &cobra.Command{Use: "seal <set-id> <revision>", Short: "Seal exact production draft membership", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			request := api.ProductionMembershipSealRequest{OperationID: effectiveOperationID,
				Total: total, MemberHash: memberHash}
			if err := redaction.ValidateMembershipSealRequest(request.Domain(etag)); err != nil {
				return usageError(err)
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.SealProductionMembership(cmd.Context(), args[0], revision, etag, request)
			if err != nil {
				return fmt.Errorf("sealing production membership (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d operation_id=%s\n",
				receipt.SetID, receipt.Revision, receipt.ETag, receipt.OperationID)
			if err != nil {
				return fmt.Errorf("writing production seal receipt: %w", err)
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "current draft ETag required for revision fencing")
	command.Flags().IntVar(&total, "total", 0, "exact number of members in this revision")
	command.Flags().StringVar(&memberHash, "member-hash", "", "exact member hash from the draft")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionDraftResolveCommand() *cobra.Command {
	var cursor string
	var etag int64
	var limit int
	var asJSON bool
	command := &cobra.Command{Use: "resolve <set-id> <revision> <member-id> <page>",
		Short: "Read one exact resolved mask page and its review binding", Args: cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			if limit < 1 || limit > redaction.MaxProductionPage || len(cursor) > 1024 {
				return usageError(errors.New("--limit must be 1-200 and --cursor at most 1024 bytes"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			page, err := strconv.Atoi(args[3])
			if err != nil || page < 1 {
				return usageError(errors.New("page must be a positive integer"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			resolved, err := connection.ResolveProductionSelection(cmd.Context(), args[0], revision, etag,
				api.ProductionResolveRequest{MemberID: args[2], Page: page, Cursor: cursor, Limit: limit})
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), resolved)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s page=%d boxes=%d review_binding=%s\n",
				resolved.MemberID, resolved.Page.Number, resolved.TotalBoxes, resolved.ReviewBinding); err != nil {
				return fmt.Errorf("writing resolved mask: %w", err)
			}
			for _, box := range resolved.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d,%d,%d,%d\n", box.X0, box.Y0, box.X1, box.Y1); err != nil {
					return fmt.Errorf("writing resolved mask boxes: %w", err)
				}
			}
			if resolved.NextCursor != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", resolved.NextCursor); err != nil {
					return fmt.Errorf("writing resolved mask cursor: %w", err)
				}
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "current draft ETag required for revision fencing")
	command.Flags().StringVar(&cursor, "cursor", "", "continue after a resolved mask page cursor")
	command.Flags().IntVar(&limit, "limit", 100, "maximum boxes to list (1-200)")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionDraftReviewCommand() *cobra.Command {
	var binding, operationID string
	var etag int64
	var asJSON bool
	command := &cobra.Command{Use: "review <set-id> <revision> <member-id>",
		Short: "Declare review of one exact resolved member plan", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			if !canonical.IsSHA256Hex(binding) {
				return usageError(errors.New("--binding must be a SHA-256 hex digest from resolve"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.ReviewProductionMember(cmd.Context(), args[0], revision, etag,
				args[2], api.ProductionMemberReviewRequest{OperationID: effectiveOperationID,
					Binding: binding, Complete: true})
			if err != nil {
				return fmt.Errorf("reviewing production member (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d operation_id=%s\n",
				receipt.SetID, receipt.Revision, receipt.ETag, receipt.OperationID)
			if err != nil {
				return fmt.Errorf("writing production review receipt: %w", err)
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "current draft ETag required for revision fencing")
	command.Flags().StringVar(&binding, "binding", "", "review binding from the exact resolved plan")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}
