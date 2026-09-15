package api

type DocumentSourceFenceFilters struct {
	TagID          string `json:"tag_id,omitzero" format:"uuid"`
	MIMEType       string `json:"mime_type,omitzero" maxLength:"255"`
	UnderNodeID    int64  `json:"under_node_id,omitzero" minimum:"1"`
	ModifiedSince  string `json:"modified_since,omitzero" format:"date-time"`
	ModifiedBefore string `json:"modified_before,omitzero" format:"date-time"`
}

type DocumentSourceFenceResolveRequest struct {
	ContentVersionIDs []string                    `json:"content_version_ids,omitzero"`
	Filters           *DocumentSourceFenceFilters `json:"filters,omitzero"`
}

// ResolvedDocumentSourceFence allows an exact empty population. Search
// requests continue to use DocumentSourceFence, whose ID array is non-empty.
type ResolvedDocumentSourceFence struct {
	VaultUID          string   `json:"vault_uid" format:"uuid"`
	ContentVersionIDs []string `json:"content_version_ids" maxItems:"4096" uniqueItems:"true"`
}

type DocumentSourceFenceResolution struct {
	Fence              ResolvedDocumentSourceFence `json:"fence"`
	FenceFingerprint   string                      `json:"fence_fingerprint" pattern:"^sha256:[0-9a-f]{64}$"`
	ObservedScopeCount int                         `json:"observed_scope_count" minimum:"0"`
}

// RenditionWindowRequest binds a bounded Unicode read to one exact current,
// live sanitized-Markdown rendition tuple.
type RenditionWindowRequest struct {
	VaultID          string `json:"vault_id" format:"uuid"`
	NodeID           int64  `json:"node_id" minimum:"1"`
	ContentVersionID string `json:"content_version_id" format:"uuid"`
	AttachmentID     string `json:"attachment_id" pattern:"^[0-9a-f]{64}$"`
	Offset           int    `json:"offset,omitzero" minimum:"0" maximum:"2147483647"`
	MaxChars         int    `json:"max_chars" minimum:"1" maximum:"16000"`
}

type RenditionTextWindow struct {
	VaultID            string `json:"vault_id" format:"uuid"`
	NodeID             int64  `json:"node_id" minimum:"1"`
	ContentVersionID   string `json:"content_version_id" format:"uuid"`
	AttachmentID       string `json:"attachment_id" pattern:"^[0-9a-f]{64}$"`
	BuildID            string `json:"build_id" pattern:"^[0-9a-f]{64}$"`
	ProfileFingerprint string `json:"profile_fingerprint" pattern:"^[0-9a-f]{64}$"`
	Text               string `json:"text" maxLength:"16000"`
	MediaType          string `json:"media_type" enum:"text/markdown"`
	Checksum           string `json:"checksum" pattern:"^[0-9a-f]{64}$"`
	RequestedOffset    int    `json:"requested_offset" minimum:"0" maximum:"2147483647"`
	ActualStart        int    `json:"actual_start" minimum:"0" maximum:"2147483647"`
	ActualEnd          int    `json:"actual_end" minimum:"0" maximum:"2147483647"`
	NextOffset         int    `json:"next_offset" minimum:"0" maximum:"2147483647"`
	EOF                bool   `json:"eof"`
	ResponseBytes      int    `json:"response_bytes" minimum:"0" maximum:"1048576"`
}
