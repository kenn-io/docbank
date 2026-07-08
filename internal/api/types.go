package api

import "go.kenn.io/docbank/internal/store"

// Node is the wire representation of a store.Node. Path is only populated on
// single-node responses; list responses omit it.
type Node struct {
	ID         int64  `json:"id"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind" enum:"dir,file"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type,omitempty"`
	Revision   int64  `json:"revision"`
	CreatedAt  string `json:"created_at"`
	ModifiedAt string `json:"modified_at"`
	TrashedAt  string `json:"trashed_at,omitempty"`
	Path       string `json:"path,omitempty"` // set on single-node responses only
}

// SearchHit pairs a matched node with its display path.
type SearchHit struct {
	Node Node   `json:"node"`
	Path string `json:"path"`
}

func fromStoreNode(n store.Node) Node {
	out := Node{
		ID: n.ID, ParentID: n.ParentID, Name: n.Name, Kind: n.Kind,
		Size: n.Size, MimeType: n.MimeType, Revision: n.Revision,
		CreatedAt: n.CreatedAt, ModifiedAt: n.ModifiedAt,
	}
	if n.TrashedAt != nil {
		out.TrashedAt = *n.TrashedAt
	}
	return out
}
