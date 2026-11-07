package notemarkdown

import (
	"context"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestResolveEvidenceStates(t *testing.T) {
	q, err := PlanTarget("area/start.md", Link{Form: "wiki", Target: "Other"})
	require.NoError(t, err)
	row := func(binding, path string, keys ...LookupKey) Candidate {
		return Candidate{Binding: binding, Path: path, Kind: "note", Keys: keys}
	}
	a := row("binding-a", "else/Other.md", q.Keys[1])
	b := row("binding-b", "other/B.md", q.Keys[2])
	exact := row("binding-exact", "area/Other.md", q.Keys[0])
	cases := []struct {
		name           string
		complete       bool
		rows           []Candidate
		state, binding string
	}{
		{"empty-ready", true, nil, "missing", ""},
		{"empty-pending", false, nil, "pending", ""},
		{"one", true, []Candidate{a}, "resolved", "binding-a"},
		{"incomplete-one", false, []Candidate{a}, "pending", ""},
		{"incomplete-exact", false, []Candidate{exact}, "pending", ""},
		{"fallback-conflict", true, []Candidate{a, b}, "ambiguous", ""},
		{"same-binding", true, []Candidate{a, row("binding-a", "else/Other.md", q.Keys[2])}, "resolved", "binding-a"},
		{"preferred", true, []Candidate{a, b, exact}, "resolved", "binding-exact"},
		{"attachment", true, []Candidate{{Binding: "file", Path: "area/Other.md", Kind: "attachment", Keys: []LookupKey{q.Keys[0]}}}, "attachment", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Resolve(t.Context(), q, CandidateSet{NamespaceGeneration: "rev-7", Complete: tc.complete, Candidates: tc.rows})
			require.NoError(t, err)
			require.Equal(t, Resolution{State: tc.state, Binding: tc.binding, NamespaceGeneration: "rev-7"}, r)
		})
	}
	for _, kind := range []string{"external", "invalid", "unsupported", "attachment"} {
		r, err := Resolve(t.Context(), TargetQuery{Kind: kind}, CandidateSet{NamespaceGeneration: "rev-7"})
		require.NoError(t, err)
		require.Equal(t, Resolution{State: kind, NamespaceGeneration: "rev-7"}, r)
	}
}

func TestResolveRejectsMalformedSet(t *testing.T) {
	q := TargetQuery{Kind: "note", Keys: []LookupKey{{"path", "area/X.md"}, {"alias", "X"}}}
	valid := Candidate{Binding: "a", Path: "area/X.md", Kind: "note", Keys: []LookupKey{q.Keys[0]}}
	cases := [][]Candidate{
		make([]Candidate, 9),
		{{Binding: "a", Path: "area/X.md", Kind: "note", Keys: make([]LookupKey, 5)}},
		{{Binding: "", Path: "area/X.md", Kind: "note", Keys: []LookupKey{q.Keys[0]}}},
		{{Binding: "a", Path: "../X.md", Kind: "note", Keys: []LookupKey{q.Keys[0]}}},
		{{Binding: "a", Path: "area/Y.md", Kind: "note", Keys: []LookupKey{q.Keys[0]}}},
		{{Binding: "a", Path: "area/X.md", Kind: "unknown", Keys: []LookupKey{q.Keys[0]}}},
		{{Binding: "a", Path: "area/X.md", Kind: "note", Keys: nil}},
		{{Binding: "a", Path: "area/X.md", Kind: "note", Keys: []LookupKey{{"alias", "foreign"}}}},
		{valid, {Binding: "a", Path: "area/Y.md", Kind: "note", Keys: []LookupKey{q.Keys[1]}}},
		{valid, {Binding: "a", Path: "area/X.md", Kind: "attachment", Keys: []LookupKey{q.Keys[1]}}},
		{{Binding: "a", Path: "a.md", Kind: "note", Keys: []LookupKey{q.Keys[1]}}, {Binding: "b", Path: "b.md", Kind: "note", Keys: []LookupKey{q.Keys[1]}}, {Binding: "c", Path: "c.md", Kind: "note", Keys: []LookupKey{q.Keys[1]}}},
	}
	for i, rows := range cases {
		r, err := Resolve(t.Context(), q, CandidateSet{Complete: true, Candidates: rows})
		require.ErrorIs(t, err, ErrInvalidCandidateSet, "case %d", i)
		require.Equal(t, Resolution{}, r)
	}
	_, err := Resolve(t.Context(), TargetQuery{Kind: "note", Keys: make([]LookupKey, 5)}, CandidateSet{})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = Resolve(t.Context(), TargetQuery{Kind: "typo"}, CandidateSet{})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestResolveFragmentPrecedence(t *testing.T) {
	n, err := Parse(t.Context(), []byte("# A\n# A\n# A-1\n# Different heading\n"))
	require.NoError(t, err)
	cases := []struct {
		fragment, state string
		index           int
	}{
		{"a-1", "resolved", 1}, {"A!", "ambiguous", -1}, {"Different Heading!", "resolved", 3},
		{"missing", "missing", -1}, {"", "none", -1}, {"^block", "unsupported", -1},
	}
	for _, tc := range cases {
		r, err := ResolveFragment(t.Context(), tc.fragment, n.Headings)
		require.NoError(t, err)
		require.Equal(t, tc.state, r.State)
		if tc.index >= 0 {
			require.Equal(t, &n.Headings[tc.index].Span, r.Heading)
		} else {
			require.Nil(t, r.Heading)
		}
	}
	r, err := ResolveFragment(t.Context(), "a-1", n.Headings)
	require.NoError(t, err)
	n.Headings[1].Span.End++
	require.NotEqual(t, n.Headings[1].Span, *r.Heading, "result must not alias caller slice")
	_, err = ResolveFragment(t.Context(), "x", make([]Heading, 4097))
	requireLimit(t, err, "headings")
	_, err = ResolveFragment(t.Context(), strings.Repeat("x", 4097), nil)
	requireLimit(t, err, "target_bytes")
}

func TestResolveCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Resolve(ctx, TargetQuery{Kind: "external"}, CandidateSet{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = ResolveFragment(ctx, "", nil)
	require.ErrorIs(t, err, context.Canceled)
}
