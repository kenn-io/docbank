package store

import (
	"context"
	"fmt"

	"go.kenn.io/docbank/document"
)

// Audit history cannot yet record or replay document validity declarations.
func requireNoValidityDeclarations(ctx context.Context, q metadataQuerier) error {
	var present bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM node_supersessions) OR EXISTS(SELECT 1 FROM node_effective_windows)`).Scan(&present); err != nil {
		return fmt.Errorf("checking document validity declarations: %w", err)
	}
	if present {
		return fmt.Errorf("document validity declarations cannot coexist with audit authority: %w", ErrAuditMutationUnsupported)
	}
	return nil
}

type metadataSupersession struct {
	Type          string `json:"type"`
	SuccessorID   int64  `json:"successor_node_id" db:"successor_node_id"`
	PredecessorID int64  `json:"predecessor_node_id" db:"predecessor_node_id"`
	Note          string `json:"note" db:"note"`
	RecordedAt    string `json:"recorded_at" db:"recorded_at"`
}
type metadataEffectiveWindow struct {
	Type   string  `json:"type"`
	NodeID int64   `json:"node_id" db:"node_id"`
	From   *string `json:"effective_from" db:"effective_from"`
	To     *string `json:"effective_to" db:"effective_to"`
}

var supersessionMetadata = newMetadataTable(metadataTable[metadataSupersession]{record: metadataSupersession{Type: "node_supersession"}, table: "node_supersessions", suffix: "ORDER BY predecessor_node_id, successor_node_id", validate: func(v metadataSupersession) error {
	if v.SuccessorID <= 0 || v.PredecessorID <= 0 {
		return ErrInvalidSupersession
	}
	if err := validSupersessionNote(v.Note); err != nil {
		return err
	}
	return validateMetadataTime("supersession recorded_at", v.RecordedAt)
}})
var effectiveWindowMetadata = newMetadataTable(metadataTable[metadataEffectiveWindow]{record: metadataEffectiveWindow{Type: "node_effective_window"}, table: "node_effective_windows", suffix: "ORDER BY node_id", validate: func(v metadataEffectiveWindow) error {
	if v.NodeID <= 0 {
		return ErrInvalidSupersession
	}
	w := document.EffectiveWindow{}
	if v.From != nil {
		w.From = *v.From
		if w.From == "" {
			return ErrInvalidSupersession
		}
	}
	if v.To != nil {
		w.To = *v.To
		if w.To == "" {
			return ErrInvalidSupersession
		}
	}
	if w.From == "" && w.To == "" {
		return ErrInvalidSupersession
	}
	return document.ValidateEffectiveWindow(w)
}})

func validateSupersessionMetadata(ctx context.Context, q metadataQuerier) error {
	for _, codec := range []metadataRecordCodec{supersessionMetadata, effectiveWindowMetadata} {
		if err := codec.validateRows(ctx, q); err != nil {
			return err
		}
	}
	var invalid int
	if err := q.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM node_supersessions s JOIN nodes a ON a.id=s.predecessor_node_id JOIN nodes b ON b.id=s.successor_node_id WHERE a.kind<>'file' OR b.kind<>'file') + (SELECT COUNT(*) FROM node_effective_windows w JOIN nodes n ON n.id=w.node_id WHERE n.kind<>'file')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("invalid supersession endpoints: %w", ErrInvalidSupersession)
	}
	var ids []int64
	err := func() error {
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT predecessor_node_id FROM node_supersessions ORDER BY predecessor_node_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return nil
	}()
	if err != nil {
		return err
	}
	visited := make(map[int64]bool)
	for _, id := range ids {
		if visited[id] {
			continue
		}
		if err = validateSupersessionComponent(ctx, q, id, visited); err != nil {
			return err
		}
	}
	return nil
}
