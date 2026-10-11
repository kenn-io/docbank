package document

import (
	"errors"
	"uuid"

	"go.kenn.io/docbank/internal/canonical"
)

var (
	ErrInvalidTextCitation  = errors.New("text citation is invalid")
	ErrCitationUnavailable  = errors.New("cited text is unavailable")
	ErrCitationLimit        = errors.New("cited rendition exceeds the read limit")
	ErrInvalidCitationRange = errors.New("citation range exceeds the rendition")
	ErrCitationIntegrity    = errors.New("cited rendition failed verification")
)

// TextCitation identifies a quotation without keeping its evidence alive.
// Start and End delimit Unicode code points in the retained sanitized Markdown.
type TextCitation struct {
	Version               int       `json:"version" enum:"1"`
	VaultUID              uuid.UUID `json:"vault_uid" format:"uuid" minLength:"36" maxLength:"36" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
	NodeID                int64     `json:"node_id" minimum:"1"`
	ContentVersionID      uuid.UUID `json:"content_version_id" format:"uuid" minLength:"36" maxLength:"36" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"`
	ContentSHA256         string    `json:"content_sha256" pattern:"^[0-9a-f]{64}$"`
	RenditionAttachmentID string    `json:"rendition_attachment_id" pattern:"^[0-9a-f]{64}$"`
	BuildID               string    `json:"build_id" pattern:"^[0-9a-f]{64}$"`
	RenditionSHA256       string    `json:"rendition_sha256" pattern:"^[0-9a-f]{64}$"`
	Start                 int       `json:"start" minimum:"0" maximum:"2147483647"`
	End                   int       `json:"end" minimum:"1" maximum:"2147483647"`
}

// ResolvedTextCitation contains exact UTF-8 text from a verified rendition.
type ResolvedTextCitation struct {
	Citation   TextCitation `json:"citation"`
	Text       string       `json:"text" minLength:"1" maxLength:"16000"`
	TextSHA256 string       `json:"text_sha256" pattern:"^[0-9a-f]{64}$"`
	TextBytes  int          `json:"text_bytes" minimum:"1" maximum:"64000"`
}

// ValidateTextCitation checks typed identities and range bounds without rewriting them.
func ValidateTextCitation(citation TextCitation) error {
	if citation.Version != 1 || citation.NodeID < 1 || citation.Start < 0 ||
		citation.End > 2147483647 || citation.End <= citation.Start ||
		citation.End-citation.Start > 16_000 {
		return ErrInvalidTextCitation
	}
	for _, id := range []uuid.UUID{citation.VaultUID, citation.ContentVersionID} {
		if id[6]>>4 != 4 || id[8]>>6 != 2 {
			return ErrInvalidTextCitation
		}
	}
	for _, digest := range []string{
		citation.ContentSHA256, citation.RenditionAttachmentID,
		citation.BuildID, citation.RenditionSHA256,
	} {
		if !canonical.IsSHA256Hex(digest) {
			return ErrInvalidTextCitation
		}
	}
	return nil
}
