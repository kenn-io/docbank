package store

import (
	"context"
	"database/sql"
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

const productionOperationPreparedInputs = "prepared_input_gates"

// ProductionGateSnapshotLoader reads production source rows through the exact
// writer transaction that will persist the gate run. It must not use Store.db
// or caller-supplied source values. The production-set persistence owner wires
// the concrete table loader when that later storage slice is integrated.
type ProductionGateSnapshotLoader func(context.Context, *sql.Tx, productionservice.PreparedInputRequest) (productionservice.StoredProductionInputs, error)

// ProductionGateStore adapts Store's immutable operation-receipt authority to
// the gate service while preserving one transaction around load, evaluation,
// gate results, receipt and audit.
type ProductionGateStore struct {
	store *Store
	load  ProductionGateSnapshotLoader
}

func NewProductionGateStore(store *Store, load ProductionGateSnapshotLoader) (*ProductionGateStore, error) {
	if store == nil || load == nil {
		return nil, invalidProductionStorage("invalid production gate store")
	}
	return &ProductionGateStore{store: store, load: load}, nil
}

func (s *ProductionGateStore) RunProductionGates(ctx context.Context, request productionservice.PreparedInputRequest, build productionservice.PreparedInputBuilder) (documentproduction.PreparedInputAuthority, error) {
	var authority documentproduction.PreparedInputAuthority
	if s == nil || s.store == nil || s.load == nil || build == nil {
		return authority, invalidProductionStorage("invalid production gate transaction")
	}
	requestSHA256, err := productionservice.PreparedInputRequestSHA256(request)
	if err != nil || !validPreparedOperation(request.OperationID, requestSHA256) {
		return authority, invalidProductionStorage("invalid prepared-input operation")
	}
	err = s.store.withStorageTx(ctx, func(tx *sql.Tx) error {
		replayed, ok, replayErr := productionOperationReplay[documentproduction.PreparedInputAuthority](ctx, tx,
			request.OperationID, productionOperationPreparedInputs, requestSHA256)
		if replayErr != nil || ok {
			authority = replayed
			if replayErr == nil {
				replayErr = validateStoredPreparedInputAuthority(authority, request.OperationID, requestSHA256)
			}
			return replayErr
		}
		stored, loadErr := s.load(ctx, tx, request)
		if loadErr != nil {
			return loadErr
		}
		authority, loadErr = build(stored)
		if loadErr != nil {
			return loadErr
		}
		if loadErr = validateStoredPreparedInputAuthority(authority, request.OperationID, requestSHA256); loadErr != nil {
			return loadErr
		}
		return recordProductionOperation(ctx, tx, request.OperationID, productionOperationPreparedInputs, requestSHA256, authority)
	})
	return authority, err
}

// PreparedInputAuthority returns only a self-consistent immutable gate run.
// Callers still pass it through production.ReserveAfterPreparedInput before
// invoking the numbering ledger.
func (s *Store) PreparedInputAuthority(ctx context.Context, operationID string) (documentproduction.PreparedInputAuthority, error) {
	var authority documentproduction.PreparedInputAuthority
	if validateUUIDv4(operationID) != nil {
		return authority, ErrNotFound
	}
	var kind, requestSHA256, responseSHA256 string
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT kind,request_sha256,response_sha256,response_json
		FROM production_operation_receipts WHERE operation_id=?`, operationID).Scan(
		&kind, &requestSHA256, &responseSHA256, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return authority, ErrNotFound
	}
	if err != nil {
		return authority, err
	}
	if kind != productionOperationPreparedInputs || digestProductionBytes(raw) != responseSHA256 {
		return authority, changedProductionPayload(operationID)
	}
	authority, err = decodePreparedInputAuthority(raw)
	if err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	if err := validateStoredPreparedInputAuthority(authority, operationID, requestSHA256); err != nil {
		return documentproduction.PreparedInputAuthority{}, err
	}
	return authority, nil
}

func decodePreparedInputAuthority(raw []byte) (documentproduction.PreparedInputAuthority, error) {
	authority, err := canonical.Decode[documentproduction.PreparedInputAuthority](raw)
	if err != nil {
		return authority, err
	}
	return authority, documentproduction.ValidatePreparedInputAuthority(authority)
}

func validateStoredPreparedInputAuthority(authority documentproduction.PreparedInputAuthority, operationID, requestSHA256 string) error {
	if err := documentproduction.ValidatePreparedInputAuthority(authority); err != nil {
		return err
	}
	if authority.Audit.OperationID != operationID || authority.Audit.RequestSHA256 != requestSHA256 {
		return changedProductionPayload(operationID)
	}
	return nil
}
