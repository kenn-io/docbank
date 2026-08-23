package document

const (
	// SourceEvidenceContractV1 identifies transient provider-neutral evidence.
	SourceEvidenceContractV1 = "source-evidence/v1"
	// NormalizedEvidenceContractV1 identifies durable canonical evidence.
	NormalizedEvidenceContractV1 = "normalized-evidence/v1"
	// EvidenceFixedScale is the fixed-point scale used for normalized confidence.
	EvidenceFixedScale int64 = 1_000_000
)

// EvidenceCompleteness describes whether a source manifest preserves its
// natural document provenance.
type EvidenceCompleteness string

const (
	// EvidenceComplete contains every required source unit and field.
	EvidenceComplete EvidenceCompleteness = "complete"
	// EvidencePartial names omitted source evidence explicitly.
	EvidencePartial EvidenceCompleteness = "partial"
	// EvidenceDegradedProvenance contains readable evidence without natural
	// family-specific provenance.
	EvidenceDegradedProvenance EvidenceCompleteness = "degraded_provenance"
)

// EvidenceUnitKind identifies the natural ordered unit of a document family.
type EvidenceUnitKind string

const (
	EvidenceUnitGeneric EvidenceUnitKind = "generic"
	EvidenceUnitLine    EvidenceUnitKind = "line"
	EvidenceUnitMessage EvidenceUnitKind = "message"
	EvidenceUnitPage    EvidenceUnitKind = "page"
	EvidenceUnitRecord  EvidenceUnitKind = "record"
	EvidenceUnitSection EvidenceUnitKind = "section"
	EvidenceUnitSheet   EvidenceUnitKind = "sheet"
	EvidenceUnitSlide   EvidenceUnitKind = "slide"
	EvidenceUnitSpine   EvidenceUnitKind = "spine"
	EvidenceUnitTime    EvidenceUnitKind = "time_range"
)

// EvidenceLocatorKind identifies how a unit maps back to its source.
type EvidenceLocatorKind string

const (
	EvidenceLocatorGeneric EvidenceLocatorKind = "generic"
	EvidenceLocatorLine    EvidenceLocatorKind = "line"
	EvidenceLocatorMessage EvidenceLocatorKind = "message"
	EvidenceLocatorPage    EvidenceLocatorKind = "page"
	EvidenceLocatorRecord  EvidenceLocatorKind = "record"
	EvidenceLocatorSection EvidenceLocatorKind = "section"
	EvidenceLocatorSheet   EvidenceLocatorKind = "sheet"
	EvidenceLocatorSlide   EvidenceLocatorKind = "slide"
	EvidenceLocatorSpine   EvidenceLocatorKind = "spine"
	EvidenceLocatorTime    EvidenceLocatorKind = "time_range"
)

// EvidenceIndexOrigin records whether integer source locators are zero- or
// one-based. Non-indexed locators use EvidenceIndexOriginNone.
type EvidenceIndexOrigin string

const (
	EvidenceIndexOriginNone EvidenceIndexOrigin = "none"
	EvidenceIndexOriginOne  EvidenceIndexOrigin = "one"
	EvidenceIndexOriginZero EvidenceIndexOrigin = "zero"
)

// EvidenceCoordinateOrigin identifies the origin of fixed-point geometry.
type EvidenceCoordinateOrigin string

const (
	EvidenceCoordinateBottomLeft EvidenceCoordinateOrigin = "bottom_left"
	EvidenceCoordinateTopLeft    EvidenceCoordinateOrigin = "top_left"
)

// EvidenceCoordinateSpace identifies what a geometry frame measures.
type EvidenceCoordinateSpace string

const (
	EvidenceCoordinateImage EvidenceCoordinateSpace = "image"
	EvidenceCoordinatePage  EvidenceCoordinateSpace = "page"
	EvidenceCoordinateUnit  EvidenceCoordinateSpace = "unit"
)

// EvidenceGeometryUnit identifies the fixed-point coordinate unit.
type EvidenceGeometryUnit string

const (
	EvidenceGeometryNormalized EvidenceGeometryUnit = "normalized"
	EvidenceGeometryPixel      EvidenceGeometryUnit = "pixel"
	EvidenceGeometryPoint      EvidenceGeometryUnit = "point"
)

// EvidenceRegionKind identifies one structural region.
type EvidenceRegionKind string

