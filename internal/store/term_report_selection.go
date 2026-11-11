package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/report"
)

type TermReportSelectionItem struct {
	Selected         report.Identity  `json:"selected"`
	Current          *report.Identity `json:"current,omitempty"`
	Name             string           `json:"name,omitempty"`
	Path             string           `json:"path,omitempty"`
	SelectedRecorded string           `json:"selected_recorded,omitempty"`
	Modified         string           `json:"modified,omitempty"`
	Status           string           `json:"status"`
}

// RefreshTermReportSelection compares the full selection in one read snapshot.
func (s *Store) RefreshTermReportSelection(ctx context.Context, documents []report.Identity) ([]TermReportSelectionItem, error) {
	selected, err := report.NormalizeSelectedDocuments(documents)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(selected.Documents)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT json_extract(j.value,'$.node_id'), COALESCE(n.name,''), COALESCE(n.kind,''), COALESCE(n.trashed_at,''), COALESCE(cv.version_id,''), COALESCE(cv.blob_hash,''), COALESCE(cv.recorded_at,''), COALESCE(old.recorded_at,'')
		FROM json_each(?) j LEFT JOIN nodes n ON n.id=json_extract(j.value,'$.node_id')
		LEFT JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id
		LEFT JOIN content_versions old ON old.node_id=n.id AND old.version_id=json_extract(j.value,'$.version_id')
		ORDER BY json_extract(j.value,'$.node_id')`, string(encoded))
	if err != nil {
		return nil, err
	}
	result := make([]TermReportSelectionItem, len(selected.Documents))
	var pathIDs []int64
	var pathIndexes []int
	for i := 0; rows.Next(); i++ {
		var nodeID int64
		var name, kind, trashed, version, hash, modified, selectedRecorded string
		if err := rows.Scan(&nodeID, &name, &kind, &trashed, &version, &hash, &modified, &selectedRecorded); err != nil {
			_ = rows.Close()
			return nil, err
		}
		item := TermReportSelectionItem{Selected: selected.Documents[i], Name: name, Modified: modified, SelectedRecorded: selectedRecorded, Status: "missing"}
		if trashed != "" {
			item.Status = "trashed"
		} else if kind == "file" && version != "" {
			current := report.Identity{NodeID: nodeID, VersionID: version, SHA256: hash}
			item.Current = &current
			item.Status = "changed"
			if current == item.Selected {
				item.Status = "current"
			}
		}
		if name != "" && item.Status != "current" {
			pathIDs = append(pathIDs, nodeID)
			pathIndexes = append(pathIndexes, i)
		}
		result[i] = item
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	paths, err := s.pathsOf(ctx, tx, pathIDs)
	if err != nil {
		return nil, err
	}
	for i, path := range paths {
		result[pathIndexes[i]].Path = path
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
