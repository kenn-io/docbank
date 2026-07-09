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

// IngestFailure records one source path that failed to import.
type IngestFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// IngestReport summarizes an ingest run.
type IngestReport struct {
	Added   int             `json:"added"`
	Skipped int             `json:"skipped"`
	Failed  []IngestFailure `json:"failed,omitempty"`
}

// GCReport summarizes a gc dry run (Run=false) or an actual reclaim.
type GCReport struct {
	CandidateBlobs   int   `json:"candidate_blobs"`
	UntrackedFiles   int   `json:"untracked_files"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	Removed          int   `json:"removed"`
	Run              bool  `json:"run"`
}

// VerifyProblem flags one blob whose content didn't check out.
type VerifyProblem struct {
	Hash    string `json:"hash"`
	Problem string `json:"problem" enum:"missing,corrupt,unreadable"`
}

// VerifyReport summarizes a full blob verification pass.
type VerifyReport struct {
	OK       int             `json:"ok"`
	Problems []VerifyProblem `json:"problems,omitempty"`
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
