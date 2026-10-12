package document

import "errors"

var (
	ErrInvalidTextNavigation = errors.New("text navigation is invalid")
	ErrTextNavigationLimit   = errors.New("retained text exceeds the navigation limit")
)

const (
	MaxTextNavigationBytes      = 4 << 20
	MaxTextNavigationLineBreaks = 65_536
	MaxTextNavigationHeadings   = 4096
	TextOutlinePageSize         = 50
	MaxTextOutlineTitleChars    = 160
	MaxTextSectionChars         = 16_000
)

// TextSectionRange identifies a whole section in retained Markdown code points.
type TextSectionRange struct {
	Start int `json:"start" minimum:"0" maximum:"2147483647"`
	End   int `json:"end" minimum:"1" maximum:"2147483647"`
}

// TextOutlineRequest uses a saved citation to locate an exact rendition's outline.
type TextOutlineRequest struct {
	Citation TextCitation `json:"citation"`
	Offset   int          `json:"offset" minimum:"0" maximum:"4097"`
}

// TextOutlineEntry includes all subsections up to the next equal or lesser level.
type TextOutlineEntry struct {
	Start          int    `json:"start" minimum:"0" maximum:"2147483647"`
	End            int    `json:"end" minimum:"1" maximum:"2147483647"`
	Level          int    `json:"level" minimum:"0" maximum:"6"`
	TitleMarkdown  string `json:"title_markdown" maxLength:"160"`
	TitleTruncated bool   `json:"title_truncated"`
}

// TextOutlinePage reports complete-document bounds even for a partial outline.
type TextOutlinePage struct {
	Citation         TextCitation       `json:"citation"`
	DocumentChars    int                `json:"document_chars" minimum:"1" maximum:"2147483647"`
	EnclosingSection TextSectionRange   `json:"enclosing_section"`
	Offset           int                `json:"offset" minimum:"0" maximum:"4097"`
	Total            int                `json:"total" minimum:"1" maximum:"4097"`
	Sections         []TextOutlineEntry `json:"sections" maxItems:"50"`
	NextOffset       *int               `json:"next_offset,omitempty" minimum:"1" maximum:"4096"`
}

// TextSectionRequest reads a relative offset within one exact whole section.
type TextSectionRequest struct {
	Citation TextCitation     `json:"citation"`
	Section  TextSectionRange `json:"section"`
	Offset   int              `json:"offset" minimum:"0" maximum:"2147483647"`
	MaxChars int              `json:"max_chars" minimum:"1" maximum:"16000"`
}

// TextSectionPage preserves exact bytes and distinguishes a page from a whole section.
type TextSectionPage struct {
	Citation    TextCitation     `json:"citation"`
	Section     TextSectionRange `json:"section"`
	Offset      int              `json:"offset" minimum:"0" maximum:"2147483647"`
	ActualStart int              `json:"actual_start" minimum:"0" maximum:"2147483647"`
	ActualEnd   int              `json:"actual_end" minimum:"0" maximum:"2147483647"`
	NextOffset  int              `json:"next_offset" minimum:"0" maximum:"2147483647"`
	Complete    bool             `json:"complete"`
	Text        string           `json:"text" maxLength:"16000"`
	TextSHA256  string           `json:"text_sha256" pattern:"^[0-9a-f]{64}$"`
	TextBytes   int              `json:"text_bytes" minimum:"0" maximum:"64000"`
}

// ValidateTextOutlineRequest checks shape without assuming retained bytes exist.
func ValidateTextOutlineRequest(request TextOutlineRequest) error {
	if err := ValidateTextCitation(request.Citation); err != nil {
		return err
	}
	if request.Offset < 0 || request.Offset > MaxTextNavigationHeadings+1 {
		return ErrInvalidTextNavigation
	}
	return nil
}

// ValidateTextSectionRequest checks arithmetic before any content is opened.
func ValidateTextSectionRequest(request TextSectionRequest) error {
	if err := ValidateTextCitation(request.Citation); err != nil {
		return err
	}
	section := request.Section
	if section.Start < 0 || section.End > 2147483647 || section.End <= section.Start ||
		request.Offset < 0 || request.Offset > section.End-section.Start ||
		request.MaxChars < 1 || request.MaxChars > MaxTextSectionChars {
		return ErrInvalidTextNavigation
	}
	return nil
}
