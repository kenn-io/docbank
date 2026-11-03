package misslog

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json/v2"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

const maxExportPageBytes = 1 << 20

// ExportItem is a provisional, deliberately narrow mapping shared by the eval
// and answer-card exporters. Private notes, paths and capabilities never enter it.
type ExportItem struct {
	SourceRecordID string     `json:"source_record_id"`
	Question       string     `json:"question"`
	Expected       []Identity `json:"expected"`
	EvidenceIDs    []string   `json:"evidence_ids"`
	Scope          string     `json:"scope,omitempty"`
}

// ExportPage bounds both receipt scanning and projected output. Offsets refer
// to live history, not a frozen snapshot.
type ExportPage struct {
	Items      []ExportItem   `json:"items"`
	Skipped    map[string]int `json:"skipped"`
	NextOffset int            `json:"next_offset"`
	Done       bool           `json:"done"`
}

// Export reads one operator-controlled page, even when recording is disabled.
// Read failures fail the page rather than returning a misleading empty export.
func (s *Service) Export(ctx context.Context, format string, limit, offset int) (ExportPage, error) {
	page := ExportPage{Items: []ExportItem{}, Skipped: map[string]int{}, NextOffset: offset}
	if (format != "eval" && format != "cards") || limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return page, ErrInvalid
	}
	if s == nil || s.store == nil {
		return page, ErrPersistence
	}
	rows, more, err := s.store.MissLogExportRecords(ctx, limit, offset)
	if err != nil {
		return page, ErrPersistence
	}
	itemBytes := 2 // JSON array brackets.
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		var receipt Receipt
		if json.Unmarshal([]byte(row.Receipt), &receipt) != nil || receipt.ID != row.ID || receipt.VaultUID != s.store.VaultID() {
			return page, ErrPersistence
		}
		item, reason, err := s.exportItem(ctx, format, receipt)
		if err != nil {
			return page, err
		}
		if reason == "" {
			raw, err := json.Marshal(item)
			if err != nil {
				return page, ErrPersistence
			}
			if len(raw)+3 > maxExportPageBytes { // Array brackets and separator.
				reason = "item_too_large"
			} else {
				if itemBytes+len(raw)+1 > maxExportPageBytes {
					break
				}
				itemBytes += len(raw) + 1 // Include the array separator.
				page.Items = append(page.Items, item)
			}
		}
		if reason != "" {
			page.Skipped[reason]++
		}
		page.NextOffset++
	}
	page.Done = !more && page.NextOffset == offset+len(rows)
	return page, nil
}

func (s *Service) exportItem(ctx context.Context, format string, r Receipt) (ExportItem, string, error) {
	item := ExportItem{SourceRecordID: r.ID, Question: r.Query, Expected: []Identity{}, EvidenceIDs: []string{}}
	if r.Feedback == nil {
		return item, "unlabeled", nil
	}
	if ValidateFeedback(*r.Feedback) != nil ||
		!utf8.ValidString(r.Query) ||
		utf8.RuneCountInString(r.Query) > 32768 ||
		!validHits(r.Hits) {
		return item, "invalid_record", nil
	}
	positive := r.Feedback.Verdict != "none_in_vault"
	if positive && len(r.Feedback.Expected) == 0 {
		return item, "missing_expected", nil
	}
	var fence *exportFence
	if format == "eval" {
		var reason string
		fence, reason = recordedExportFence(r)
		if reason != "" {
			return item, reason, nil
		}
		if positive && fence == nil {
			return item, "immutable_scope_required", nil
		}
		if fence != nil {
			scope, ok := encodeExportFence(fence)
			if !ok {
				return item, "invalid_scope", nil
			}
			item.Scope = scope
		}
	}
	for _, expected := range r.Feedback.Expected {
		node, err := s.store.NodeByID(ctx, expected.NodeID)
		if errors.Is(err, store.ErrNotFound) {
			return item, "identity_unavailable", nil
		}
		if err != nil {
			return item, "", ErrPersistence
		}
		if node.Kind != "file" || node.TrashedAt != nil {
			return item, "identity_unavailable", nil
		}
		if expected.ContentVersionID == "" {
			expected.ContentVersionID = node.CurrentVersionID
		}
		view, err := s.store.ContentVersionViewByID(ctx, expected.NodeID, expected.ContentVersionID)
		if errors.Is(err, store.ErrNotFound) {
			return item, "identity_unavailable", nil
		}
		if err != nil {
			return item, "", ErrPersistence
		}
		if view.Node.TrashedAt != nil || view.Node.CurrentVersionID != view.Version.ID {
			return item, "identity_changed", nil
		}
		if fence != nil && !slices.Contains(fence.ContentVersionIDs, expected.ContentVersionID) {
			return item, "outside_recorded_scope", nil
		}
		item.Expected = append(item.Expected, expected)
		if format == "cards" {
			for _, hit := range r.Hits {
				if hit.NodeID == expected.NodeID && hit.ContentVersionID == expected.ContentVersionID {
					for _, id := range hit.EvidenceIDs {
						if !slices.Contains(item.EvidenceIDs, id) {
							item.EvidenceIDs = append(item.EvidenceIDs, id)
						}
					}
				}
			}
		}
	}
	return item, "", nil
}

