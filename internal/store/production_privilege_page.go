package store

import (
	"bytes"
	"context"
	"errors"
	"strconv"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
)

const MaxProductionPrivilegePage = 100

type ProductionPrivilegePublicPage struct {
	Receipt    documentproduction.PrivilegeLogReceipt  `json:"receipt"`
	Rows       []documentproduction.PrivilegePublicRow `json:"rows"`
	NextCursor string                                  `json:"next_cursor"`
}

// ProductionPrivilegePublicPage reads one exact frozen revision and a bounded
// page of its allowlisted rows. Frozen rows are immutable in the Store schema.
func (s *Store) ProductionPrivilegePublicPage(ctx context.Context, logID string, revision int64,
	cursor string, limit int) (ProductionPrivilegePublicPage, error) {
	if validateUUIDv4(logID) != nil || revision < 1 || limit < 1 || limit > MaxProductionPrivilegePage ||
		len(cursor) > 6 {
		return ProductionPrivilegePublicPage{}, invalidProductionStorage("invalid production privilege page")
	}
	after := 0
	if cursor != "" {
		value, err := strconv.Atoi(cursor)
		if err != nil || value < 1 || strconv.Itoa(value) != cursor {
			return ProductionPrivilegePublicPage{}, invalidProductionStorage("invalid production privilege cursor")
		}
		after = value
	}
	receipt, err := s.PrivilegeLogReceipt(ctx, logID, revision)
	if err != nil {
		return ProductionPrivilegePublicPage{}, err
	}
	if after > receipt.RowCount {
		return ProductionPrivilegePublicPage{}, invalidProductionStorage("invalid production privilege cursor")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT row_ordinal,row_id,canonical_json
		FROM production_privilege_log_rows WHERE log_id=? AND revision=? AND row_ordinal>?
		ORDER BY row_ordinal LIMIT ?`, logID, revision, after, limit+1)
	if err != nil {
		return ProductionPrivilegePublicPage{}, err
	}
	defer func() { _ = rows.Close() }()
	page := ProductionPrivilegePublicPage{Receipt: receipt, Rows: make([]documentproduction.PrivilegePublicRow, 0, limit)}
	expectedOrdinal := after + 1
	for rows.Next() {
		var ordinal int
		var rowID string
		var raw []byte
		if err := rows.Scan(&ordinal, &rowID, &raw); err != nil {
			return ProductionPrivilegePublicPage{}, err
		}
		if ordinal != expectedOrdinal || ordinal > receipt.RowCount {
			return ProductionPrivilegePublicPage{}, errors.New("production privilege row order is inconsistent")
		}
		expectedOrdinal++
		row, err := canonical.Decode[documentproduction.PrivilegeRow](raw)
		if err != nil {
			return ProductionPrivilegePublicPage{}, err
		}
		canonicalRows, _, err := documentproduction.CanonicalPrivilegeRows([]documentproduction.PrivilegeRow{row})
		if err != nil || row.ID != rowID || !bytes.Equal(raw, canonicalRows[1:len(canonicalRows)-1]) {
			return ProductionPrivilegePublicPage{}, errors.New("production privilege row is inconsistent")
		}
		if len(page.Rows) == limit {
			page.NextCursor = strconv.Itoa(ordinal - 1)
			break
		}
		page.Rows = append(page.Rows, documentproduction.PublicPrivilegeRows([]documentproduction.PrivilegeRow{row})[0])
	}
	if err := rows.Err(); err != nil {
		return ProductionPrivilegePublicPage{}, err
	}
	if page.NextCursor == "" && expectedOrdinal-1 != receipt.RowCount {
		return ProductionPrivilegePublicPage{}, errors.New("production privilege row count is inconsistent")
	}
	return page, nil
}
