package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	documentproduction "go.kenn.io/docbank/document/production"
	productionservice "go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
)

// ProductionPlayersSnapshotCreateRequest records versioned people and aliases
// for an exact privilege-log input. The path supplies the snapshot identity.
type ProductionPlayersSnapshotCreateRequest struct {
	OperationID string                      `json:"operation_id"`
	Players     []documentproduction.Player `json:"players"`
}

func productionPlayersMutationError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return FromStoreError(err)
	}
	if problem, ok := errors.AsType[*documentproduction.Problem](err); ok {
		if problem.Code == documentproduction.ProblemChangedPayload {
			return NewError(http.StatusConflict, "production_players_conflict", "player snapshot authority changed")
		}
		return NewError(http.StatusUnprocessableEntity, "invalid_production_players", "player snapshot is invalid")
	}
	return NewError(http.StatusInternalServerError, "production_players_failed", "player snapshot operation failed")
}

func registerProductionPlayersRoutes(api huma.API, d Deps, g *OperationGate) {
	huma.Register(api, huma.Operation{
		OperationID: "createProductionPlayersSnapshot", Method: http.MethodPost,
		Path:          "/api/v1/production-player-snapshots/{snapshot_id}/revisions/{revision}",
		Summary:       "Record versioned people and aliases for privilege logs",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 64 << 20,
	}, func(ctx context.Context, in *struct {
		SnapshotID string `path:"snapshot_id"`
		Revision   int64  `path:"revision" minimum:"1"`
		Body       ProductionPlayersSnapshotCreateRequest
	}) (*struct {
		Body documentproduction.PlayersSnapshot
	}, error) {
		if _, ok := workspaceSnapshotOwner(ctx); !ok {
			return nil, NewError(http.StatusUnauthorized, "unauthorized", "authenticated production actor is missing")
		}
		var created documentproduction.PlayersSnapshot
		err := g.mutate(func() error {
			prepared, err := productionservice.PreparePlayersSnapshot(in.Body.OperationID,
				documentproduction.PlayersSnapshot{
					Contract: documentproduction.PlayersSnapshotContractV1,
					ID:       in.SnapshotID, Revision: in.Revision, Players: in.Body.Players,
				})
			if err != nil {
				return err
			}
			created, err = d.Store.PutProductionPlayersSnapshot(ctx, prepared)
			return err
		})
		if err != nil {
			return nil, productionPlayersMutationError(err)
		}
		return &struct {
			Body documentproduction.PlayersSnapshot
		}{Body: created}, nil
	})
}
