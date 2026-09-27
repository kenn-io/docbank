package daemonconn

import (
	"context"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
)

// CreateProductionPlayersSnapshot records versioned people and aliases and
// verifies the immutable authority returned by the daemon.
func (c *Connection) CreateProductionPlayersSnapshot(ctx context.Context, snapshotID string, revision int64,
	request api.ProductionPlayersSnapshotCreateRequest) (documentproduction.PlayersSnapshot, error) {
	if !validUUIDv4(snapshotID) || revision < 1 || !validUUIDv4(request.OperationID) ||
		len(request.Players) == 0 || len(request.Players) > documentproduction.MaxPrivilegeRows {
		return documentproduction.PlayersSnapshot{}, errors.New("invalid production players snapshot request")
	}
	_, expectedDigest, err := documentproduction.CanonicalPlayersSnapshot(documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       snapshotID, Revision: revision, Players: request.Players,
	})
	if err != nil {
		return documentproduction.PlayersSnapshot{}, err
	}
	result, err := c.API().CreateProductionPlayersSnapshot(ctx,
		&apiclient.CreateProductionPlayersSnapshotRequestOptions{
			PathParams: &apiclient.CreateProductionPlayersSnapshotPath{SnapshotID: snapshotID, Revision: revision},
			Body:       &request,
		})
	if err != nil {
		return documentproduction.PlayersSnapshot{}, err
	}
	if result == nil || documentproduction.ValidatePlayersSnapshot(*result) != nil ||
		result.ID != snapshotID || result.Revision != revision || result.SHA256 != expectedDigest {
		return documentproduction.PlayersSnapshot{}, integrityErrorf("production players snapshot is inconsistent")
	}
	return *result, nil
}
