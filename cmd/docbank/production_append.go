package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func newProductionDraftAppendCommand() *cobra.Command {
	var membersFile, operationID string
	var etag int64
	var asJSON bool
	command := &cobra.Command{Use: "append <set-id> <revision>", Short: "Append retained members to an exact production draft", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if etag < 1 {
				return usageError(errors.New("--etag is required and must be positive"))
			}
			if membersFile == "" {
				return usageError(errors.New("--members-file is required"))
			}
			revision, err := productionDraftRevision(args[1])
			if err != nil {
				return err
			}
			members, err := readProductionMembers(cmd, membersFile)
			if err != nil {
				return err
			}
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			request := api.ProductionMemberAppendRequest{OperationID: effectiveOperationID, Members: members}
			if err := redaction.ValidateApplyRequest(request.Domain(etag)); err != nil {
				return usageError(err)
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			receipt, err := connection.AppendProductionMembers(cmd.Context(), args[0], revision, etag, request)
			if err != nil {
				return fmt.Errorf("appending production members (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), receipt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d operation_id=%s\n",
				receipt.SetID, receipt.Revision, receipt.ETag, receipt.OperationID)
			if err != nil {
				return fmt.Errorf("writing production member receipt: %w", err)
			}
			return nil
		}}
	command.Flags().Int64Var(&etag, "etag", 0, "current draft ETag required for revision fencing")
	command.Flags().StringVar(&membersFile, "members-file", "", "JSON array of retained members; use - for stdin")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func readProductionMembers(cmd *cobra.Command, path string) ([]api.ProductionMember, error) {
	reader := cmd.InOrStdin()
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening production members: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	raw, err := io.ReadAll(io.LimitReader(reader, redaction.MaxCommandBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading production members: %w", err)
	}
	if len(raw) > redaction.MaxCommandBytes {
		return nil, usageError(fmt.Errorf("production members exceed %d bytes", redaction.MaxCommandBytes))
	}
	var members []api.ProductionMember
	if err := json.Unmarshal(raw, &members, json.RejectUnknownMembers(true)); err != nil {
		return nil, usageError(fmt.Errorf("invalid production members JSON: %w", err))
	}
	return members, nil
}
