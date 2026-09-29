package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/report"
)

// TermReportHistory retains only a reusable request and run receipt. Report
// artifacts and date-evidence pages are deliberately excluded.
type TermReportHistory struct {
	Request         report.Request    `json:"request"`
	Summary         report.Summary    `json:"summary"`
	PhotoOwnerID    string            `json:"-"`
	PhotoOwnerBound bool              `json:"-"`
	PhotoNoOwner    bool              `json:"-"`
	Members         []report.Identity `json:"-"`
}

type TermReportHistoryPage struct {
	Items []TermReportHistory `json:"items"`
	Total int                 `json:"total"`
}

const metadataTermReportHistoryType = "term_report_history"

type metadataTermReportHistory struct {
	Type            string `json:"type"`
	ID              string `json:"id"`
	ParentID        string `json:"parent_id"`
	ObservedAt      string `json:"observed_at"`
	RequestJSON     []byte `json:"request_json" format:"byte"`
	SummaryJSON     []byte `json:"summary_json" format:"byte"`
	PhotoOwnerID    string `json:"photo_owner_id"`
	PhotoOwnerBound bool   `json:"photo_owner_bound"`
	PhotoNoOwner    bool   `json:"photo_no_owner"`
	MembersJSON     []byte `json:"members_json" format:"byte"`
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
	return validateTermReportHistoryAuthority(item)
}

