// Package redaction defines the durable contracts used to select and resolve
// production-set redactions.
package redaction

type Box struct {
	Page        int    `json:"page"`
	FrameSHA256 string `json:"frame_sha256"`
	X0          int64  `json:"x0"`
	Y0          int64  `json:"y0"`
	X1          int64  `json:"x1"`
	Y1          int64  `json:"y1"`
}

type Span struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type Unit struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Spans []Span `json:"spans"`
	Boxes []Box  `json:"boxes"`
}

type Atom struct {
	Span  Span  `json:"span"`
	Boxes []Box `json:"boxes"`
}

// Gap records visible page content without accepted text mapping. Anchor is
// its exact reading-order position in the map's global UTF-8 byte stream.
// Unordered is reserved for visible native page objects whose traversal cannot
// be placed in text order; Anchor is then the owning page's start placeholder
// and resolution may retain the gap only by masking the complete page.
type Gap struct {
	Box       Box   `json:"box"`
	Anchor    int64 `json:"anchor"`
	Unordered bool  `json:"unordered,omitzero"`
}

type Page struct {
	Number      int    `json:"number"`
	FrameSHA256 string `json:"frame_sha256"`
	Width       int64  `json:"width"`
	Height      int64  `json:"height"`
	Span        Span   `json:"span"`
}

type TextMap struct {
	Contract       string `json:"contract"`
	SHA256         string `json:"sha256"`
	PDFSHA256      string `json:"pdf_sha256"`
	EvidenceSHA256 string `json:"evidence_sha256"`
	Text           string `json:"text"`
	Pages          []Page `json:"pages"`
	Atoms          []Atom `json:"atoms"`
	Units          []Unit `json:"units"`
	Gaps           []Gap  `json:"gaps"`
}

type Selector struct {
	Kind      string `json:"kind"`
	MapSHA256 string `json:"map_sha256"`
	UnitID    string `json:"unit_id,omitzero"`
	Span      *Span  `json:"span,omitzero"`
	Boxes     []Box  `json:"boxes,omitzero"`
	Pages     []int  `json:"pages,omitzero"`
}

type Decision struct {
	ID        string   `json:"id"`
	MemberID  string   `json:"member_id"`
	Action    string   `json:"action"`
	Reason    string   `json:"reason"`
	Label     string   `json:"label"`
	Uncertain bool     `json:"uncertain"`
	Selector  Selector `json:"selector"`
	Actor     string   `json:"actor,omitzero"`
	CreatedAt string   `json:"created_at,omitzero"`
	Revision  int64    `json:"revision,omitzero"`
}

// FamilyContext records the exact family occurrence selected for a member.
// RootVersionID is the selected family root. RelationOperationID and
// RelationOrder identify an exact retained relationship when Kind is
// email_attachment; they are absent for self-contained occurrences.
type FamilyContext struct {
	Kind                string `json:"kind"`
	RootVersionID       string `json:"root_version_id"`
	RelationOperationID string `json:"relation_operation_id,omitzero"`
	RelationOrder       int    `json:"relation_order,omitzero"`
}

type Member struct {
	ID                  string        `json:"id"`
	VaultID             string        `json:"vault_id"`
	SourceVersionID     string        `json:"source_version_id"`
	SourceSHA256        string        `json:"source_sha256"`
	PDFSHA256           string        `json:"pdf_sha256"`
	NodeID              int64         `json:"node_id"`
	SourceSize          int64         `json:"source_size"`
	PDFSize             int64         `json:"pdf_size"`
	Ordinal             int64         `json:"ordinal"`
	Family              FamilyContext `json:"family"`
	Mode                string        `json:"mode"`
	MapSHA256           string        `json:"map_sha256"`
	PageInventorySHA256 string        `json:"page_inventory_sha256"`
	Reviewed            bool          `json:"reviewed"`
	ReviewBinding       string        `json:"review_binding"`
}

type Run struct {
	Kind                string `json:"kind"`
	Text                string `json:"text"`
	Page                int    `json:"page"`
	Anchor              int64  `json:"anchor"`
	SourceSpan          *Span  `json:"source_span"`
	Boxes               []Box  `json:"boxes"`
	FontSizeMilliPoints int64  `json:"font_size_milli_points"`
}

