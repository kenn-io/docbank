package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func newProductionDraftForkCommand() *cobra.Command {
	var operationID string
	var asJSON bool
	command := &cobra.Command{Use: "fork <set-id> <revision>", Short: "Fork an exact production draft into a new editable revision", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			draft, err := connection.ForkProductionDraft(cmd.Context(), args[0], revision,
				api.ProductionForkRequest{OperationID: effectiveOperationID})
			if err != nil {
				return fmt.Errorf("forking production draft (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), draft)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d operation_id=%s\n",
				draft.SetID, draft.Revision, draft.ETag, effectiveOperationID)
			if err != nil {
				return fmt.Errorf("writing production draft fork: %w", err)
			}
			return nil
		}}
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}
