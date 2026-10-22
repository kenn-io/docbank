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

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
)

const (
	MetadataKey      = "docbank.evidence"
	MaxChars         = 1000
	MaxEntries       = 64
	ContextPageSize  = 10
	MaxContextOffset = MaxEntries
)

var (
	ErrInvalid             = errors.New("invalid Kata evidence request")
	ErrUnsupported         = errors.New("unsupported Kata evidence metadata")
	ErrUnsupportedSource   = errors.New("unsupported Kata evidence source")
	ErrEntryLimit          = fmt.Errorf("Kata issue already holds the maximum of %d Docbank quotes", MaxEntries)
	ErrChanged             = errors.New("selected document passage changed; select it again")
	ErrUnavailable         = errors.New("selected document passage is unavailable")
	ErrConflict            = errors.New("Kata evidence kept changing; retry the same request")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different request")
)

// Reference shares only msgvault's version/entries/id/reference envelope fields.
type Reference struct {
	Version   int                    `json:"version"`
	Kind      string                 `json:"kind"`
	Docbank   *DocumentReference     `json:"docbank,omitzero"`
	Rendition *document.TextCitation `json:"rendition,omitzero"`
}

// DocumentReference pins original UTF-8 text with half-open rune offsets.
type DocumentReference struct {
	VaultUID         string `json:"vault_uid"`
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	ContentSHA256    string `json:"content_sha256"`
	StartRune        int    `json:"start_rune"`
	EndRune          int    `json:"end_rune"`
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
	Offset int    `json:"offset" minimum:"0" maximum:"64"`
}

type Evidence struct {
	ID           string    `json:"id"`
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
	// Resolve compares the saved quote with the verified source passage.
	Resolve(ctx context.Context, reference Reference, quote string) (Resolution, error)
}

func ValidateReference(ref Reference) error {
	if ref.Version != 1 || ref.Kind != "docbank" || (ref.Docbank == nil) == (ref.Rendition == nil) {
		return ErrInvalid
	}
	if c := ref.Rendition; c != nil {
		if document.ValidateTextCitation(*c) != nil || c.End-c.Start > MaxChars {
			return ErrInvalid
		}
		return nil
	}
	p := ref.Docbank
	if p.NodeID < 1 || canonical.ValidateUUIDv4(p.VaultUID) != nil ||
		canonical.ValidateUUIDv4(p.ContentVersionID) != nil || !canonical.IsSHA256Hex(p.ContentSHA256) ||
		p.StartRune < 0 || p.EndRune <= p.StartRune || p.EndRune > 2147483647 || p.EndRune-p.StartRune > MaxChars {
		return ErrInvalid
	}
	return nil
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

func NewEvidence(ref Reference, words, label string) Evidence {
	return Evidence{ID: ID(ref), Reference: ref, Excerpt: words, Label: label, ContentTrust: "untrusted"}
}
