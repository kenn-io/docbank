package store

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/docbank/report"
)

type TermReportSelectionItem struct {
	Selected report.Identity  `json:"selected"`
	Current  *report.Identity `json:"current,omitempty"`
	Name     string           `json:"name,omitempty"`
	Modified string           `json:"modified,omitempty"`
	Status   string           `json:"status"`
}

// RefreshTermReportSelection compares the full selection in one read snapshot.
func (s *Store) RefreshTermReportSelection(ctx context.Context, documents []report.Identity) ([]TermReportSelectionItem, error) {
	selected, err := report.NormalizeSelectedDocuments(documents)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(selected.Documents))
	for i, id := range selected.Documents {
		ids[i] = id.NodeID
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	result := make([]TermReportSelectionItem, len(ids))
	err = s.withLexicalGenerationRead(ctx, func(q metadataQuerier, _ LexicalGeneration) error {
		rows, err := q.QueryContext(ctx, `SELECT j.value, COALESCE(n.name,''), COALESCE(n.kind,''), COALESCE(n.trashed_at,''), COALESCE(cv.version_id,''), COALESCE(cv.blob_hash,''), COALESCE(cv.recorded_at,'')
   FROM json_each(?) j LEFT JOIN nodes n ON n.id=j.value
   LEFT JOIN content_versions cv ON cv.node_id=n.id AND cv.version_id=n.current_version_id ORDER BY j.value`, string(encoded))
		if err != nil {
			return err
		}
		defer rows.Close()
		i := 0
		for rows.Next() {
			var nodeID int64
			var name, kind, trashed, version, hash, modified string
			if err := rows.Scan(&nodeID, &name, &kind, &trashed, &version, &hash, &modified); err != nil {
				return err
			}
			item := TermReportSelectionItem{Selected: selected.Documents[i], Name: name, Modified: modified, Status: "missing"}
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
			result[i] = item
			i++
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	return result, err
}
