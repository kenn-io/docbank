package api

// NoteCoverageSelection requests optional existing processing coverage.
type NoteCoverageSelection struct {
	Profile   string `json:"profile"`
	BindingID string `json:"binding_id,omitzero"`
}

type NoteProjectionReadiness struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitzero"`
	Recipe     string `json:"recipe,omitzero"`
	Generation string `json:"generation,omitzero"`
}

type NoteReadiness struct {
	Publisher          NotePublicationStatus   `json:"publisher"`
	Parse              NoteProjectionReadiness `json:"parse"`
	Graph              NoteProjectionReadiness `json:"graph"`
	CoverageSelection  *NoteCoverageSelection  `json:"coverage_selection,omitzero"`
	Coverage           *CoverageReport         `json:"coverage,omitzero"`
	CoverageObservedAt string                  `json:"coverage_observed_at,omitzero"`
}

type NoteSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type NoteContextRequest struct {
	Publication      string                 `json:"publication"`
	SourceRef        string                 `json:"source_ref"`
	ContentVersionID string                 `json:"content_version_id,omitzero"`
	Direction        string                 `json:"direction"`
	Cursor           string                 `json:"cursor,omitzero"`
	Limit            int                    `json:"limit,omitzero"`
	Coverage         *NoteCoverageSelection `json:"coverage,omitzero"`
}

type NoteMetadata struct {
	Title          string   `json:"title"`
	Aliases        []string `json:"aliases"`
	Tags           []string `json:"tags"`
	AliasesOmitted int      `json:"aliases_omitted"`
	TagsOmitted    int      `json:"tags_omitted"`
}

type NoteDiagnostic struct {
	Code string   `json:"code"`
	Span NoteSpan `json:"span"`
}

type NoteEdge struct {
	Key             string                 `json:"key"`
	Form            string                 `json:"form"`
	State           string                 `json:"state"`
	RawTarget       string                 `json:"raw_target"`
	Target          string                 `json:"target"`
	Label           string                 `json:"label"`
	Embed           bool                   `json:"embed"`
	LabelTruncated  bool                   `json:"label_truncated"`
	Source          PublishedNoteIdentity  `json:"source"`
	Span            NoteSpan               `json:"span"`
	DestinationSpan NoteSpan               `json:"destination_span"`
	TargetIdentity  *PublishedNoteIdentity `json:"target_identity,omitzero"`
	TargetHeading   *NoteSpan              `json:"target_heading,omitzero"`
}

type NoteContextPage struct {
	Source          PublishedNoteIdentity `json:"source"`
	ArchiveHead     *NoteArchiveHead      `json:"archive_head,omitzero"`
	Metadata        NoteMetadata          `json:"metadata"`
	DisplayTitle    string                `json:"display_title"`
	Diagnostics     []NoteDiagnostic      `json:"diagnostics"`
	Readiness       NoteReadiness         `json:"readiness"`
	Items           []NoteEdge            `json:"items"`
	NextCursor      string                `json:"next_cursor,omitzero"`
	TruncatedFields []string              `json:"truncated_fields"`
}
