package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func newProductionDraftFinalizeCommand() *cobra.Command {
	var namespaceID, snapshotID, operationID string
	var etag, startAt int64
	var asJSON bool
	command := &cobra.Command{Use: "finalize <set-id> <revision>",
		Short: "Finalize an exact reviewed production draft", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			if namespaceID == "" || snapshotID == "" {
				return usageError(errors.New("--namespace-id and --snapshot-id are required"))
			}
			if startAt < 0 {
				return usageError(errors.New("--start-at must be nonnegative"))
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
			result, err := connection.FinalizeProductionDraft(cmd.Context(), args[0], revision, etag,
				api.ProductionFinalizeRequest{OperationID: effectiveOperationID, NamespaceID: namespaceID,
					SnapshotID: snapshotID, StartAt: startAt})
			if err != nil {
				return fmt.Errorf("finalizing production draft (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d state=%s receipt_sha256=%s operation_id=%s\n",
				result.Draft.SetID, result.Draft.Revision, result.Draft.State, result.ReceiptSHA256, effectiveOperationID)
			if err != nil {
				return fmt.Errorf("writing production finalization: %w", err)
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "current draft ETag required for revision fencing")
	command.Flags().StringVar(&namespaceID, "namespace-id", "", "existing Bates namespace UUID")
	command.Flags().StringVar(&snapshotID, "snapshot-id", "", "numbering snapshot UUID")
	command.Flags().Int64Var(&startAt, "start-at", 0, "first reserved number; 0 uses the next available namespace sequence")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}
