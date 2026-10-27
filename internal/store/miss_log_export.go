package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MissLogExportRecords reads one byte-bounded page before callers resolve its
// identities. Closing rows first also releases SQLite's single connection.
// The extra row inspected at the byte boundary is at most one 8 MiB receipt.
func (s *Store) MissLogExportRecords(ctx context.Context, limit, offset int) ([]MissLogRecord, bool, error) {
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, false, errors.New("invalid miss log export pagination")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, created_at, receipt FROM miss_log_records ORDER BY created_at DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, false, fmt.Errorf("querying miss log export page: %w", err)
	}
	defer rows.Close()
	records := []MissLogRecord{}
	total := 0
	for rows.Next() {
		var row MissLogRecord
		var created string
		if err := rows.Scan(&row.ID, &created, &row.Receipt); err != nil {
			return nil, false, fmt.Errorf("reading miss log export receipt: %w", err)
		}
		if len(row.Receipt) > 8<<20 {
			return nil, false, errors.New("miss log receipt exceeds export bounds")
		}
		if total+len(row.Receipt) > 8<<20 {
			return records, true, nil
		}
		total += len(row.Receipt)
		row.CreatedAt, err = time.Parse(timestampLayout, created)
		if err != nil {
			return nil, false, fmt.Errorf("reading miss log export timestamp: %w", err)
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("reading miss log export page: %w", err)
	}
	return records, len(records) == limit, nil
}
