package embedmodel

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"go.kenn.io/kit/embedconfig"
)

const (
	// KindText is ordinary text the shared client can encode.
	KindText = "text"
	// KindImage is a non-text part. This package accepts the description and
	// leaves the bytes with the caller.
	KindImage = "image"
	// KindFile is a non-text part. This package accepts the description and
	// leaves the bytes with the caller.
	KindFile = "file"
)

// Part is one piece of caller content. Kind is an open label. text, image,
// and file are the kinds this package understands. Other kinds are accepted
// when they carry text or a media type.
type Part struct {
	Kind      string
	MediaType string
	Text      string
}

// SourceSpan is a half-open coordinate range in the caller's source.
// Byte and rune offsets name the same range. Label is an optional caller
// handle such as a page or a message, and this package does not interpret it.
type SourceSpan struct {
	ByteStart int
	ByteEnd   int
	RuneStart int
	RuneEnd   int
	Label     string
}

// Content is one embedding input. Set Text, or set Parts, not both. An empty
// Kind means text. Spans locate the input in the source the caller indexed.
type Content struct {
	Role  embedconfig.Role
	Kind  string
	Text  string
	Parts []Part
	Spans []SourceSpan
}

// ErrUnsupportedContent reports that the shared text path cannot encode this
// input. Callers keep their existing image and file encoders.
var ErrUnsupportedContent = errors.New("embed content is not text")

// BlankText reports whether text has nothing an embedding model can represent.
// The rule matches vector.blank: empty, or every rune is whitespace, Unicode
// category Cf, or a control character. Visible blanks such as U+2800 stay.
func BlankText(text string) bool {
	for _, r := range text {
		if !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) && !unicode.Is(unicode.Cc, r) {
			return false
		}
	}
	return true
}

// Format applies the literal prefix and suffix for role.
// Named formatters are not applied here. The caller applies those before
// constructing the text.
func Format(role embedconfig.Role, text string, roles embedconfig.Roles) (string, error) {
	switch role {
	case embedconfig.RoleDocument:
		return roles.DocumentPrefix + text + roles.DocumentSuffix, nil
	case embedconfig.RoleQuery:
		return roles.QueryPrefix + text + roles.QuerySuffix, nil
	default:
		return "", errors.New("embed role must be document or query")
	}
}

// Validate checks the shape of one input.
func (c Content) Validate() error {
	switch c.Role {
	case embedconfig.RoleDocument, embedconfig.RoleQuery:
	default:
		return errors.New("embed content role must be document or query")
	}
	if c.Text != "" && len(c.Parts) > 0 {
		return errors.New("embed content sets text and parts")
	}
	for i, part := range c.Parts {
		if err := part.Validate(); err != nil {
			return fmt.Errorf("embed content part %d: %w", i, err)
		}
	}
	for i, span := range c.Spans {
		if err := span.Validate(); err != nil {
			return fmt.Errorf("embed content span %d: %w", i, err)
		}
	}
	return nil
}

// Validate checks one part.
func (p Part) Validate() error {
	if p.Kind == "" {
		return errors.New("kind is required")
	}
	switch p.Kind {
	case KindImage, KindFile:
		if strings.TrimSpace(p.MediaType) == "" || p.Text != "" {
			return fmt.Errorf("%s part needs a media type and no inline text", p.Kind)
		}
	case KindText:
		if p.Text == "" {
			return errors.New("text part is empty")
		}
	default:
		if p.Text == "" && strings.TrimSpace(p.MediaType) == "" {
			return errors.New("part needs text or a media type")
		}
	}
	return nil
}

// Validate checks one source span.
func (s SourceSpan) Validate() error {
	if s.ByteStart < 0 || s.RuneStart < 0 || s.ByteEnd < s.ByteStart || s.RuneEnd < s.RuneStart {
		return errors.New("span offsets must be ordered and non-negative")
	}
	return nil
}

// EmbedText returns the text the shared client can encode.
// A top-level image or file kind returns ErrUnsupportedContent even when
// Text is set. Parts of those kinds do the same. Those kinds stay valid
// descriptions for callers that encode them elsewhere.
func (c Content) EmbedText() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	switch c.Kind {
	case KindImage, KindFile:
		return "", ErrUnsupportedContent
	}
	if len(c.Parts) == 0 {
		return c.Text, nil
	}
	pieces := make([]string, 0, len(c.Parts))
	for _, part := range c.Parts {
		if part.Kind != KindText {
			return "", ErrUnsupportedContent
		}
		pieces = append(pieces, part.Text)
	}
	return strings.Join(pieces, "\n"), nil
}
