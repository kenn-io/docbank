package store

import (
	"context"
	"database/sql"
	"errors"

	productionservice "go.kenn.io/docbank/internal/production"
)

// ExportProductionPrivilegeLog verifies one frozen receipt and every stored
// private row in a single read snapshot, then emits only the public columns.
func (s *Store) ExportProductionPrivilegeLog(ctx context.Context, logID string, revision int64,
	format string) (productionservice.PrivilegeLogExport, error) {
	var empty productionservice.PrivilegeLogExport
	if validateUUIDv4(logID) != nil || revision < 1 ||
		format != "json" && format != "csv" && format != "xlsx" && format != "pdf" {
		return empty, invalidProductionStorage("invalid production privilege export")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	var digest string
	err = tx.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_privilege_log_receipts
		WHERE log_id=? AND revision=?`, logID, revision).Scan(&raw, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	receipt, err := decodePrivilegeReceipt(raw, digest)
	if err != nil {
		return empty, err
	}
	withheld, err := loadProductionWithheld(ctx, tx, receipt.WithheldSelectionSHA256)
	if err != nil {
		return empty, err
	}
	rows, err := loadPrivilegeRows(ctx, tx, logID, revision)
	if err != nil {
		return empty, err
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	switch format {
	case "json":
		return productionservice.ExportPrivilegeLogJSON(receipt, withheld, rows)
	case "csv":
		return productionservice.ExportPrivilegeLogCSV(receipt, withheld, rows)
	case "xlsx":
		return productionservice.ExportPrivilegeLogXLSX(receipt, withheld, rows)
	default:
		return productionservice.ExportPrivilegeLogPDF(receipt, withheld, rows)
	}
}
