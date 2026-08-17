package document

// SourceDocument contains provider-neutral evidence in source order.
type SourceDocument struct {
	Family   string
	UnitKind string
	Units    []SourceUnit
}

// SourceUnit contains the transient evidence for one source unit.
type SourceUnit struct {
	Index      int
	Markdown   string
	Header     string
	Footer     string
	Dimensions UnitDimensions
}

// UnitDimensions records source dimensions when the provider reports them.
type UnitDimensions struct {
	DPI    int
	Height int
	Width  int
}

// NormalizedDocument is deterministic, inert document evidence.
type NormalizedDocument struct {
	PolicyVersion int
	Family        string
	UnitKind      string
	Units         []NormalizedUnit
	Chunks        []Chunk
	Checksum      string
	Truncated     bool
}

// NormalizedUnit is one canonical source unit and its provenance.
type NormalizedUnit struct {
	Index        int
	SourceKey    string
	Kind         string
	Text         string
	Header       string
	Footer       string
	Dimensions   UnitDimensions
	CharCount    int
	Checksum     string
	Truncated    bool
	HeadingMarks []HeadingMark
}

// HeadingMark records the active heading path at a character offset.
type HeadingMark struct {
	CharOffset int
	Path       []string
}

// Chunk is a stable publication and provenance unit.
type Chunk struct {
	Key         string
	Ordinal     int
	Text        string
	HeadingPath []string
	CharCount   int
	Checksum    string
	Truncated   bool
	Spans       []ChunkSpan
}

// ChunkSpan maps a chunk range to its normalized source unit.
type ChunkSpan struct {
	UnitIndex int
	CharStart int
	CharEnd   int
}
