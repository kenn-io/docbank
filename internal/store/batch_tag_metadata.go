package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const metadataBatchTagReceiptType = "batch_tag_receipt"

type metadataBatchTagReceipt struct {
	Type          string `json:"type"`
	OperationID   string `json:"operation_id"`
	RequestDigest string `json:"request_digest"`
	ReceiptJSON   []byte `json:"receipt_json" format:"byte"`
}

func exportBatchTagReceipts(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT operation_id,request_digest,receipt_json
		FROM batch_tag_receipts ORDER BY operation_id`)
	if err != nil {
		return fmt.Errorf("exporting batch tag receipts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataBatchTagReceipt{Type: metadataBatchTagReceiptType}
		if err := rows.Scan(
			&record.OperationID, &record.RequestDigest, &record.ReceiptJSON,
		); err != nil {
			return fmt.Errorf("scanning batch tag receipt metadata: %w", err)
		}
		if err := validateBatchTagMetadataRecord(record); err != nil {
			return fmt.Errorf("validating batch tag receipt metadata for export: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("batch tag receipt", rows)
}

func importBatchTagReceipt(
	ctx context.Context, tx *sql.Tx, record metadataBatchTagReceipt,
) error {
	if err := validateBatchTagMetadataRecord(record); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO batch_tag_receipts(
		operation_id,request_digest,receipt_json) VALUES(?,?,?)`,
		record.OperationID, record.RequestDigest, record.ReceiptJSON)
	return err
}

func validateBatchTagMetadataRecord(record metadataBatchTagReceipt) error {
	if record.Type != metadataBatchTagReceiptType {
		return errors.New("invalid batch tag receipt metadata type")
	}
	if err := validateUUIDv4(record.OperationID); err != nil {
		return fmt.Errorf("invalid batch tag receipt operation ID: %w", err)
	}
	receipt, err := decodeBatchTagReceiptV1(record.ReceiptJSON)
	if err != nil {
		return err
	}
	if receipt.OperationID != record.OperationID || receipt.RequestDigest != record.RequestDigest {
		return errors.New("batch tag receipt metadata identity does not match receipt JSON")
	}
	return nil
}

func validateBatchTagReceiptMetadataState(ctx context.Context, tx metadataQuerier) error {
	return exportBatchTagReceipts(ctx, tx, func(any) error { return nil })
}
