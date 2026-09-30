package store

import (
	"context"
	"errors"
	"fmt"
)

const metadataBatchTagReceiptType = "batch_tag_receipt"

type metadataBatchTagReceipt struct {
	Type          string `json:"type"`
	OperationID   string `json:"operation_id" db:"operation_id"`
	RequestDigest string `json:"request_digest" db:"request_digest"`
	ReceiptJSON   []byte `json:"receipt_json" format:"byte" db:"receipt_json"`
}

var batchTagReceiptMetadata = newMetadataTable(metadataTable[metadataBatchTagReceipt]{
	record: metadataBatchTagReceipt{Type: metadataBatchTagReceiptType}, table: "batch_tag_receipts",
	suffix: "ORDER BY operation_id", validate: validateBatchTagMetadataRecord, checkExport: true})

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
	return batchTagReceiptMetadata.validateRows(ctx, tx)
}
