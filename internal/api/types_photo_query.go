package api

// PhotoBrowseRequest carries strict QueryV1 JSON and live-page options.
type PhotoBrowseRequest struct {
	SetID    string                 `json:"set_id,omitzero" format:"uuid"`
	Query    QueryPayload           `json:"query"`
	Coverage WorkspaceQueryCoverage `json:"coverage,omitzero"`
	PageSize int                    `json:"page_size,omitempty,omitzero" minimum:"1" maximum:"250" default:"50"`
	Cursor   string                 `json:"cursor,omitempty" maxLength:"32768"`
}

type PhotoPreviewSlot struct {
	State        string `json:"state" enum:"missing,ready,unsupported,failed"`
	GenerationID string `json:"generation_id,omitempty" pattern:"^[0-9a-f]{64}$"`
	URL          string `json:"url,omitempty"`
	SHA256       string `json:"sha256,omitempty" pattern:"^[0-9a-f]{64}$"`
	Size         *int64 `json:"size,omitempty" minimum:"1"`
	MediaType    string `json:"media_type,omitempty" enum:"image/jpeg"`
	Width        *int   `json:"width,omitempty" minimum:"1"`
	Height       *int   `json:"height,omitempty" minimum:"1"`
}

type PhotoPreviewSlots struct {
	Grid  PhotoPreviewSlot `json:"grid"`
	Fit   PhotoPreviewSlot `json:"fit"`
	Large PhotoPreviewSlot `json:"large"`
}

type PhotoBrowseRow struct {
	AssetID              string            `json:"asset_id" format:"uuid"`
	Kind                 string            `json:"kind" enum:"photo,video"`
	Revision             int64             `json:"revision" minimum:"1"`
	DisplayFileID        string            `json:"display_file_id" format:"uuid"`
	NodeID               int64             `json:"node_id" minimum:"1"`
	ContentVersionID     string            `json:"content_version_id" format:"uuid"`
	Name                 string            `json:"name"`
	MediaType            string            `json:"media_type"`
	ImportTime           string            `json:"import_time"`
	CaptureTime          *string           `json:"capture_time" nullable:"true"`
	CaptureTimePrecision *string           `json:"capture_time_precision" nullable:"true"`
	CaptureTimeTimezone  *string           `json:"capture_time_timezone" nullable:"true"`
	CaptureTimeOffset    *string           `json:"capture_time_offset" nullable:"true"`
	WidthPX              *int64            `json:"width_px" nullable:"true"`
	HeightPX             *int64            `json:"height_px" nullable:"true"`
	Previews             PhotoPreviewSlots `json:"previews"`
}

type PhotoBrowsePage struct {
	Items      []PhotoBrowseRow `json:"items"`
	Total      int64            `json:"total" minimum:"0"`
	NextCursor string           `json:"next_cursor,omitempty"`
}