const (
	EvidenceRegionCode      EvidenceRegionKind = "code"
	EvidenceRegionFigure    EvidenceRegionKind = "figure"
	EvidenceRegionFooter    EvidenceRegionKind = "footer"
	EvidenceRegionHeading   EvidenceRegionKind = "heading"
	EvidenceRegionHeader    EvidenceRegionKind = "header"
	EvidenceRegionImage     EvidenceRegionKind = "image"
	EvidenceRegionList      EvidenceRegionKind = "list"
	EvidenceRegionParagraph EvidenceRegionKind = "paragraph"
	EvidenceRegionTable     EvidenceRegionKind = "table"
	EvidenceRegionTableCell EvidenceRegionKind = "table_cell"
)

// EvidenceArtifactRole identifies a bounded retained provider artifact.
type EvidenceArtifactRole string

const (
	EvidenceArtifactImage      EvidenceArtifactRole = "provider_image"
	EvidenceArtifactMarkdown   EvidenceArtifactRole = "provider_markdown"
	EvidenceArtifactStructured EvidenceArtifactRole = "structured_evidence"
	EvidenceArtifactTranscript EvidenceArtifactRole = "provider_transcript"
)

// EvidenceConfidenceInterpretation identifies how a provider score should be
// interpreted without assuming a universal scale.
type EvidenceConfidenceInterpretation string

const (
	EvidenceConfidenceHigherIsBetter EvidenceConfidenceInterpretation = "higher_is_better"
	EvidenceConfidenceLowerIsBetter  EvidenceConfidenceInterpretation = "lower_is_better"
	EvidenceConfidenceProbability    EvidenceConfidenceInterpretation = "probability"
)

// EvidenceOmissionKind identifies the scope of explicitly missing evidence.
type EvidenceOmissionKind string

const (
	EvidenceOmissionField EvidenceOmissionKind = "field"
	EvidenceOmissionRange EvidenceOmissionKind = "range"
	EvidenceOmissionUnit  EvidenceOmissionKind = "unit"
)

// EvidenceTextRangeV1 is a half-open rune range in one unit's text.
type EvidenceTextRangeV1 struct {
	End   int `json:"end"`
	Start int `json:"start"`
}

// EvidenceBoxV1 is one fixed-point axis-aligned box.
type EvidenceBoxV1 struct {
	Bottom int64 `json:"bottom"`
	Left   int64 `json:"left"`
	Right  int64 `json:"right"`
	Top    int64 `json:"top"`
}

// EvidencePointV1 is one fixed-point polygon point.
type EvidencePointV1 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

// EvidencePolygonV1 is one ordered fixed-point polygon.
type EvidencePolygonV1 struct {
	Points []EvidencePointV1 `json:"points"`
}

// SourceEvidenceLocatorV1 contains a provider-reported unit locator.
type SourceEvidenceLocatorV1 struct {
	End         int64               `json:"end"`
	IndexOrigin EvidenceIndexOrigin `json:"index_origin"`
	Kind        EvidenceLocatorKind `json:"kind"`
	Name        string              `json:"name,omitempty"`
	Start       int64               `json:"start"`
}

// SourceEvidenceConfidenceV1 contains a finite provider score and its scale.
type SourceEvidenceConfidenceV1 struct {
	Interpretation EvidenceConfidenceInterpretation `json:"interpretation"`
	Maximum        float64                          `json:"maximum"`
	Minimum        float64                          `json:"minimum"`
	Value          float64                          `json:"value"`
}

// SourceEvidenceGeometryV1 contains bounded fixed-point source geometry.
type SourceEvidenceGeometryV1 struct {
	Boxes            []EvidenceBoxV1          `json:"boxes,omitempty"`
	CoordinateOrigin EvidenceCoordinateOrigin `json:"coordinate_origin"`
	CoordinateSpace  EvidenceCoordinateSpace  `json:"coordinate_space"`
	Height           int64                    `json:"height"`
	Orientation      int64                    `json:"orientation"`
	Polygons         []EvidencePolygonV1      `json:"polygons,omitempty"`
	Scale            int64                    `json:"scale"`
	Unit             EvidenceGeometryUnit     `json:"unit"`
	Width            int64                    `json:"width"`
}

// SourceEvidenceArtifactV1 identifies a provider-local retained artifact.
type SourceEvidenceArtifactV1 struct {
	Pointer    string               `json:"pointer"`
	ProviderID string               `json:"provider_id"`
	Role       EvidenceArtifactRole `json:"role"`
	SHA256     string               `json:"sha256"`
}

// SourceEvidenceOmissionV1 names an exact missing field, range, or unit.
type SourceEvidenceOmissionV1 struct {
	Field     string               `json:"field,omitempty"`
	Kind      EvidenceOmissionKind `json:"kind"`
	Range     *EvidenceTextRangeV1 `json:"range,omitempty"`
	Reason    string               `json:"reason"`
	UnitOrder int                  `json:"unit_order,omitempty"`
}