func validateTermReportHistoryAuthority(item TermReportHistory) error {
	if !item.PhotoOwnerBound {
		if item.PhotoOwnerID != "" || item.PhotoNoOwner || len(item.Members) != 0 {
			return errors.New("unbound report history has owner authority")
		}
		return nil
	}
	if item.PhotoNoOwner {
		if item.PhotoOwnerID != "" {
			return errors.New("ownerless report history has an owner ID")
		}
	} else if item.PhotoOwnerID == "" || validateUUIDv4(item.PhotoOwnerID) != nil {
		return errors.New("bound report history has an invalid owner ID")
	}
	if len(item.Members) > 50000 {
		return errors.New("report history has too many members")
	}
	for _, member := range item.Members {
		if member.NodeID <= 0 || member.VersionID == "" || !canonical.IsSHA256Hex(member.SHA256) {
			return errors.New("report history has an invalid member identity")
		}
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
	membersJSON, err := json.Marshal(item.Members)
	if err != nil {
		return err
	}
	if len(requestJSON) > report.MaxRequestSummaryJSONBytes || len(summaryJSON) > report.MaxRequestSummaryJSONBytes ||
		len(membersJSON) > report.MaxRequestSummaryJSONBytes {
		return errors.New("report history receipt exceeds limit")
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		visibilityCtx := WithPhotoOwnerBinding(ctx, item.PhotoOwnerID, item.PhotoOwnerBound, item.PhotoNoOwner)
		if item.PhotoOwnerBound {
			for _, member := range item.Members {
				if err := photoVersionVisibilityCheckTx(visibilityCtx, tx, member.VersionID); err != nil {
					return fmt.Errorf("saving report history member %s: %w", member.VersionID, err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO term_report_history(
			id,parent_id,observed_at,request_json,summary_json,
			photo_owner_id,photo_owner_bound,photo_no_owner,members_json
		) VALUES(?,?,?,?,?,?,?,?,?)`, item.Summary.ID, item.Summary.ParentID,
			item.Summary.ObservedAt.UTC().Format(timestampLayout), requestJSON, summaryJSON,
			item.PhotoOwnerID, item.PhotoOwnerBound, item.PhotoNoOwner, membersJSON); err != nil {
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
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TermReportHistoryPage{}, fmt.Errorf("beginning report history read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = func() error {
		ownerID, bound, noOwner, err := photoOwnerBindingTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,parent_id,observed_at,request_json,summary_json,
			photo_owner_id,photo_owner_bound,photo_no_owner,members_json
			FROM term_report_history ORDER BY observed_at DESC,id DESC`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		page.Items = make([]TermReportHistory, 0, limit)
		pageBytes := 0
		eligible := 0
		for rows.Next() {
			var id, parentID, observedAt, rowOwnerID string
			var requestJSON, summaryJSON, membersJSON []byte
			var rowBound, rowNoOwner bool
			if err := rows.Scan(&id, &parentID, &observedAt, &requestJSON, &summaryJSON,
				&rowOwnerID, &rowBound, &rowNoOwner, &membersJSON); err != nil {
				return err
			}
			if bound && (!rowBound || rowNoOwner != noOwner || !noOwner && rowOwnerID != ownerID) {
				continue
			}
			item, err := decodeTermReportHistory(id, parentID, observedAt, requestJSON, summaryJSON,
				rowOwnerID, rowBound, rowNoOwner, membersJSON)
			if err != nil {
				return err
			}
			if bound {
				visibilityCtx := WithPhotoOwnerBinding(ctx, ownerID, true, noOwner)
				visible := true
				for _, member := range item.Members {
					if err := photoVersionVisibilityCheckTx(visibilityCtx, tx, member.VersionID); err != nil {
						if errors.Is(err, ErrNotFound) {
							visible = false
							break
						}
						return err
					}
				}
				if !visible {
					continue
				}
			}
			eligible++
			if eligible <= offset || len(page.Items) >= limit {
				continue
			}
			itemBytes := len(requestJSON) + len(summaryJSON) + len(membersJSON)
			if len(page.Items) != 0 && pageBytes+itemBytes > maxTermReportHistoryPage {
				continue
			}
			page.Items = append(page.Items, item)
			pageBytes += itemBytes
		}
		if err := rows.Err(); err != nil {
			return err
		}
		page.Total = eligible
		return nil
	}()
	if err != nil {
		return TermReportHistoryPage{}, err
	}
	if err := tx.Commit(); err != nil {
		return TermReportHistoryPage{}, fmt.Errorf("committing report history read: %w", err)
	}
	return page, nil
}

func decodeTermReportHistory(id, parentID, observedAt string, requestJSON, summaryJSON []byte,
	photoOwnerID string, photoOwnerBound, photoNoOwner bool, membersJSON []byte,
) (TermReportHistory, error) {
	var item TermReportHistory
	if len(requestJSON) > report.MaxRequestSummaryJSONBytes || len(summaryJSON) > report.MaxRequestSummaryJSONBytes ||
		len(membersJSON) > report.MaxRequestSummaryJSONBytes || json.Unmarshal(requestJSON, &item.Request) != nil ||
		json.Unmarshal(summaryJSON, &item.Summary) != nil || json.Unmarshal(membersJSON, &item.Members) != nil {
		return item, errors.New("invalid report history JSON")
	}
	item.PhotoOwnerID = photoOwnerID
	item.PhotoOwnerBound = photoOwnerBound
	item.PhotoNoOwner = photoNoOwner
	if err := validateTermReportHistory(item); err != nil {
		return item, err
	}
	if len(item.Members) == 0 {
		item.Members = nil
	}
	if item.Summary.ID != id || item.Summary.ParentID != parentID ||
		item.Summary.ObservedAt.UTC().Format(timestampLayout) != observedAt {
		return item, errors.New("report history row does not match receipt")
	}
	return item, nil
}

func exportTermReportHistory(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	rows, err := q.QueryContext(ctx, `SELECT id,parent_id,observed_at,request_json,summary_json,
		photo_owner_id,photo_owner_bound,photo_no_owner,members_json
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
			&record.RequestJSON, &record.SummaryJSON, &record.PhotoOwnerID, &record.PhotoOwnerBound,
			&record.PhotoNoOwner, &record.MembersJSON); err != nil {
			return err
		}
		if _, err := decodeTermReportHistory(record.ID, record.ParentID,
			record.ObservedAt, record.RequestJSON, record.SummaryJSON, record.PhotoOwnerID,
			record.PhotoOwnerBound, record.PhotoNoOwner, record.MembersJSON); err != nil {
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
		record.RequestJSON, record.SummaryJSON, record.PhotoOwnerID, record.PhotoOwnerBound,
		record.PhotoNoOwner, record.MembersJSON); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO term_report_history(
		id,parent_id,observed_at,request_json,summary_json,
		photo_owner_id,photo_owner_bound,photo_no_owner,members_json
	) VALUES(?,?,?,?,?,?,?,?,?)`, record.ID, record.ParentID, record.ObservedAt,
		record.RequestJSON, record.SummaryJSON, record.PhotoOwnerID, record.PhotoOwnerBound,
		record.PhotoNoOwner, record.MembersJSON)
	return err
}
