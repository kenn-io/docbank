package docbank

import (
	"context"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	productionservice "go.kenn.io/docbank/internal/production"
)

type ProductionPlayersSnapshotCreateRequest = api.ProductionPlayersSnapshotCreateRequest
type ProductionWithheldSelectionCreateRequest = api.ProductionWithheldSelectionCreateRequest

// CreateProductionPlayersSnapshot records one immutable, versioned player
// authority in this embedded vault. Exact operation retries return the same
// snapshot; changed payloads conflict.
func (v *Vault) CreateProductionPlayersSnapshot(ctx context.Context, snapshotID string, revision int64,
	request ProductionPlayersSnapshotCreateRequest) (documentproduction.PlayersSnapshot, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return documentproduction.PlayersSnapshot{}, ErrClosed
	}
	var created documentproduction.PlayersSnapshot
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		prepared, err := productionservice.PreparePlayersSnapshot(request.OperationID,
			documentproduction.PlayersSnapshot{
				Contract: documentproduction.PlayersSnapshotContractV1,
				ID:       snapshotID, Revision: revision, Players: request.Players,
			})
		if err != nil {
			return err
		}
		created, err = v.metadata.PutProductionPlayersSnapshot(ctx, prepared)
		return err
	})
	return created, err
}

// CreateProductionWithheldSelection records an explicit choice only after the
// Store verifies the exact sealed members and policy in this embedded vault.
func (v *Vault) CreateProductionWithheldSelection(ctx context.Context, setID string, revision int64,
	request ProductionWithheldSelectionCreateRequest) (documentproduction.WithheldSelection, error) {
	v.lifecycle.RLock()
	defer v.lifecycle.RUnlock()
	if v.closed {
		return documentproduction.WithheldSelection{}, ErrClosed
	}
	selection := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       request.SelectionID, SetID: setID, Revision: revision,
		PolicySHA256: request.PolicySHA256, Members: request.Members,
	}
	var created documentproduction.WithheldSelection
	err := embeddedMutationGate{vault: v}.MutateContext(ctx, func() error {
		var err error
		created, err = v.metadata.CreateStoredProductionWithheldSelection(ctx, request.OperationID, selection)
		return err
	})
	return created, err
}
