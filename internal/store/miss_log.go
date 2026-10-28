package store

import (
	"context"
	"database/sql"
	"time"
)

// MissLogRecord is sensitive, daemon-local state excluded from portable metadata.
type MissLogRecord struct {
	ID        string
	CreatedAt time.Time
	Receipt   string
}

// AddMissLogRecord saves a receipt and prunes expired and excess records in one
// storage transaction, sharing SQLite writer serialization.
func (s *Store) AddMissLogRecord(ctx context.Context, record MissLogRecord, now time.Time, days, maxRecords int) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO miss_log_records(id, created_at, receipt) VALUES ($1, $2, $3)`, record.ID, record.CreatedAt.UTC().Format(timestampLayout), record.Receipt)
		if err != nil {
			return err
		}
		return pruneMissLogRecordsTx(ctx, tx, now, days, maxRecords)
	})
}

// ListMissLogRecords returns records newest first with bounded pagination.
func (s *Store) ListMissLogRecords(ctx context.Context, limit, offset int) ([]MissLogRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, receipt FROM miss_log_records ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []MissLogRecord
	for rows.Next() {
		var record MissLogRecord
		if err := rows.Scan(&record.ID, &record.Receipt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) DeleteMissLogRecord(ctx context.Context, id string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM miss_log_records WHERE id = $1`, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count == 0 {
			return ErrNotFound
		}
		return err
	})
}

func (s *Store) PurgeMissLogRecords(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, `DELETE FROM miss_log_records`); return err })
}

// PruneMissLogRecords enforces retention at enabled daemon startup.
func (s *Store) PruneMissLogRecords(ctx context.Context, now time.Time, days, maxRecords int) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error { return pruneMissLogRecordsTx(ctx, tx, now, days, maxRecords) })
}

func pruneMissLogRecordsTx(ctx context.Context, tx *sql.Tx, now time.Time, days, maxRecords int) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM miss_log_records WHERE created_at < $1 OR id IN (SELECT id FROM miss_log_records ORDER BY created_at DESC, id DESC LIMIT 9223372036854775807 OFFSET $2)`, now.UTC().AddDate(0, 0, -days).Format(timestampLayout), maxRecords)
	return err
}
