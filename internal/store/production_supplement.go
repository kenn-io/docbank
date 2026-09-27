package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

const productionOperationSupplement = "supplement"

// CreateProductionSupplement reserves a child's numbers and records its link
// to a published parent in one logical transaction.
func (s *Store) CreateProductionSupplement(ctx context.Context, actor string, request production.SupplementRequest) (production.SupplementRecord, error) {
	bad := func() (production.SupplementRecord, error) {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	requestSHA, err := production.SupplementRequestSHA256(request)
	if ctx == nil || err != nil || !validProductionActor(actor) {
		return bad()
	}
	if prior, err := s.LoadProductionSupplement(ctx, request.OperationID); err == nil {
		if prior.RequestSHA256 != requestSHA {
			return bad()
		}
		return prior, nil
	} else if !errors.Is(err, ErrNotFound) {
		return bad()
	}
	parent, err := s.LoadProductionPackageInputs(ctx, request.ParentJobID)
	if err != nil || parent.Job.Receipt.SHA256 != request.ParentReceiptSHA256 {
		return bad()
	}
	parentAllocation, err := s.ProductionNumberingForJob(ctx, parent.Job.ID)
	if err != nil || parentAllocation.AllocationID != parent.Reservation.ID || parentAllocation.EndSequence < 1 {
		return bad()
	}
	child, err := s.LoadProductionJob(ctx, request.JobID)
	if err != nil || child.State != production.ProductionJobQueued || child.SetID != parent.Job.SetID ||
		child.Revision <= parent.Job.Revision || child.RevisionSHA256 != request.PreparedSHA256 ||
		child.PreparedInputSHA256 != request.PreparedInputSHA256 || child.RevisionSHA256 == parent.Job.RevisionSHA256 {
		return bad()
	}
	var predecessor sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT predecessor_revision FROM production_revisions
		WHERE set_id=? AND revision=?`, child.SetID, child.Revision).Scan(&predecessor); err != nil ||
		!predecessor.Valid || predecessor.Int64 != parent.Job.Revision {
		return bad()
	}
	finalized, err := s.LoadFinalizedProduction(ctx, child.SetID, child.Revision)
	if err != nil {
		return bad()
	}
	plan, err := s.productionJobBatesRequest(ctx, child, finalized)
	if err != nil || plan.NamespaceID != parentAllocation.NamespaceID ||
		plan.SnapshotID == parentAllocation.SnapshotID || plan.StartAt != 0 {
		return bad()
	}
	var result production.SupplementRecord
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		prior, replay, err := productionOperationReplay[production.SupplementRecord](ctx, tx, request.OperationID, productionOperationSupplement, requestSHA)
		if err != nil {
			return err
		}
		if replay {
			if production.ValidateSupplementRecord(prior) != nil {
				return production.ErrSupplementConflict
			}
			result = prior
			return nil
		}
		var parentState, parentReceipt, parentAllocationID string
		err = tx.QueryRowContext(ctx, `SELECT state,receipt_sha256,allocation_id FROM production_jobs WHERE job_id=?`, parent.Job.ID).
			Scan(&parentState, &parentReceipt, &parentAllocationID)
		if err != nil || parentState != production.ProductionJobSucceeded || parentReceipt != request.ParentReceiptSHA256 ||
			parentAllocationID != parentAllocation.AllocationID {
			return production.ErrSupplementConflict
		}
		var childState, childSetID, childPrepared, childReceipt string
		var childRevision, childETag int64
		err = tx.QueryRowContext(ctx, `SELECT state,set_id,revision,etag,revision_sha256,prepared_input_sha256
   FROM production_jobs WHERE job_id=?`, child.ID).Scan(&childState, &childSetID, &childRevision, &childETag, &childPrepared, &childReceipt)
		if err != nil || childState != production.ProductionJobQueued || childSetID != child.SetID || childRevision != child.Revision ||
			childETag != child.ETag || childPrepared != request.PreparedSHA256 || childReceipt != request.PreparedInputSHA256 {
			return production.ErrSupplementConflict
		}
		var existing int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, child.ID).Scan(&existing)
		if err != nil || existing != 0 {
			return production.ErrSupplementConflict
		}
		var cursor int64
		err = tx.QueryRowContext(ctx, `SELECT next_sequence FROM bates_namespace_cursors WHERE namespace_id=?`, plan.NamespaceID).Scan(&cursor)
		if err != nil || cursor <= parentAllocation.EndSequence {
			return production.ErrSupplementConflict
		}
		var allocation BatesAllocation
		err = production.ReserveAfterPreparedInput(finalized.Authority, production.PreparedInputReference{
			OperationID: finalized.Authority.Audit.OperationID, PreparedSHA256: request.PreparedSHA256,
			ReceiptSHA256: request.PreparedInputSHA256,
		}, func(documentproduction.PreparedInputAuthority) error {
			var reserveErr error
			allocation, reserveErr = s.reserveBatesRangeTx(ctx, tx, plan)
			return reserveErr
		})
		if err != nil || allocation.StartSequence <= parentAllocation.EndSequence {
			return production.ErrSupplementConflict
		}
		numbers := make([]documentproduction.AssignedNumber, 0, len(allocation.Labels))
		for _, label := range allocation.Labels {
			numbers = append(numbers, documentproduction.AssignedNumber{MemberID: label.OccurrenceID,
				MemberOrdinal: int64(label.Ordinal), Page: label.SourcePage, Text: label.Label})
		}
		reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1,
			Authority: "bates-ledger/v1", ID: allocation.AllocationID, OperationID: child.ID,
			RevisionSHA256: request.PreparedSHA256, State: allocation.State, Numbers: numbers}
		_, reservation.SHA256, err = documentproduction.CanonicalNumberReservation(reservation)
		if err != nil {
			return err
		}
		result = production.SupplementRecord{Contract: production.SupplementRecordContractV1, OperationID: request.OperationID,
			ParentJobID: parent.Job.ID, JobID: child.ID, ParentReceiptSHA256: request.ParentReceiptSHA256,
			PreparedSHA256: request.PreparedSHA256, PreparedInputSHA256: request.PreparedInputSHA256, RequestSHA256: requestSHA,
			SetID: child.SetID, Revision: child.Revision, NamespaceID: allocation.NamespaceID,
			ParentAllocationID: parentAllocation.AllocationID, AllocationID: allocation.AllocationID,
			NumberReservationSHA256: reservation.SHA256, ParentEndSequence: parentAllocation.EndSequence,
			StartSequence: allocation.StartSequence, EndSequence: allocation.EndSequence,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		_, result.SHA256, err = production.CanonicalSupplementRecord(result)
		if err != nil {
			return err
		}
		raw, err := canonical.Marshal(result)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO production_operations(operation_id,set_id,actor,kind,request_sha256,receipt_json,created_at)
   VALUES(?,?,?,?,?,?,?)`, request.OperationID, child.SetID, actor, productionOperationSupplement, requestSHA, raw, result.CreatedAt); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO production_audit_evidence(operation_id,set_id,revision,actor,kind,
   request_sha256,receipt_sha256,created_at) VALUES(?,?,?,?,?,?,?,?)`, request.OperationID,
			child.SetID, child.Revision, actor, productionOperationSupplement, requestSHA, digestProductionBytes(raw), result.CreatedAt); err != nil {
			return err
		}
		return recordProductionOperation(ctx, tx, request.OperationID, productionOperationSupplement, requestSHA, result)
	})
	if err != nil {
		return bad()
	}
	return result, nil
}

func (s *Store) LoadProductionSupplement(ctx context.Context, operationID string) (production.SupplementRecord, error) {
	if ctx == nil || validateUUIDv4(operationID) != nil {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	return loadProductionSupplement(ctx, s.db, operationID)
}

func loadProductionSupplement(ctx context.Context, q metadataQuerier, operationID string) (production.SupplementRecord, error) {
	var result production.SupplementRecord
	var setID, kind, requestSHA, createdAt string
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT set_id,kind,request_sha256,receipt_json,created_at
		FROM production_operations WHERE operation_id=?`, operationID).
		Scan(&setID, &kind, &requestSHA, &raw, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result, err = canonical.Decode[production.SupplementRecord](raw)
	if err != nil || kind != productionOperationSupplement || production.ValidateSupplementRecord(result) != nil ||
		result.OperationID != operationID || result.SetID != setID || result.RequestSHA256 != requestSHA ||
		result.CreatedAt != createdAt {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	encoded, err := canonical.Marshal(result)
	if err != nil || !bytes.Equal(encoded, raw) {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	var parentState, parentReceipt, parentAllocationID string
	var parentRevision int64
	err = q.QueryRowContext(ctx, `SELECT state,revision,receipt_sha256,allocation_id FROM production_jobs WHERE job_id=?`,
		result.ParentJobID).Scan(&parentState, &parentRevision, &parentReceipt, &parentAllocationID)
	if err != nil || parentState != production.ProductionJobSucceeded ||
		parentReceipt != result.ParentReceiptSHA256 || parentAllocationID != result.ParentAllocationID {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	var childSetID, childPrepared, childReceipt, allocationID string
	var childRevision int64
	err = q.QueryRowContext(ctx, `SELECT set_id,revision,revision_sha256,prepared_input_sha256
		FROM production_jobs WHERE job_id=?`, result.JobID).
		Scan(&childSetID, &childRevision, &childPrepared, &childReceipt)
	if err != nil || childSetID != result.SetID || childRevision != result.Revision ||
		childPrepared != result.PreparedSHA256 || childReceipt != result.PreparedInputSHA256 {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	var predecessor sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT predecessor_revision FROM production_revisions
		WHERE set_id=? AND revision=?`, result.SetID, result.Revision).Scan(&predecessor); err != nil ||
		!predecessor.Valid || predecessor.Int64 != parentRevision {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	err = q.QueryRowContext(ctx, `SELECT allocation_id FROM bates_allocations WHERE operation_id=?`,
		result.JobID).Scan(&allocationID)
	if err != nil || allocationID != result.AllocationID {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	parentAllocation, err := loadBatesAllocation(ctx, q, result.ParentAllocationID)
	if err != nil || parentAllocation.NamespaceID != result.NamespaceID ||
		parentAllocation.EndSequence != result.ParentEndSequence {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	childAllocation, err := loadBatesAllocation(ctx, q, result.AllocationID)
	if err != nil || childAllocation.NamespaceID != result.NamespaceID ||
		childAllocation.SnapshotID == parentAllocation.SnapshotID ||
		childAllocation.StartSequence != result.StartSequence || childAllocation.EndSequence != result.EndSequence {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	numbers := make([]documentproduction.AssignedNumber, 0, len(childAllocation.Labels))
	for _, label := range childAllocation.Labels {
		numbers = append(numbers, documentproduction.AssignedNumber{MemberID: label.OccurrenceID,
			MemberOrdinal: int64(label.Ordinal), Page: label.SourcePage, Text: label.Label})
	}
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1,
		Authority: "bates-ledger/v1", ID: childAllocation.AllocationID, OperationID: result.JobID,
		RevisionSHA256: result.PreparedSHA256, State: batesAllocationStateReserved, Numbers: numbers}
	_, reservationSHA, err := documentproduction.CanonicalNumberReservation(reservation)
	if err != nil || reservationSHA != result.NumberReservationSHA256 {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	var receiptKind, receiptRequest, receiptResponseSHA string
	var receiptRaw []byte
	err = q.QueryRowContext(ctx, `SELECT kind,request_sha256,response_sha256,response_json
		FROM production_operation_receipts WHERE operation_id=?`, operationID).
		Scan(&receiptKind, &receiptRequest, &receiptResponseSHA, &receiptRaw)
	if err != nil || receiptKind != kind || receiptRequest != requestSHA ||
		receiptResponseSHA != digestProductionBytes(raw) || !bytes.Equal(receiptRaw, raw) {
		return production.SupplementRecord{}, production.ErrSupplementConflict
	}
	return result, nil
}
