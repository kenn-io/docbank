package daemonconn

import (
	"context"
	"errors"
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// CreateProductionWithheldSelection records an explicit choice from the
// sealed membership and verifies the returned stored authority.
func (c *Connection) CreateProductionWithheldSelection(ctx context.Context, setID string, revision int64,
	request api.ProductionWithheldSelectionCreateRequest) (documentproduction.WithheldSelection, error) {
	if !validUUIDv4(setID) || revision < 1 || !validUUIDv4(request.OperationID) ||
		!validUUIDv4(request.SelectionID) || len(request.PolicySHA256) != 64 || len(request.Members) == 0 {
		return documentproduction.WithheldSelection{}, errors.New("invalid production withheld selection request")
	}
	result, err := c.API().CreateProductionWithheldSelection(ctx,
		&apiclient.CreateProductionWithheldSelectionRequestOptions{
			PathParams: &apiclient.CreateProductionWithheldSelectionPath{SetID: setID, Revision: revision},
			Body:       &request,
		})
	if err != nil {
		return documentproduction.WithheldSelection{}, err
	}
	if result == nil || documentproduction.ValidateWithheldSelection(*result) != nil ||
		result.SetID != setID || result.Revision != revision || result.ID != request.SelectionID ||
		result.PolicySHA256 != request.PolicySHA256 || !slices.EqualFunc(result.Members, request.Members,
		func(a, b documentproduction.WithheldMember) bool {
			return a.ID == b.ID && a.Ordinal == b.Ordinal &&
				a.SourceVersionID == b.SourceVersionID && a.SourceSHA256 == b.SourceSHA256 &&
				a.SourceSize == b.SourceSize && a.FamilyOrder == b.FamilyOrder && a.Family == b.Family
		}) {
		return documentproduction.WithheldSelection{}, integrityErrorf("production withheld selection is inconsistent")
	}
	return *result, nil
}
