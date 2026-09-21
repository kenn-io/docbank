package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/report"
)

// TermReportHistory retains only a reusable request and run receipt. Report
// artifacts and date-evidence pages are deliberately excluded.
type TermReportHistory struct {
	Request report.Request `json:"request"`
	Summary report.Summary `json:"summary"`
}

type TermReportHistoryPage struct {
	Items []TermReportHistory `json:"items"`
	Total int                 `json:"total"`
}

const metadataTermReportHistoryType = "term_report_history"

type metadataTermReportHistory struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	ParentID    string `json:"parent_id"`
	ObservedAt  string `json:"observed_at"`
	RequestJSON []byte `json:"request_json" format:"byte"`
	SummaryJSON []byte `json:"summary_json" format:"byte"`
}

const (
	termReportHistoryLimit   = 100
	maxTermReportHistoryPage = 16 << 20
)

func validateTermReportHistory(item TermReportHistory) error {
	request, err := report.NormalizeRequest(item.Request)
	if err != nil || len(request.DateChoices) != 0 {
		return errors.New("invalid report history request")
	}
	s := item.Summary
	if len(s.ID) != 48 || !validReportHistoryID(s.ID) || s.ObservedAt.IsZero() || s.ExpiresAt.IsZero() ||
		(s.State != report.StateComplete && s.State != "needs_review") ||
		len(s.Terms) != len(request.Terms) || s.UnresolvedDates < 0 {
		return errors.New("invalid report history receipt")
	}
	for i := range s.Terms {
		if s.Terms[i] != request.Terms[i] {
			return errors.New("report history terms do not match request")
		}
	}
	if s.ParentID != "" && !validReportHistoryID(s.ParentID) {
		return errors.New("invalid report history parent")
	}
	if !s.ExpiresAt.After(s.ObservedAt) {
		return errors.New("invalid report history expiry")
	}
	return nil
}

func validReportHistoryID(id string) bool {
	if len(id) != 48 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// SaveTermReportHistory commits a bounded history entry only after its frozen
// run has been prepared. The oldest entry falls out once the vault has 100.
func (s *Store) SaveTermReportHistory(ctx context.Context, item TermReportHistory) error {
	item.Request.DateChoices = nil
	if err := validateTermReportHistory(item); err != nil {
		return err
	}
	requestJSON, err := json.Marshal(item.Request)
	if err != nil {
		return err
	}
	summaryJSON, err := json.Marshal(item.Summary)
	if err != nil {
		return err
	}
	if len(requestJSON) > report.MaxRequestSummaryJSONBytes || len(summaryJSON) > report.MaxRequestSummaryJSONBytes {
		return errors.New("report history receipt exceeds limit")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO term_report_history(
			id,parent_id,observed_at,request_json,summary_json
		) VALUES(?,?,?,?,?)`, item.Summary.ID, item.Summary.ParentID,
			item.Summary.ObservedAt.UTC().Format(timestampLayout), requestJSON, summaryJSON); err != nil {
			return fmt.Errorf("saving report history: %w", err)
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM term_report_history WHERE id IN (
			SELECT id FROM term_report_history ORDER BY observed_at DESC,id DESC
			LIMIT -1 OFFSET ?
		)`, termReportHistoryLimit)
		return err
	})
}

func (s *Store) ListTermReportHistory(ctx context.Context, offset, limit int) (TermReportHistoryPage, error) {
	if offset < 0 || offset > termReportHistoryLimit || limit < 1 || limit > 50 {
		return TermReportHistoryPage{}, errors.New("invalid report history page")
	}
	var page TermReportHistoryPage
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM term_report_history`).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,parent_id,observed_at,request_json,summary_json
		FROM term_report_history ORDER BY observed_at DESC,id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	page.Items = make([]TermReportHistory, 0, limit)
	pageBytes := 0
	for rows.Next() {
		var id, parentID, observedAt string
		var requestJSON, summaryJSON []byte
		if err := rows.Scan(&id, &parentID, &observedAt, &requestJSON, &summaryJSON); err != nil {
			return TermReportHistoryPage{}, err
		}
		itemBytes := len(requestJSON) + len(summaryJSON)
		if len(page.Items) != 0 && pageBytes+itemBytes > maxTermReportHistoryPage {
			break
		}
		item, err := decodeTermReportHistory(id, parentID, observedAt, requestJSON, summaryJSON)
		if err != nil {
			return TermReportHistoryPage{}, err
		}
		page.Items = append(page.Items, item)
		pageBytes += itemBytes
	}
	return page, rows.Err()
}

func decodeTermReportHistory(id, parentID, observedAt string, requestJSON, summaryJSON []byte) (TermReportHistory, error) {
	var item TermReportHistory
	if len(requestJSON) > report.MaxRequestSummaryJSONBytes || len(summaryJSON) > report.MaxRequestSummaryJSONBytes ||
		json.Unmarshal(requestJSON, &item.Request) != nil || json.Unmarshal(summaryJSON, &item.Summary) != nil {
		return item, errors.New("invalid report history JSON")
	}
	if err := validateTermReportHistory(item); err != nil {
		return item, err
	}
	if item.Summary.ID != id || item.Summary.ParentID != parentID ||
		item.Summary.ObservedAt.UTC().Format(timestampLayout) != observedAt {
		return item, errors.New("report history row does not match receipt")
	}
	return item, nil
}

func exportTermReportHistory(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT id,parent_id,observed_at,request_json,summary_json
		FROM term_report_history ORDER BY observed_at,id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	count := 0
	for rows.Next() {
		var record metadataTermReportHistory
		record.Type = metadataTermReportHistoryType
		if err := rows.Scan(&record.ID, &record.ParentID, &record.ObservedAt,
			&record.RequestJSON, &record.SummaryJSON); err != nil {
			return err
		}
		if _, err := decodeTermReportHistory(record.ID, record.ParentID,
			record.ObservedAt, record.RequestJSON, record.SummaryJSON); err != nil {
			return err
		}
		if err := write(record); err != nil {
			return err
		}
		count++
		if count > termReportHistoryLimit {
			return errors.New("report history exceeds retention limit")
		}
	}
	return rows.Err()
}

func importTermReportHistory(ctx context.Context, tx *sql.Tx, record metadataTermReportHistory) error {
	if record.Type != metadataTermReportHistoryType {
		return errors.New("invalid report history record type")
	}
	if _, err := decodeTermReportHistory(record.ID, record.ParentID, record.ObservedAt,
		record.RequestJSON, record.SummaryJSON); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO term_report_history(
		id,parent_id,observed_at,request_json,summary_json
	) VALUES(?,?,?,?,?)`, record.ID, record.ParentID, record.ObservedAt,
		record.RequestJSON, record.SummaryJSON)
	return err
}
