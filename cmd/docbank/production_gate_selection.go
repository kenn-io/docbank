package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newProductionDraftGateSelectionCommand() *cobra.Command {
	var approvalID, privilegeLogID string
	var privilegeLogRevision int64
	var asJSON bool
	cmd := &cobra.Command{Use: "select-gates <set-id> <revision>",
		Short: "Pin an existing approval and frozen privilege log to a sealed revision",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			if (privilegeLogID == "") != (privilegeLogRevision == 0) || privilegeLogRevision < 0 {
				return usageError(errors.New("--privilege-log-id and --privilege-log-revision must be supplied together"))
			}
			request := api.ProductionGateSelectionRequest{ApprovalID: approvalID,
				PrivilegeLogID: privilegeLogID, PrivilegeLogRevision: privilegeLogRevision}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if err := connection.SelectProductionGateAuthority(cmd.Context(), args[0], revision, request); err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), struct {
					SetID                string `json:"set_id"`
					Revision             int64  `json:"revision"`
					ApprovalID           string `json:"approval_id,omitzero"`
					PrivilegeLogID       string `json:"privilege_log_id,omitzero"`
					PrivilegeLogRevision int64  `json:"privilege_log_revision,omitzero"`
				}{SetID: args[0], Revision: revision, ApprovalID: approvalID,
					PrivilegeLogID: privilegeLogID, PrivilegeLogRevision: privilegeLogRevision})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "selected gate authority for %s revision=%d\n", args[0], revision)
			if err != nil {
				return fmt.Errorf("writing gate selection: %w", err)
			}
			return nil
		}}
	cmd.Flags().StringVar(&approvalID, "approval-id", "", "existing authenticated-human approval grant ID")
	cmd.Flags().StringVar(&privilegeLogID, "privilege-log-id", "", "existing frozen privilege log ID")
	cmd.Flags().Int64Var(&privilegeLogRevision, "privilege-log-revision", 0, "exact frozen privilege log revision")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}
