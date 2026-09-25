package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func newProductionJobAdmitCommand() *cobra.Command {
	var jobID, operationID string
	var etag int64
	var asJSON bool
	command := &cobra.Command{Use: "admit <set-id> <revision>", Short: "Submit a finalized revision for production", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			effectiveJobID := jobID
			if effectiveJobID == "" {
				effectiveJobID = uuid.NewV4().String()
			}
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			status, err := connection.AdmitProductionJob(cmd.Context(), args[0], revision, etag,
				api.ProductionJobAdmissionRequest{JobID: effectiveJobID, OperationID: effectiveOperationID})
			if err != nil {
				return fmt.Errorf("admitting production job (job %s, operation %s): %w",
					effectiveJobID, effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), status)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d state=%s operation_id=%s\n",
				status.JobID, status.Revision, status.State, effectiveOperationID)
			if err != nil {
				return fmt.Errorf("writing production admission: %w", err)
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "finalized draft ETag required for revision fencing")
	command.Flags().StringVar(&jobID, "job-id", "", "job UUID; generated when omitted")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionJobStatusCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "status <set-id> <job-id>", Short: "Read one production job status", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			status, err := connection.ProductionJobStatus(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), status)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d state=%s receipt_sha256=%s\n",
				status.JobID, status.Revision, status.State, status.ReceiptSHA256)
			if err != nil {
				return fmt.Errorf("writing production job status: %w", err)
			}
			return nil
		}}
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionJobCancelCommand() *cobra.Command {
	var operationID string
	var etag int64
	var asJSON bool
	command := &cobra.Command{Use: "cancel <set-id> <job-id>", Short: "Cancel an exact production job", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.CancelProductionJob(cmd.Context(), args[0], args[1], etag,
				api.ProductionJobCancelRequest{OperationID: effectiveOperationID})
			if err != nil {
				return fmt.Errorf("canceling production job (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d operation_id=%s\n",
				receipt.SetID, receipt.Revision, receipt.ETag, receipt.OperationID)
			if err != nil {
				return fmt.Errorf("writing production job cancellation: %w", err)
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "finalized draft ETag pinned by the job")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}
