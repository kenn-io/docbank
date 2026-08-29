package api

// DocumentQuery is the typed daemon catalog request. Empty values select the
// store-owned defaults; Cursor is always opaque to clients.
type DocumentQuery struct {
	PathPrefix string `json:"path_prefix,omitzero"`
	Sort       string `json:"sort,omitzero" enum:"path,name,modified_at,size,media_type"`
	Direction  string `json:"direction,omitzero" enum:"asc,desc"`
	PageSize   int    `json:"page_size,omitzero" minimum:"1" maximum:"250"`
	Cursor     string `json:"cursor,omitzero"`
}

type DocumentRenditionIdentity struct {
	ProfileFingerprint string `json:"profile_fingerprint" pattern:"^[0-9a-f]{64}$"`
	AttachmentID       string `json:"attachment_id" pattern:"^[0-9a-f]{64}$"`
	BuildID            string `json:"build_id" pattern:"^[0-9a-f]{64}$"`
}

type DocumentSummary struct {
	NodeID                int64                       `json:"node_id" minimum:"1"`
	ContentVersionID      string                      `json:"content_version_id" format:"uuid"`
	Path                  string                      `json:"path"`
	Name                  string                      `json:"name"`
	MediaType             string                      `json:"media_type"`
	Size                  int64                       `json:"size" minimum:"0"`
	ModifiedAt            string                      `json:"modified_at" format:"date-time"`
	LatestProcessingState string                      `json:"latest_processing_state,omitzero"`
	ActiveRenditions      []DocumentRenditionIdentity `json:"active_renditions"`
}

// DocumentIdentity identifies one exact current/live document summary. The
// path fence makes a move observable to callers that need a stable source.
type DocumentIdentity struct {
	NodeID           int64  `json:"node_id" minimum:"1"`
	ContentVersionID string `json:"content_version_id" format:"uuid"`
	Path             string `json:"path"`
}

// DocumentSummaryResolveRequest resolves no more than 100 exact identities
// atomically against the daemon's current/live catalog.
type DocumentSummaryResolveRequest struct {
	Identities []DocumentIdentity `json:"identities" minItems:"1" maxItems:"100"`
}

type DocumentSummaryResolveResponse struct {
	Items []DocumentSummary `json:"items" maxItems:"100"`
}

type DocumentPage struct {
	PathPrefix     string            `json:"path_prefix"`
	Sort           string            `json:"sort" enum:"path,name,modified_at,size,media_type"`
	Direction      string            `json:"direction" enum:"asc,desc"`
	PageSize       int               `json:"page_size" minimum:"1" maximum:"250"`
	Items          []DocumentSummary `json:"items" maxItems:"250"`
	NextCursor     string            `json:"next_cursor,omitzero"`
	PreviousCursor string            `json:"previous_cursor,omitzero"`
}
