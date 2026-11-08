package notemarkdown

import "errors"

// Recipe identifies the syntax, limits, and artifact encoding of Parse.
const Recipe = "docbank-authored-markdown/v1"

// ResolverRecipe identifies logical target and fragment resolution policy.
const ResolverRecipe = "docbank-note-targets/v1"

var (
	ErrLimit               = errors.New("authored Markdown resource limit")
	ErrInvalidInput        = errors.New("invalid authored Markdown input")
	ErrInvalidArtifact     = errors.New("invalid authored Markdown artifact")
	ErrInvalidCandidateSet = errors.New("invalid authored Markdown candidate set")
)

// LimitError contains a fixed resource code, never copied source content.
type LimitError struct{ Resource string }

func (e *LimitError) Error() string { return "authored Markdown limit: " + e.Resource }
func (e *LimitError) Unwrap() error { return ErrLimit }

// Span addresses half-open UTF-8 byte offsets in the exact original.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Metadata struct {
	Title   string   `json:"title"`
	Aliases []string `json:"aliases"`
	Tags    []string `json:"tags"`
}

type Heading struct {
	Level    int    `json:"level"`
	Span     Span   `json:"span"`
	Section  Span   `json:"section"`
	Markdown string `json:"markdown"`
	Text     string `json:"text"`
	Anchor   string `json:"anchor"`
}

type Link struct {
	Span            Span   `json:"span"`
	DestinationSpan Span   `json:"destination_span"`
	Form            string `json:"form"`
	Embed           bool   `json:"embed"`
	RawTarget       string `json:"raw_target"`
	Target          string `json:"target"`
}

type Diagnostic struct {
	Code string `json:"code"`
	Span Span   `json:"span"`
}

type Note struct {
	SourceSHA256     string       `json:"source_sha256"`
	SourceBytes      int          `json:"source_bytes"`
	Recipe           string       `json:"recipe"`
	FrontmatterState string       `json:"frontmatter_state"`
	Frontmatter      Span         `json:"frontmatter"`
	BodyStart        int          `json:"body_start"`
	Metadata         Metadata     `json:"metadata"`
	Headings         []Heading    `json:"headings"`
	Links            []Link       `json:"links"`
	Diagnostics      []Diagnostic `json:"diagnostics"`
}

type LookupKey struct {
	Kind  string
	Value string
}

type TargetQuery struct {
	Keys     []LookupKey
	Fragment string
	Kind     string
}

type Candidate struct {
	Binding string
	Path    string
	Kind    string
	Keys    []LookupKey
}

// CandidateSet is evidence from one owner-authorized namespace snapshot.
// Complete certifies all queried keys, or sufficient matches to prove ambiguity.
type CandidateSet struct {
	NamespaceGeneration string
	Complete            bool
	Candidates          []Candidate
}

type Resolution struct {
	State               string
	Binding             string
	NamespaceGeneration string
}

type FragmentResolution struct {
	State   string
	Heading *Span
}
