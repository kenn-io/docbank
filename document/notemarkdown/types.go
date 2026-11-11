package notemarkdown

import "errors"

// Recipe identifies the source structure and normalization rules of Parse.
const Recipe = "docbank-note-markdown/v2"

var (
	ErrLimit        = errors.New("Markdown note resource limit")
	ErrInvalidInput = errors.New("invalid Markdown note input")
)

// LimitError contains a fixed resource code, never copied source content.
type LimitError struct{ Resource string }

func (e *LimitError) Error() string { return "Markdown note limit: " + e.Resource }
func (e *LimitError) Unwrap() error { return ErrLimit }

// Span addresses half-open byte offsets in the exact original, including its BOM.
type Span struct{ Start, End int }
type Diagnostic struct {
	Code string
	Span Span
}

// Limits can narrow the hard limits. Zero selects the default; negative values
// and values above the defaults are invalid. Internal work limits also apply.
type Limits struct {
	MaxBytes, MaxHeadings, MaxLinks                    int
	MaxFrontmatterKeys, MaxValuesPerKey, MaxValueBytes int
	MaxDiagnostics                                     int
}

type Note struct {
	Frontmatter        *Frontmatter
	BodyStart          int
	Title, TitleSource string
	Headings           []Heading
	Links              []Link
	Lines              int
	Diagnostics        []Diagnostic
}

type Frontmatter struct {
	Start, End    int
	Raw           map[string]any
	Fields        []Field
	Aliases, Tags []string
}
type Field struct {
	Key, Value string
	Ordinal    int
}
type Heading struct {
	Ordinal, Level             int
	Text, Slug, Path           string
	Start, LineEnd, SectionEnd int
	Line, SectionEndLine       int
}
type Link struct {
	Ordinal                      int
	Form                         string
	Raw, Target, Fragment, Label string
	Start, End, Line             int
}

// Source syntax stays private: consumers receive the P02 projection only.
type parsedLink struct {
	Span                    Span
	Form                    string
	Target, Fragment, Label string
}
