package report

import (
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	maxTerms           = 128
	maxExpressionRunes = 8192
	maxChoices         = 50000
)

var ErrInvalidSelection = errors.New("invalid report selection")

// NormalizeRequest validates all request-level bounds without changing the
// caller's row order, expressions, or date cutoffs. Store-dependent identities
// and evidence bindings are checked when the frozen observation is built.
func NormalizeRequest(r Request) (Request, error) {
	if r.Version != 1 {
		return Request{}, fmt.Errorf("unsupported report request version %d", r.Version)
	}
	if len(r.Profile) > 128 || strings.TrimSpace(r.Profile) != r.Profile {
		return Request{}, errors.New("invalid report processing profile")
	}
	scopes := 0
	if r.AllDocuments {
		scopes++
	}
	if len(r.CollectionIDs) > 0 {
		scopes++
	}
	if r.SelectedDocuments != nil {
		scopes++
	}
	if scopes != 1 {
		return Request{}, errors.New("select all documents, collections, or exact documents")
	}
	if r.SelectedDocuments != nil {
		selected, err := normalizeSelectedDocuments(r.SelectedDocuments.Documents)
		if err != nil {
			return Request{}, err
		}
		r.SelectedDocuments = selected
	}
	if len(r.CollectionIDs) > 50000 {
		return Request{}, errors.New("too many collections")
	}
	seenCollections := make(map[string]bool, len(r.CollectionIDs))
	for _, id := range r.CollectionIDs {
		if id == "" || strings.TrimSpace(id) != id || seenCollections[id] {
			return Request{}, errors.New("invalid or duplicate collection ID")
		}
		seenCollections[id] = true
	}
	if err := validateTimezone(r.Timezone); err != nil {
		return Request{}, fmt.Errorf("report timezone: %w", err)
	}
	if r.SourceTimezone != "" {
		if err := validateTimezone(r.SourceTimezone); err != nil {
			return Request{}, fmt.Errorf("source timezone: %w", err)
		}
	}
	if r.NumericDateOrder != "" && r.NumericDateOrder != "MDY" && r.NumericDateOrder != "DMY" {
		return Request{}, errors.New("numeric date order must be MDY or DMY")
	}
	if r.CoverageMode == "" {
		r.CoverageMode = "strict"
	}
	if r.CoverageMode != "strict" && r.CoverageMode != "available_only" {
		return Request{}, fmt.Errorf("unsupported coverage mode %q", r.CoverageMode)
	}
	if len(r.Terms) == 0 || len(r.Terms) > maxTerms {
		return Request{}, fmt.Errorf("report requires 1 to %d terms", maxTerms)
	}
	seenTerms := make(map[int]bool, len(r.Terms))
	for _, term := range r.Terms {
		if term.Number <= 0 || seenTerms[term.Number] {
			return Request{}, fmt.Errorf("invalid or duplicate term number %d", term.Number)
		}
		seenTerms[term.Number] = true
		if term.Syntax != "simple" && term.Syntax != "advanced" {
			return Request{}, fmt.Errorf("term %d: unsupported syntax %q", term.Number, term.Syntax)
		}
		if !utf8.ValidString(term.Expression) || strings.ContainsRune(term.Expression, 0) ||
			strings.TrimSpace(term.Expression) == "" || utf8.RuneCountInString(term.Expression) > maxExpressionRunes {
			return Request{}, fmt.Errorf("term %d: invalid expression", term.Number)
		}
		start, err := parseISODate(term.Dates.Start)
		if err != nil {
			return Request{}, fmt.Errorf("term %d: invalid start date: %w", term.Number, err)
		}
		end, err := parseISODate(term.Dates.End)
		if err != nil || end.Before(start) {
			return Request{}, fmt.Errorf("term %d: invalid cutoff date", term.Number)
		}
	}
	if len(r.DateChoices) > maxChoices {
		return Request{}, errors.New("too many reviewed date choices")
	}
	seenChoices := make(map[Identity]bool, len(r.DateChoices))
	for _, choice := range r.DateChoices {
		if err := validateChoiceShape(choice); err != nil {
			return Request{}, fmt.Errorf("%w: %w", ErrInvalidChoice, err)
		}
		if seenChoices[choice.Document] {
			return Request{}, fmt.Errorf("%w: duplicate reviewed choice for document %d", ErrInvalidChoice, choice.Document.NodeID)
		}
		seenChoices[choice.Document] = true
	}
	r.CollectionIDs = append([]string(nil), r.CollectionIDs...)
	r.Terms = append([]Term(nil), r.Terms...)
	r.DateChoices = append([]DateChoice(nil), r.DateChoices...)
	return r, nil
}

