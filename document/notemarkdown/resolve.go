package notemarkdown

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// Resolve selects only from the supplied namespace snapshot. Complete must
// certify every queried key; incomplete evidence always returns pending.
// Bindings are opaque exact-version identities whose authority the caller
// checks. This function validates the shape and consistency of that evidence.
func Resolve(ctx context.Context, query TargetQuery, set CandidateSet) (Resolution, error) {
	if err := ctx.Err(); err != nil {
		return Resolution{}, err
	}
	if !validQuery(query) {
		return Resolution{}, ErrInvalidInput
	}
	if len(set.Candidates) > 8 || !boundedIdentity(set.NamespaceGeneration, false) {
		return Resolution{}, ErrInvalidCandidateSet
	}
	byBinding := map[string]Candidate{}
	byKey := map[LookupKey]map[string]bool{}
	for _, row := range set.Candidates {
		if err := ctx.Err(); err != nil {
			return Resolution{}, err
		}
		if !boundedIdentity(row.Binding, true) || len(row.Path) > maxTarget || !canonicalSourcePath(row.Path) || row.Kind != "note" && row.Kind != "attachment" || len(row.Keys) == 0 || len(row.Keys) > 4 {
			return Resolution{}, ErrInvalidCandidateSet
		}
		if previous, ok := byBinding[row.Binding]; ok && (previous.Path != row.Path || previous.Kind != row.Kind) {
			return Resolution{}, ErrInvalidCandidateSet
		}
		byBinding[row.Binding] = row
		for _, key := range row.Keys {
			if !containsKey(query.Keys, key) {
				return Resolution{}, ErrInvalidCandidateSet
			}
			if key.Kind == "path" && key.Value != row.Path {
				return Resolution{}, ErrInvalidCandidateSet
			}
			if key.Kind == "basename" && key.Value != strings.TrimSuffix(path.Base(row.Path), path.Ext(row.Path)) {
				return Resolution{}, ErrInvalidCandidateSet
			}
			if byKey[key] == nil {
				byKey[key] = map[string]bool{}
			}
			byKey[key][row.Binding] = true
			if len(byKey[key]) > 2 {
				return Resolution{}, ErrInvalidCandidateSet
			}
		}
	}
	result := Resolution{NamespaceGeneration: set.NamespaceGeneration}
	if query.Kind != "note" {
		result.State = query.Kind
		return result, nil
	}
	if !set.Complete {
		result.State = "pending"
		return result, nil
	}
	selected := map[string]bool{}
	// Ordered exact path keys take precedence. Basenames and aliases are one
	// combined fallback set, so their iteration order cannot pick a winner.
	for _, key := range query.Keys {
		if key.Kind == "path" && len(byKey[key]) > 0 {
			selected = byKey[key]
			break
		}
	}
	if len(selected) == 0 {
		for _, key := range query.Keys {
			if key.Kind != "path" {
				for binding := range byKey[key] {
					selected[binding] = true
				}
			}
		}
	}
	switch len(selected) {
	case 0:
		result.State = "missing"
	case 1:
		for binding := range selected {
			if byBinding[binding].Kind == "attachment" {
				result.State = "attachment"
			} else {
				result.State = "resolved"
				result.Binding = binding
			}
		}
	default:
		result.State = "ambiguous"
	}
	return result, nil
}

func boundedIdentity(value string, nonempty bool) bool {
	return (!nonempty || value != "") && len(value) <= maxTarget && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func validQuery(q TargetQuery) bool {
	if !boundedIdentity(q.Fragment, false) || len(q.Keys) > 4 {
		return false
	}
	switch q.Kind {
	case "attachment", "external", "invalid", "unsupported":
		return len(q.Keys) == 0
	case "note":
		if len(q.Keys) == 0 {
			return false
		}
	default:
		return false
	}
	seen := map[LookupKey]bool{}
	for _, key := range q.Keys {
		if seen[key] || !boundedIdentity(key.Value, true) {
			return false
		}
		seen[key] = true
		switch key.Kind {
		case "path":
			if !canonicalSourcePath(key.Value) {
				return false
			}
		case "basename", "alias":
		default:
			return false
		}
	}
	return true
}

func containsKey(keys []LookupKey, key LookupKey) bool { return slices.Contains(keys, key) }

// ResolveFragment uses headings from the selected exact content version.
// Exact generated anchors precede normalized heading-text matches. An absent
// parse generation must be handled as pending by the owner before this call;
// a ready empty list means missing. The returned span is an independent copy.
func ResolveFragment(ctx context.Context, fragment string, headings []Heading) (FragmentResolution, error) {
	if err := ctx.Err(); err != nil {
		return FragmentResolution{}, err
	}
	if err := limitCount("target_bytes", len(fragment), maxTarget); err != nil {
		return FragmentResolution{}, err
	}
	if err := limitCount("headings", len(headings), maxHeadings); err != nil {
		return FragmentResolution{}, err
	}
	if !utf8.ValidString(fragment) || strings.ContainsRune(fragment, 0) {
		return FragmentResolution{}, ErrInvalidInput
	}
	if fragment == "" {
		return FragmentResolution{State: "none"}, nil
	}
	if strings.HasPrefix(fragment, "^") {
		return FragmentResolution{State: "unsupported"}, nil
	}
	b := &budget{}
	// Check every descriptor before scanning or hashing caller-built strings.
	for _, h := range headings {
		for _, text := range []string{h.Anchor, h.Text, h.Markdown} {
			if err := b.reserveStrings(len(text)); err != nil {
				return FragmentResolution{}, err
			}
		}
	}
	exact := -1
	used := stringSet{}
	previousEnd := 0
	for i, h := range headings {
		for field, text := range []string{h.Anchor, h.Text, h.Markdown} {
			if err := validateText(ctx, text, field < 2, b); err != nil {
				if errors.Is(err, ErrInvalidArtifact) {
					return FragmentResolution{}, ErrInvalidInput
				}
				return FragmentResolution{}, err
			}
		}
		if h.Span.Start < previousEnd || h.Span.End < h.Span.Start || h.Level < 1 || h.Level > 6 || h.Anchor == "" {
			return FragmentResolution{}, ErrInvalidInput
		}
		previousEnd = h.Span.End
		duplicate, err := used.add(ctx, h.Anchor, b)
		if err != nil {
			return FragmentResolution{}, err
		}
		if duplicate {
			return FragmentResolution{}, ErrInvalidInput
		}
		if h.Anchor == fragment {
			exact = i
		}
	}
	resolved := func(index int) FragmentResolution {
		span := headings[index].Span
		return FragmentResolution{State: "resolved", Heading: &span}
	}
	if exact >= 0 {
		return resolved(exact), nil
	}
	key, err := headingKey(ctx, fragment, b)
	if err != nil {
		return FragmentResolution{}, err
	}
	match := -1
	for i, h := range headings {
		target, err := headingKey(ctx, h.Text, b)
		if err != nil {
			return FragmentResolution{}, err
		}
		if key == target {
			if match >= 0 {
				return FragmentResolution{State: "ambiguous"}, nil
			}
			match = i
		}
	}
	if match >= 0 {
		return resolved(match), nil
	}
	return FragmentResolution{State: "missing"}, nil
}
