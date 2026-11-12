package query

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/civil"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/civiltime"
)

// LoadZone enforces the timeline zone admission policy.
func LoadZone(name string) (*time.Location, error) {
	if name == "UTC" {
		return time.UTC, nil
	}
	if !strings.Contains(name, "/") || strings.HasPrefix(name, "posix/") || strings.HasPrefix(name, "right/") {
		return nil, fmt.Errorf("invalid timezone %q", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone %q: %w", name, err)
	}
	return loc, nil
}

// RangeBounds composes inclusive civil endpoints from Kit day boundaries.
func RangeBounds(from, to civil.Date, loc *time.Location) (time.Time, time.Time, error) {
	if !from.IsValid() || !to.IsValid() || from.After(to) {
		return time.Time{}, time.Time{}, errors.New("invalid date range")
	}
	start, _, ok := civiltime.DayBounds(from, loc)
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("date %s does not exist in %s", from, loc)
	}
	_, end, ok := civiltime.DayBounds(to, loc)
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("date %s does not exist in %s", to, loc)
	}
	return start, end, nil
}

// DateError carries the stable API code for a date or timezone failure.
type DateError struct {
	Code string
	Err  error
}

func (e *DateError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *DateError) Unwrap() error { return e.Err }

// DocumentDateFilter limits one visible event claim.
type DocumentDateFilter struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Kinds      []string `json:"kinds,omitempty"`
	Precisions []string `json:"precisions,omitempty"`
}

// ParseDocumentDate parses an inclusive date range.
func ParseDocumentDate(value string) (string, string, error) {
	from, to, found := strings.Cut(value, "..")
	if !found {
		to = from
	}
	if _, err := civil.ParseDate(from); err != nil {
		return "", "", &DateError{Code: "invalid_date", Err: err}
	}
	if _, err := civil.ParseDate(to); err != nil {
		return "", "", &DateError{Code: "invalid_date", Err: err}
	}
	if from > to {
		return "", "", &DateError{Code: "invalid_date", Err: errors.New("date range is reversed")}
	}
	return from, to, nil
}

func normalizeDocumentDate(value *DocumentDateFilter) (*DocumentDateFilter, error) {
	from, to, err := ParseDocumentDate(value.From + ".." + value.To)
	if err != nil {
		return nil, err
	}
	out := &DocumentDateFilter{From: from, To: to, Kinds: slices.Clone(value.Kinds), Precisions: slices.Clone(value.Precisions)}
	for _, kind := range out.Kinds {
		if !document.ValidDateKind(document.DateKind(kind)) {
			return nil, fmt.Errorf("unknown date kind %q", kind)
		}
	}
	for _, precision := range out.Precisions {
		if !ValidDocumentDatePrecision(precision) {
			return nil, fmt.Errorf("unknown date precision %q", precision)
		}
	}
	slices.Sort(out.Kinds)
	out.Kinds = slices.Compact(out.Kinds)
	slices.Sort(out.Precisions)
	out.Precisions = slices.Compact(out.Precisions)
	return out, nil
}

// IsDocumentDateField identifies the advanced document-date vocabulary.
func IsDocumentDateField(field string) bool {
	return slices.Contains([]string{"date", "date_kind", "date_precision"}, field)
}

// ValidDocumentDatePrecision reports whether precision is supported by date queries.
func ValidDocumentDatePrecision(precision string) bool {
	return slices.Contains([]string{"exact", "day", "coarse"}, precision)
}

// UsesDocumentDate reports whether a query needs a stable timeline zone.
func UsesDocumentDate(q Query) bool {
	if q.Filters.DocumentDate != nil || q.Sort.Field == "document_date" {
		return true
	}
	if q.Syntax != "advanced" {
		return false
	}
	expr, err := ParseExpression(q.Text, q.Syntax)
	if err != nil {
		return false
	}
	var has func(*Expression) bool
	has = func(e *Expression) bool {
		if e.Kind == ExpressionField && IsDocumentDateField(e.Field) {
			return true
		}
		return slices.ContainsFunc(e.Children, has)
	}
	return has(expr)
}
