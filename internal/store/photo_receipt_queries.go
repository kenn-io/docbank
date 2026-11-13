package store

import (
	"bytes"
	"encoding/json/v2"
)

// photoReceiptQueryJSON contains only the fields used by SQL. Raw before/after
// JSON remains the portable evidence. PostgreSQL rejects NUL even in unrelated
// strings when its JSON operators decode a whole receipt.
func photoReceiptQueryJSON(operation, after string) (string, error) {
	if operation != "xmp_sync" && operation != "authored" && operation != "authored_undo" && operation != "authored_sidecar" && operation != "checkout_commit" {
		return "{}", nil
	}
	type node struct {
		FileID string `json:"file_id,omitzero"`
		NodeID int64  `json:"node_id,omitzero"`
	}
	type sidecar struct {
		FileID    string `json:"file_id,omitzero"`
		NodeID    int64  `json:"node_id,omitzero"`
		VersionID string `json:"version_id,omitzero"`
	}
	type representative struct {
		AssetID string `json:"asset_id"`
		Before  *int64 `json:"before,omitzero"`
		After   *int64 `json:"after,omitzero"`
	}
	type outside struct {
		Path      string `json:"path"`
		Digest    string `json:"digest"`
		VersionID string `json:"version_id,omitzero"`
	}
	var source struct {
		FileID          string              `json:"file_id"`
		NodeID          int64               `json:"node_id"`
		Token           string              `json:"token"`
		Conflicts       PhotoAuthoredFields `json:"conflicts"`
		CheckoutID      string              `json:"checkout_id"`
		OperationID     string              `json:"operation_id"`
		UndoOf          string              `json:"undo_of"`
		Sidecar         *sidecar            `json:"sidecar"`
		Representatives []representative    `json:"representatives,omitzero"`
		After           []node              `json:"after"`
		Sync            *struct {
			Sidecar sidecar `json:"sidecar"`
			Settled struct {
				VersionID string `json:"version_id"`
			} `json:"settled"`
			WrittenVersion string    `json:"written_version"`
			Outside        []outside `json:"outside"`
		} `json:"sync"`
	}
	// Most asset receipts encode arrays. None of their fields are queried in SQL.
	rawAfter := bytes.TrimSpace([]byte(after))
	if len(rawAfter) == 0 || rawAfter[0] != '{' {
		return "{}", nil
	}
	if err := json.Unmarshal(rawAfter, &source); err != nil {
		return "", err
	}
	type outsideQuery struct {
		Path      []byte `json:"path"`
		Digest    string `json:"digest"`
		VersionID string `json:"version_id,omitzero"`
	}
	query := struct {
		FileID          string              `json:"file_id,omitzero"`
		NodeID          int64               `json:"node_id,omitzero"`
		Token           []byte              `json:"token"`
		Conflicts       PhotoAuthoredFields `json:"conflicts,omitzero"`
		CheckoutID      string              `json:"checkout_id,omitzero"`
		OperationID     string              `json:"operation_id,omitzero"`
		UndoOf          string              `json:"undo_of,omitzero"`
		Sidecar         *sidecar            `json:"sidecar,omitzero"`
		Representatives []representative    `json:"representatives,omitzero"`
		After           []node              `json:"after,omitzero"`
		Sync            any                 `json:"sync,omitzero"`
	}{FileID: source.FileID, NodeID: source.NodeID, Token: []byte(source.Token), Conflicts: source.Conflicts, CheckoutID: source.CheckoutID, OperationID: source.OperationID, UndoOf: source.UndoOf, Sidecar: source.Sidecar, After: source.After, Representatives: source.Representatives}
	if source.Sync != nil {
		entries := make([]outsideQuery, len(source.Sync.Outside))
		for i, v := range source.Sync.Outside {
			entries[i] = outsideQuery{[]byte(v.Path), v.Digest, v.VersionID}
		}
		query.Sync = struct {
			Sidecar        sidecar        `json:"sidecar"`
			Settled        any            `json:"settled"`
			WrittenVersion string         `json:"written_version,omitzero"`
			Outside        []outsideQuery `json:"outside"`
		}{source.Sync.Sidecar, source.Sync.Settled, source.Sync.WrittenVersion, entries}
	}
	raw, err := json.Marshal(query)
	return string(raw), err
}
