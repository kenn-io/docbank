// Package kataevidence quotes verified Docbank passages into Kata issues.
package kataevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"uuid"

	"go.kenn.io/docbank/document"
)

const (
	MetadataKey         = "docbank.evidence"
	MsgvaultMetadataKey = "msgvault.evidence"
	MaxChars            = 1000
	MaxEntries          = 64
	ContextPageSize     = 10
	MaxContextOffset    = MaxEntries + 256
)

var (
	ErrInvalid             = errors.New("invalid Kata evidence request")
	ErrUnsupported         = errors.New("unsupported Kata evidence metadata")
	ErrUnsupportedSource   = errors.New("unsupported Kata evidence source")
	ErrChanged             = errors.New("selected document passage changed; select it again")
	ErrUnavailable         = errors.New("selected document passage is unavailable")
	ErrConflict            = errors.New("Kata evidence kept changing; retry the same request")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")
)

// Reference extends msgvault's reference union without inventing message provenance.
type Reference struct {
	Version int                `json:"version"`
	Kind    string             `json:"kind"`
	Docbank *DocumentReference `json:"docbank,omitzero"`
}

// DocumentReference pins original UTF-8 text or an exact retained rendition.
// Rune offsets are half-open; the three rendition fields are present together.
type DocumentReference struct {
	VaultUID              string `json:"vault_uid"`
	NodeID                int64  `json:"node_id"`
	ContentVersionID      string `json:"content_version_id"`
	ContentSHA256         string `json:"content_sha256"`
	RenditionAttachmentID string `json:"rendition_attachment_id,omitzero"`
	BuildID               string `json:"build_id,omitzero"`
	RenditionSHA256       string `json:"rendition_sha256,omitzero"`
	StartRune             int    `json:"start_rune"`
	EndRune               int    `json:"end_rune"`
}

type Selector struct {
	NodeID    int64  `json:"node_id,omitzero" minimum:"1"`
	Path      string `json:"path,omitzero" maxLength:"4096"`
	StartRune int    `json:"start_rune" minimum:"0" maximum:"2147483647"`
	EndRune   int    `json:"end_rune" minimum:"1" maximum:"2147483647"`
	// ExpectedVersion and Quote let a caller bind an earlier selection.
	ExpectedVersion string                 `json:"expected_version,omitzero" maxLength:"36"`
	Quote           string                 `json:"quote,omitzero" maxLength:"1000"`
	Citation        *document.TextCitation `json:"citation,omitzero"`
}

type CiteRequest struct {
	Selector       Selector `json:"selector"`
	Title          string   `json:"title,omitzero" maxLength:"512"`
	Brief          string   `json:"brief,omitzero" maxLength:"2000"`
	Issue          string   `json:"issue,omitzero" maxLength:"256"`
	IdempotencyKey string   `json:"idempotency_key,omitzero" maxLength:"128"`
}

type ContextRequest struct {
	Issue  string `json:"issue" minLength:"1" maxLength:"256"`
	Offset int    `json:"offset" minimum:"0" maximum:"320"`
}

type Evidence struct {
	ID           string    `json:"id"`
	Passage      string    `json:"passage"`
	Reference    Reference `json:"reference"`
	Excerpt      string    `json:"excerpt"`
	Label        string    `json:"label"`
	ContentTrust string    `json:"content_trust"`
}

type Resolution struct {
	State      string   `json:"state"`
	Evidence   Evidence `json:"evidence"`
	SavedQuote string   `json:"saved_quote,omitzero"`
}

type Archive interface {
	VaultUID() string
	Prepare(ctx context.Context, selector Selector) (Evidence, error)
	Resolve(ctx context.Context, reference Reference) (Resolution, error)
}

func ValidateReference(ref Reference) error {
	p := ref.Docbank
	if ref.Version != 1 || ref.Kind != "docbank" || p == nil || p.NodeID < 1 ||
		!canonicalUUID(p.VaultUID) || !canonicalUUID(p.ContentVersionID) || !digest(p.ContentSHA256) ||
		p.StartRune < 0 || p.EndRune <= p.StartRune || p.EndRune > 2147483647 || p.EndRune-p.StartRune > MaxChars {
		return ErrInvalid
	}
	if p.RenditionAttachmentID != "" || p.BuildID != "" || p.RenditionSHA256 != "" {
		if !digest(p.RenditionAttachmentID) || !digest(p.BuildID) || !digest(p.RenditionSHA256) {
			return ErrInvalid
		}
	}
	return nil
}

func canonicalUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value && id[6]>>4 == 4 && id[8]>>6 == 2
}

func digest(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

// Digest uses the same unambiguous length-prefix encoding as msgvault, with independent domains.
func Digest(domain string, fields ...string) string {
	h := sha256.New()
	for _, part := range append([]string{domain}, fields...) {
		_, _ = io.WriteString(h, strconv.Itoa(len(part))+":"+part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ID(ref Reference) string {
	data, _ := json.Marshal(ref)
	return Digest("docbank.kata.evidence.v1", string(data))
}

func PassageID(ref Reference, words string) string {
	p := ref.Docbank
	if p == nil {
		return ""
	}
	return Digest("docbank.kata.passage.v1", p.VaultUID, strconv.FormatInt(p.NodeID, 10),
		p.RenditionAttachmentID, strconv.Itoa(p.StartRune), strconv.Itoa(p.EndRune), words)
}

func NewEvidence(ref Reference, words, label string) Evidence {
	return Evidence{ID: ID(ref), Passage: PassageID(ref, words), Reference: ref, Excerpt: words, Label: label, ContentTrust: "untrusted"}
}

func (p DocumentReference) TextCitation() (document.TextCitation, error) {
	vault, err := uuid.Parse(p.VaultUID)
	if err != nil {
		return document.TextCitation{}, fmt.Errorf("%w: vault", ErrInvalid)
	}
	version, err := uuid.Parse(p.ContentVersionID)
	if err != nil {
		return document.TextCitation{}, fmt.Errorf("%w: version", ErrInvalid)
	}
	c := document.TextCitation{Version: 1, VaultUID: vault, NodeID: p.NodeID, ContentVersionID: version,
		ContentSHA256: p.ContentSHA256, RenditionAttachmentID: p.RenditionAttachmentID, BuildID: p.BuildID,
		RenditionSHA256: p.RenditionSHA256, Start: p.StartRune, End: p.EndRune}
	return c, document.ValidateTextCitation(c)
}