// SourceEvidenceRegionV1 contains one provider-local structural region.
type SourceEvidenceRegionV1 struct {
	ArtifactProviderID string                      `json:"artifact_provider_id,omitempty"`
	Confidence         *SourceEvidenceConfidenceV1 `json:"confidence,omitempty"`
	Geometry           *SourceEvidenceGeometryV1   `json:"geometry,omitempty"`
	Kind               EvidenceRegionKind          `json:"kind"`
	Order              int                         `json:"order"`
	ParentProviderID   string                      `json:"parent_provider_id,omitempty"`
	ProviderID         string                      `json:"provider_id"`
	TextRange          EvidenceTextRangeV1         `json:"text_range"`
}

// SourceEvidenceTableCellV1 contains one provider-neutral table cell.
type SourceEvidenceTableCellV1 struct {
	Column           int                 `json:"column"`
	ColumnSpan       int                 `json:"column_span"`
	Header           bool                `json:"header"`
	Order            int                 `json:"order"`
	RegionProviderID string              `json:"region_provider_id,omitempty"`
	Row              int                 `json:"row"`
	RowSpan          int                 `json:"row_span"`
	TextRange        EvidenceTextRangeV1 `json:"text_range"`
}

// SourceEvidenceTableV1 contains one provider-local table and its cells.
type SourceEvidenceTableV1 struct {
	Cells            []SourceEvidenceTableCellV1 `json:"cells"`
	Columns          int                         `json:"columns"`
	Order            int                         `json:"order"`
	ProviderID       string                      `json:"provider_id"`
	RegionProviderID string                      `json:"region_provider_id,omitempty"`
	Rows             int                         `json:"rows"`
}

// SourceEvidenceUnitV1 contains one ordered source unit.
type SourceEvidenceUnitV1 struct {
	Confidence  *SourceEvidenceConfidenceV1 `json:"confidence,omitempty"`
	HeadingPath []string                    `json:"heading_path,omitempty"`
	Locator     SourceEvidenceLocatorV1     `json:"locator"`
	Omissions   []SourceEvidenceOmissionV1  `json:"omissions,omitempty"`
	Order       int                         `json:"order"`
	ProviderID  string                      `json:"provider_id,omitempty"`
	Regions     []SourceEvidenceRegionV1    `json:"regions,omitempty"`
	Tables      []SourceEvidenceTableV1     `json:"tables,omitempty"`
	Text        string                      `json:"text"`
}

// SourceEvidenceV1 is transient provider-neutral evidence. Provider IDs are
// used only to resolve relationships and never enter durable identity.
type SourceEvidenceV1 struct {
	Artifacts       []SourceEvidenceArtifactV1 `json:"artifacts,omitempty"`
	Completeness    EvidenceCompleteness       `json:"completeness"`
	ContractVersion string                     `json:"contract_version"`
	Family          string                     `json:"family"`
	Omissions       []SourceEvidenceOmissionV1 `json:"omissions,omitempty"`
	UnitKind        EvidenceUnitKind           `json:"unit_kind"`
	Units           []SourceEvidenceUnitV1     `json:"units"`
}

// EvidenceConfidenceV1 is canonical fixed-point confidence.
type EvidenceConfidenceV1 struct {
	Interpretation EvidenceConfidenceInterpretation `json:"interpretation"`
	Maximum        int64                            `json:"maximum"`
	Minimum        int64                            `json:"minimum"`
	Scale          int64                            `json:"scale"`
	Value          int64                            `json:"value"`
}

// EvidenceGeometryV1 is canonical fixed-point geometry.
type EvidenceGeometryV1 struct {
	Boxes            []EvidenceBoxV1          `json:"boxes,omitempty"`
	CoordinateOrigin EvidenceCoordinateOrigin `json:"coordinate_origin"`
	CoordinateSpace  EvidenceCoordinateSpace  `json:"coordinate_space"`
	Height           int64                    `json:"height"`
	Orientation      int64                    `json:"orientation"`
	Polygons         []EvidencePolygonV1      `json:"polygons,omitempty"`
	Scale            int64                    `json:"scale"`
	Unit             EvidenceGeometryUnit     `json:"unit"`
	Width            int64                    `json:"width"`
}

// EvidenceLocatorV1 is a canonical source locator.
type EvidenceLocatorV1 struct {
	End         int64               `json:"end"`
	IndexOrigin EvidenceIndexOrigin `json:"index_origin"`
	Kind        EvidenceLocatorKind `json:"kind"`
	Name        string              `json:"name,omitempty"`
	Start       int64               `json:"start"`
}

