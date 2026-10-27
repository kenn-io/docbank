package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"
)

// MissLogRecord is sensitive, daemon-local state excluded from portable metadata.
type MissLogRecord struct {
	ID        string
	CreatedAt time.Time
	Receipt   string
}

// TryAddMissLogRecord gives automatic persistence a 100 ms context deadline,
// including admission to the existing storage writer.
// Explicit management and startup retention keep their ordinary blocking path.
func (s *Store) TryAddMissLogRecord(ctx context.Context, record MissLogRecord, now time.Time, days, maxRecords int) error {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	return s.AddMissLogRecord(ctx, record, now, days, maxRecords)
}

// AddMissLogRecord saves a receipt and prunes expired and excess records in one
// storage transaction, sharing SQLite and PostgreSQL writer serialization.
func (s *Store) AddMissLogRecord(ctx context.Context, record MissLogRecord, now time.Time, days, maxRecords int) error {
	if record.ID == "" || len(record.ID) > 128 || record.CreatedAt.IsZero() || len(record.Receipt) > 8<<20 || !jsontext.Value(record.Receipt).IsValid() || days < 1 || days > 365 || maxRecords < 1 || maxRecords > 100000 {
		return errors.New("invalid miss log record or retention")
	}
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
	if limit < 1 || limit > 1000 || offset < 0 {
		return nil, errors.New("invalid miss log pagination")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, receipt FROM miss_log_records ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []MissLogRecord
	for rows.Next() {
		var record MissLogRecord
		var created string
		if err := rows.Scan(&record.ID, &created, &record.Receipt); err != nil {
			return nil, err
		}
		record.CreatedAt, err = time.Parse(timestampLayout, created)
		if err != nil {
			return nil, fmt.Errorf("parse miss log timestamp: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) DeleteMissLogRecord(ctx context.Context, id string) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM miss_log_records WHERE id = $1`, id)
		return err
	})
}

func (s *Store) PurgeMissLogRecords(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, `DELETE FROM miss_log_records`); return err })
}

// PruneMissLogRecords enforces retention at enabled daemon startup.
func (s *Store) PruneMissLogRecords(ctx context.Context, now time.Time, days, maxRecords int) error {
	if now.IsZero() || days < 1 || days > 365 || maxRecords < 1 || maxRecords > 100000 {
		return errors.New("invalid miss log retention")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error { return pruneMissLogRecordsTx(ctx, tx, now, days, maxRecords) })
}

func pruneMissLogRecordsTx(ctx context.Context, tx *sql.Tx, now time.Time, days, maxRecords int) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM miss_log_records WHERE created_at < $1 OR id IN (SELECT id FROM miss_log_records ORDER BY created_at DESC, id DESC LIMIT 9223372036854775807 OFFSET $2)`, now.UTC().AddDate(0, 0, -days).Format(timestampLayout), maxRecords)
	return err
}

// DeleteMissLogRecordsBefore removes records strictly older than the cutoff.
func (s *Store) DeleteMissLogRecordsBefore(ctx context.Context, cutoff time.Time) error {
	if cutoff.IsZero() {
		return errors.New("invalid miss log cutoff")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM miss_log_records WHERE created_at < $1`, cutoff.UTC().Format(timestampLayout))
		return err
	})
}

// MissLogRecordByID reads private local state without exporting it.
func (s *Store) MissLogRecordByID(ctx context.Context, id string) (MissLogRecord, error) {
	var r MissLogRecord
	var created string
	if err := s.db.QueryRowContext(ctx, `SELECT id,created_at,receipt FROM miss_log_records WHERE id=$1`, id).Scan(&r.ID, &created, &r.Receipt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	var err error
	r.CreatedAt, err = time.Parse(timestampLayout, created)
	if err != nil {
		return r, fmt.Errorf("parse miss log timestamp: %w", err)
	}
	return r, nil
}
