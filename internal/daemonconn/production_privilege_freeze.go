package daemonconn

import (
	"context"
	"errors"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
)

// FreezeProductionPrivilegeLog asks the daemon to recheck a validated stored
// draft and any required approval before committing an immutable receipt.
func (c *Connection) FreezeProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	request api.ProductionPrivilegeFreezeRequest) (documentproduction.PrivilegeLogReceipt, error) {
	frozenAt, err := time.Parse(time.RFC3339Nano, request.FrozenAt)
	if !validUUIDv4(logID) || revision < 1 || !validUUIDv4(request.OperationID) ||
		request.ExpectedGeneration < 1 || !canonical.IsSHA256Hex(request.ExpectedInputsSHA256) ||
		request.ExpectedApprovalEvaluationSHA256 != "" &&
			!canonical.IsSHA256Hex(request.ExpectedApprovalEvaluationSHA256) ||
		err != nil || frozenAt.Location() != time.UTC ||
		frozenAt.Format(time.RFC3339Nano) != request.FrozenAt {
		return documentproduction.PrivilegeLogReceipt{}, errors.New("invalid production privilege freeze request")
	}
	result, err := c.API().FreezeProductionPrivilegeLog(ctx,
		&apiclient.FreezeProductionPrivilegeLogRequestOptions{
			PathParams: &apiclient.FreezeProductionPrivilegeLogPath{Log: logID, Revision: revision},
			Body:       &request,
		})
	if err != nil {
		return documentproduction.PrivilegeLogReceipt{}, err
	}
	if result == nil || documentproduction.ValidatePrivilegeLogReceipt(*result) != nil ||
		result.LogID != logID || result.Revision != revision ||
		result.InputsSHA256 != request.ExpectedInputsSHA256 ||
		result.ApprovalEvaluationSHA256 != request.ExpectedApprovalEvaluationSHA256 ||
		result.FrozenAt != request.FrozenAt {
		return documentproduction.PrivilegeLogReceipt{}, integrityErrorf("production privilege freeze receipt is inconsistent")
	}
	return *result, nil
}
