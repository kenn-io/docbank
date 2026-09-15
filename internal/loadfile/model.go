// Package loadfile decodes legal load-file formats into an ordered,
// provider-independent intermediate model.
package loadfile

const diagnosticSeverityBlocking = "blocking"
const maxDiagnosticsPerOperation = 100_000

func appendDiagnosticBounded(diagnostics *[]Diagnostic, diagnostic Diagnostic) error {
	if len(*diagnostics) == maxDiagnosticsPerOperation {
		return ErrLoadfileLimit
	}
	*diagnostics = append(*diagnostics, diagnostic)
	return nil
}

type Package struct {
	Profile     Profile
	Volumes     []Volume
	Records     []Record
	Images      []ImageRef
	Diagnostics []Diagnostic
}

type Record struct {
	RowID       string    `json:"row_id"`
	DocID       string    `json:"doc_id"`
	LoadFile    string    `json:"load_file"`
	RowOrdinal  int       `json:"row_ordinal"`
	ColumnOrder []string  `json:"column_order"`
	Fields      []Field   `json:"fields"`
	Files       []FileRef `json:"files"`
	Family      Family    `json:"family"`
}

type Field struct {
	Column    string `json:"column"`
	Ordinal   int    `json:"ordinal"`
	Canonical string `json:"canonical"`
	Raw       string `json:"raw"`
	Value     Value  `json:"value"`
}

type FileRef struct {
	Role     string `json:"role"`
	Volume   string `json:"volume"`
	RelPath  string `json:"rel_path"`
	Declared string `json:"declared"`
	SHA256   string `json:"sha256"`
	Status   string `json:"status"`
	Size     int64  `json:"size"`
}

type ImageRef struct {
	ImageKey          string `json:"image_key"`
	Volume            string `json:"volume"`
	RelPath           string `json:"rel_path"`
	DocumentBreak     bool   `json:"document_break"`
	FolderBreak       bool   `json:"folder_break"`
	BoxBreak          bool   `json:"box_break"`
	PageOrdinal       int    `json:"page_ordinal"`
	SourcePage        int    `json:"source_page"`
	DeclaredPageCount int    `json:"declared_page_count"`
	Boundary          string `json:"boundary"`
	Rotation          int    `json:"rotation"`
}

type Family struct {
	ParentDocID      string   `json:"parent_doc_id"`
	AttachmentDocIDs []string `json:"attachment_doc_ids"`
	RangeBegin       string   `json:"range_begin"`
	RangeEnd         string   `json:"range_end"`
	GroupID          string   `json:"group_id"`
}

type Volume struct {
	Name         string `json:"name"`
	DeclaredRoot string `json:"declared_root"`
	Ordinal      int    `json:"ordinal"`
}

type Value struct {
	Kind   string     `json:"kind"`
	Text   string     `json:"text"`
	List   []string   `json:"list"`
	Number int64      `json:"number"`
	Bool   bool       `json:"bool"`
	Time   *TimeClaim `json:"time"`
}

type TimeClaim struct {
	Raw           string  `json:"raw"`
	DateValue     string  `json:"date_value"`
	Precision     string  `json:"precision"`
	TimezoneKind  string  `json:"timezone_kind"`
	ZoneText      string  `json:"zone_text"`
	OffsetSeconds *int    `json:"offset_seconds"`
	UTCKey        *string `json:"utc_key"`
}

type Diagnostic struct {
	Code       string
	Severity   string
	LoadFile   string
	RowID      string
	Column     string
	Detail     string
	RowOrdinal int
}