type exportFence struct {
	VaultUID          string   `json:"vault_uid"`
	ContentVersionIDs []string `json:"content_version_ids"`
}

func encodeExportFence(fence *exportFence) (string, bool) {
	raw, err := json.Marshal(fence)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func recordedExportFence(r Receipt) (*exportFence, string) {
	rawFence, exists := r.Scope["fence"]
	if !exists {
		return nil, ""
	}
	raw, err := json.Marshal(rawFence)
	if err != nil {
		return nil, "invalid_scope"
	}
	var fence exportFence
	if json.Unmarshal(raw, &fence) != nil || fence.VaultUID != r.VaultUID || fence.ContentVersionIDs == nil || len(fence.ContentVersionIDs) > 4096 {
		return nil, "invalid_scope"
	}
	fp, err := processing.SourceFenceFingerprint(processing.SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: fence.ContentVersionIDs})
	if err != nil || fp != r.FenceFingerprint {
		return nil, "invalid_scope"
	}
	return &fence, ""
}

// WriteEvalCSV emits RFC 4180 record terminators while preserving field bytes.
// csv.Writer's UseCRLF option would also rewrite newlines inside questions.
func WriteEvalCSV(w io.Writer, items []ExportItem) error {
	if _, err := io.WriteString(w, "id,question,expected_ids,scope\r\n"); err != nil {
		return err
	}
	return WriteEvalCSVRows(w, items)
}

// WriteEvalCSVRows appends a page without repeating the header.
func WriteEvalCSVRows(w io.Writer, items []ExportItem) error {
	for _, item := range items {
		ids := make([]string, len(item.Expected))
		for i, id := range item.Expected {
			ids[i] = "node:" + strconv.FormatInt(id.NodeID, 10)
		}
		var row bytes.Buffer
		writer := csv.NewWriter(&row)
		if err := writer.Write([]string{item.SourceRecordID, item.Question, strings.Join(ids, ";"), item.Scope}); err != nil {
			return err
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			return err
		}
		if _, err := w.Write(row.Bytes()[:row.Len()-1]); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// WriteCardsJSONL isolates the provisional card mapping from receipt storage.
func WriteCardsJSONL(w io.Writer, items []ExportItem) error {
	for _, item := range items {
		card := struct {
			Question       string     `json:"question"`
			Expected       []Identity `json:"expected"`
			EvidenceIDs    []string   `json:"evidence_ids"`
			SourceRecordID string     `json:"source_record_id"`
		}{item.Question, item.Expected, item.EvidenceIDs, item.SourceRecordID}
		if err := json.MarshalWrite(w, card); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	return nil
}