// EvidenceArtifactV1 is a canonical bounded artifact pointer.
type EvidenceArtifactV1 struct {
	ID      string               `json:"id"`
	Pointer string               `json:"pointer"`
	Role    EvidenceArtifactRole `json:"role"`
	SHA256  string               `json:"sha256"`
}

// EvidenceOmissionV1 is a canonical explicit omission.
type EvidenceOmissionV1 struct {
	Field     string               `json:"field,omitempty"`
	Kind      EvidenceOmissionKind `json:"kind"`
	Range     *EvidenceTextRangeV1 `json:"range,omitempty"`
	Reason    string               `json:"reason"`
	UnitOrder int                  `json:"unit_order,omitempty"`
}

// NormalizedEvidenceRegionV1 is one canonical structural region.
type NormalizedEvidenceRegionV1 struct {
	ArtifactID string                `json:"artifact_id,omitempty"`
	Confidence *EvidenceConfidenceV1 `json:"confidence,omitempty"`
	Geometry   *EvidenceGeometryV1   `json:"geometry,omitempty"`
	ID         string                `json:"id"`
	Kind       EvidenceRegionKind    `json:"kind"`
	Order      int                   `json:"order"`
	ParentID   string                `json:"parent_id,omitempty"`
	TextRange  EvidenceTextRangeV1   `json:"text_range"`
}

// NormalizedEvidenceTableCellV1 is one canonical table cell.
type NormalizedEvidenceTableCellV1 struct {
	Column     int                 `json:"column"`
	ColumnSpan int                 `json:"column_span"`
	Header     bool                `json:"header"`
	Order      int                 `json:"order"`
	RegionID   string              `json:"region_id,omitempty"`
	Row        int                 `json:"row"`
	RowSpan    int                 `json:"row_span"`
	TextRange  EvidenceTextRangeV1 `json:"text_range"`
}

// NormalizedEvidenceTableV1 is one canonical table.
type NormalizedEvidenceTableV1 struct {
	Cells    []NormalizedEvidenceTableCellV1 `json:"cells"`
	Columns  int                             `json:"columns"`
	ID       string                          `json:"id"`
	Order    int                             `json:"order"`
	RegionID string                          `json:"region_id,omitempty"`
	Rows     int                             `json:"rows"`
}

// NormalizedEvidenceUnitV1 is one canonical ordered source unit.
type NormalizedEvidenceUnitV1 struct {
	Confidence  *EvidenceConfidenceV1        `json:"confidence,omitempty"`
	HeadingPath []string                     `json:"heading_path,omitempty"`
	ID          string                       `json:"id"`
	Locator     EvidenceLocatorV1            `json:"locator"`
	Omissions   []EvidenceOmissionV1         `json:"omissions,omitempty"`
	Order       int                          `json:"order"`
	Regions     []NormalizedEvidenceRegionV1 `json:"regions,omitempty"`
	Tables      []NormalizedEvidenceTableV1  `json:"tables,omitempty"`
	Text        string                       `json:"text"`
}

// NormalizedEvidenceV1 is durable canonical provider-neutral evidence.
// Checksum is the SHA-256 of the exact canonical JSON returned by
// MarshalNormalizedEvidenceV1 and is intentionally not self-encoded.
type NormalizedEvidenceV1 struct {
	Artifacts       []EvidenceArtifactV1       `json:"artifacts,omitempty"`
	Checksum        string                     `json:"-"`
	Completeness    EvidenceCompleteness       `json:"completeness"`
	ContractVersion string                     `json:"contract_version"`
	Family          string                     `json:"family"`
	Omissions       []EvidenceOmissionV1       `json:"omissions,omitempty"`
	UnitKind        EvidenceUnitKind           `json:"unit_kind"`
	Units           []NormalizedEvidenceUnitV1 `json:"units"`
}

// EvidencePolicy bounds normalized evidence without making provider policy
// part of the public source contract.
type EvidencePolicy struct {
	maxArtifacts      int
	maxCellsPerTable  int
	maxDocumentChars  int
	maxOmissions      int
	maxRegionsPerUnit int
	maxTablesPerUnit  int
	maxUnits          int
}

// NewEvidencePolicy returns the fixed v1 evidence bounds for a document text
// limit.
func NewEvidencePolicy(maxDocumentChars int) (EvidencePolicy, error) {
	policy := EvidencePolicy{
		maxArtifacts:      10_000,
		maxCellsPerTable:  1_000_000,
		maxDocumentChars:  maxDocumentChars,
		maxOmissions:      100_000,
		maxRegionsPerUnit: 1_000_000,
		maxTablesPerUnit:  100_000,
		maxUnits:          100_000,
	}
	if err := policy.validate(); err != nil {
		return EvidencePolicy{}, err
	}
	return policy, nil
}
