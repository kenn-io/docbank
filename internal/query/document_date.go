package query

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/civiltime"
)

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
	if _, err := civiltime.ParseDate(from); err != nil {
		return "", "", &DateError{Code: "invalid_date", Err: err}
	}
	if _, err := civiltime.ParseDate(to); err != nil {
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
		if !slices.Contains([]string{"exact", "day", "coarse"}, precision) {
			return nil, fmt.Errorf("unknown date precision %q", precision)
		}
	}
	slices.Sort(out.Kinds)
	out.Kinds = slices.Compact(out.Kinds)
	slices.Sort(out.Precisions)
	out.Precisions = slices.Compact(out.Precisions)
	return out, nil
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
		if e.Kind == ExpressionField && slices.Contains([]string{"date", "date_kind", "date_precision"}, e.Field) {
			return true
		}
		return slices.ContainsFunc(e.Children, has)
	}
	return has(expr)
}