func normalizeSelectedDocuments(documents []Identity) (*SelectedDocuments, error) {
	if len(documents) > 50000 {
		return nil, fmt.Errorf("%w: too many selected documents", ErrReportLimit)
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("%w: select at least one document", ErrInvalidSelection)
	}
	seen := make(map[int64]bool, len(documents))
	for _, id := range documents {
		version, err := uuid.Parse(id.VersionID)
		if id.NodeID <= 0 || seen[id.NodeID] || err != nil || version.String() != id.VersionID ||
			version.Version() != 4 || version.Variant() != uuid.RFC4122 || !canonical.IsSHA256Hex(id.SHA256) {
			return nil, fmt.Errorf("%w: invalid or repeated document identity", ErrInvalidSelection)
		}
		seen[id.NodeID] = true
	}
	selected := &SelectedDocuments{Documents: slices.Clone(documents)}
	slices.SortFunc(selected.Documents, func(a, b Identity) int { return cmp.Compare(a.NodeID, b.NodeID) })
	return selected, nil
}

func validateTimezone(zone string) error {
	if zone == "" || zone == "Local" || strings.TrimSpace(zone) != zone {
		return fmt.Errorf("invalid timezone %q", zone)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", zone, err)
	}
	return nil
}

func parseISODate(value string) (time.Time, error) {
	date, err := time.Parse(time.DateOnly, value)
	if err != nil || date.Format(time.DateOnly) != value {
		return time.Time{}, errors.New("expected a literal YYYY-MM-DD date")
	}
	return date, nil
}

func validateChoiceShape(choice DateChoice) error {
	if choice.Document.NodeID <= 0 || choice.Document.VersionID == "" || !validSHA256(choice.Document.SHA256) ||
		choice.CandidateID == "" || !validSHA256(choice.EvidenceSHA256) ||
		strings.TrimSpace(choice.Reason) == "" || len(choice.Reason) > 4096 {
		return errors.New("reviewed date choice has an invalid binding or reason")
	}
	switch choice.Action {
	case "select":
		if choice.ReviewedDate != "" || choice.ReviewedTimezone != "" || choice.ReviewedRole != "" {
			return errors.New("select action cannot replace candidate fields")
		}
	case "interpret":
		if _, err := parseISODate(choice.ReviewedDate); err != nil {
			return fmt.Errorf("interpret action requires a reviewed date: %w", err)
		}
		if err := validateTimezone(choice.ReviewedTimezone); err != nil {
			return fmt.Errorf("interpret action requires a reviewed timezone: %w", err)
		}
		if choice.ReviewedRole != "" && !validReviewedRole(choice.ReviewedRole) {
			return fmt.Errorf("invalid reviewed role %q", choice.ReviewedRole)
		}
	case "reclassify":
		if choice.ReviewedDate != "" || choice.ReviewedTimezone != "" || !validReviewedRole(choice.ReviewedRole) {
			return errors.New("reclassify action requires a role without date replacement")
		}
	default:
		return fmt.Errorf("unsupported reviewed date action %q", choice.Action)
	}
	return nil
}

func validReviewedRole(role string) bool {
	switch role {
	case "document_date", "created", "authored", "sent", "captured", "signed", "effective", "expiry":
		return true
	default:
		return false
	}
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// validateSelectedMembers requires the whole normalized selection exactly once.
func validateSelectedMembers(request Request, members []Member) error {
	if request.SelectedDocuments == nil {
		return nil
	}
	documents := request.SelectedDocuments.Documents
	if len(documents) != len(members) {
		return fmt.Errorf("%w: selected member count differs", ErrInvalidSelection)
	}
	remaining := make(map[Identity]bool, len(documents))
	for _, id := range documents {
		remaining[id] = true
	}
	for _, member := range members {
		if !remaining[member.Identity] {
			return fmt.Errorf("%w: selected member identity differs", ErrInvalidSelection)
		}
		delete(remaining, member.Identity)
	}
	return nil
}

// ByteLimit returns the encoded date-page ceiling, including its cursor.
func (p DatePageRequest) ByteLimit() (int, error) {
	if p.MaxBytes == 0 {
		return 1 << 20, nil
	}
	if p.MaxBytes < 64<<10 || p.MaxBytes > 1<<20 {
		return 0, fmt.Errorf("%w: date page max_bytes must be 65536 through 1048576", ErrReportLimit)
	}
	return p.MaxBytes, nil
}