type RedactionRegion struct {
	Boxes       []Box    `json:"boxes"`
	Label       string   `json:"label"`
	DecisionIDs []string `json:"decision_ids"`
}

type Resolved struct {
	Contract             string            `json:"contract"`
	MapSHA256            string            `json:"map_sha256"`
	RecipeSHA256         string            `json:"recipe_sha256"`
	SHA256               string            `json:"sha256"`
	PageCount            int               `json:"page_count"`
	Pages                []Page            `json:"pages"`
	Gaps                 []Gap             `json:"gaps"`
	RedactBoxes          []Box             `json:"redact_boxes"`
	Removed              []Span            `json:"removed"`
	Runs                 []Run             `json:"runs"`
	UncertainDecisionIDs []string          `json:"uncertain_decision_ids"`
	Regions              []RedactionRegion `json:"regions"`
}

type Recipe struct {
	Contract              string `json:"contract"`
	RendererSHA256        string `json:"renderer_sha256"`
	WriterVersion         string `json:"writer_version"`
	FontSHA256            string `json:"font_sha256"`
	DPI                   int    `json:"dpi"`
	PaddingPixels         int    `json:"padding_pixels"`
	MaxPixels             int64  `json:"max_pixels"`
	MaxAxis               int64  `json:"max_axis"`
	WASMMemoryBytes       int64  `json:"wasm_memory_bytes"`
	PageTimeoutSeconds    int64  `json:"page_timeout_seconds"`
	QualifiedPeakRSSBytes int64  `json:"qualified_peak_rss_bytes"`
	MaxStagingBytes       int64  `json:"max_staging_bytes"`
}

type ReviewInput struct {
	SetID               string `json:"set_id"`
	MemberID            string `json:"member_id"`
	VaultID             string `json:"vault_id"`
	SourceVersionID     string `json:"source_version_id"`
	Revision            int64  `json:"revision"`
	Ordinal             int64  `json:"ordinal"`
	NodeID              int64  `json:"node_id"`
	SourceSize          int64  `json:"source_size"`
	PDFSize             int64  `json:"pdf_size"`
	SourceSHA256        string `json:"source_sha256"`
	PDFSHA256           string `json:"pdf_sha256"`
	PageInventorySHA256 string `json:"page_inventory_sha256"`
	MapSHA256           string `json:"map_sha256"`
	Mode                string `json:"mode"`
	MemberHash          string `json:"member_hash"`
	InstructionsSHA256  string `json:"instructions_sha256"`
	RecipeSHA256        string `json:"recipe_sha256"`
	DecisionsSHA256     string `json:"decisions_sha256"`
	ResolvedSHA256      string `json:"resolved_sha256"`
}

// PolicySelection pins the immutable production policy selected for a draft.
// API commands name an ID and version; the service resolves and stores SHA256
// before the draft can become authority.
type PolicySelection struct {
	PolicyID     string `json:"policy_id"`
	Version      int64  `json:"version"`
	PolicySHA256 string `json:"policy_sha256"`
}

type Endorsement struct {
	Kind                string `json:"kind"`
	Text                string `json:"text"`
	FontSHA256          string `json:"font_sha256"`
	Color               string `json:"color"`
	Box                 Box    `json:"box"`
	FontSizeMilliPoints int64  `json:"font_size_milli_points"`
}

type PageLayout struct {
	Source      Page  `json:"source"`
	Output      Page  `json:"output"`
	StripHeight int64 `json:"strip_height"`
}

type Problem struct { //nolint:errname // Problem is the pinned public contract name.
	Code              string    `json:"code"`
	DecisionIDs       []string  `json:"decision_ids"`
	Expanded          *Selector `json:"expanded"`
	ExpandedMapSHA256 string    `json:"expanded_map_sha256,omitzero"`
	ExpandedSpans     []Span    `json:"expanded_spans,omitzero"`
	ExpandedBoxes     []Box     `json:"expanded_boxes,omitzero"`
}

func (p *Problem) Error() string {
	if p == nil {
		return ""
	}
	return p.Code
}
