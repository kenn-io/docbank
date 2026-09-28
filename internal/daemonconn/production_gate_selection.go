package daemonconn

import (
	"context"
	"errors"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// SelectProductionGateAuthority pins existing verified approval and frozen
// privilege authority to an exact sealed revision through the daemon.
func (c *Connection) SelectProductionGateAuthority(ctx context.Context, setID string, revision int64,
	request api.ProductionGateSelectionRequest) error {
	parsed, err := productionSetUUID(setID)
	if err != nil || revision < 1 ||
		request.ApprovalID != "" && !validUUIDv4(request.ApprovalID) ||
		(request.PrivilegeLogID == "") != (request.PrivilegeLogRevision == 0) ||
		request.PrivilegeLogID != "" && (!validUUIDv4(request.PrivilegeLogID) || request.PrivilegeLogRevision < 1) {
		return errors.New("invalid production gate selection")
	}
	_, err = c.API().SelectProductionGateAuthority(ctx, &apiclient.SelectProductionGateAuthorityRequestOptions{
		PathParams: &apiclient.SelectProductionGateAuthorityPath{SetID: parsed, Revision: revision},
		Body:       &request,
	})
	return err
}
