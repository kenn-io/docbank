package report

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxTerms           = 128
	maxExpressionRunes = 8192
	maxChoices         = 50000
)

// NormalizeRequest validates all request-level bounds without changing the
// caller's row order, expressions, or date cutoffs. Store-dependent identities
// and evidence bindings are checked when the frozen observation is built.
func NormalizeRequest(r Request) (Request, error) {
	if r.Version != 1 && r.Version != 2 {
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
	if len(r.SelectedDocuments) > 0 {
		scopes++
	}
	if scopes != 1 || r.Version == 1 && len(r.SelectedDocuments) > 0 {
		return Request{}, errors.New("select all documents, collections, or exact documents")
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
	if len(r.SelectedDocuments) > 50000 {
		return Request{}, errors.New("too many selected documents")
	}
	selectedNodes := make(map[int64]bool, len(r.SelectedDocuments))
	for _, member := range r.SelectedDocuments {
		if member.NodeID <= 0 || member.VersionID == "" || !validSHA256(member.SHA256) || selectedNodes[member.NodeID] {
			return Request{}, errors.New("invalid or duplicate selected document")
		}
		selectedNodes[member.NodeID] = true
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
	r.SelectedDocuments = append([]Identity(nil), r.SelectedDocuments...)
	r.Terms = append([]Term(nil), r.Terms...)
	r.DateChoices = append([]DateChoice(nil), r.DateChoices...)
	return r, nil
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
