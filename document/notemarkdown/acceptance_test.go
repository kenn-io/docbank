package notemarkdown

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcceptanceVersionBoundInputs(t *testing.T) {
	ctx := t.Context()
	a, err := Parse(ctx, []byte("# A\n[[Target#Old]]\n"))
	require.NoError(t, err)
	b, err := Parse(ctx, []byte("# B\n[[Target#New]]\n"))
	require.NoError(t, err)
	again, err := Parse(ctx, []byte("# A\n[[Target#Old]]\n"))
	require.NoError(t, err)
	ca, err := CanonicalArtifact(ctx, a)
	require.NoError(t, err)
	cb, err := CanonicalArtifact(ctx, b)
	require.NoError(t, err)
	cc, err := CanonicalArtifact(ctx, again)
	require.NoError(t, err)
	require.Equal(t, ca, cc)
	require.NotEqual(t, ca, cb)
	q, err := PlanTarget("Source.md", a.Links[0])
	require.NoError(t, err)
	oldTarget, err := Parse(ctx, []byte("# Old\n"))
	require.NoError(t, err)
	newTarget, err := Parse(ctx, []byte("# New\n"))
	require.NoError(t, err)
	for _, tc := range []struct {
		revision, binding string
		headings          []Heading
		fragmentState     string
	}{
		{"rev-a", "target-v1", oldTarget.Headings, "resolved"},
		{"rev-b", "target-v2", newTarget.Headings, "missing"},
		{"rev-c", "target-v3", oldTarget.Headings, "resolved"},
	} {
		r, err := Resolve(ctx, q, CandidateSet{NamespaceGeneration: tc.revision, Complete: true, Candidates: []Candidate{{Binding: tc.binding, Path: "Target.md", Kind: "note", Keys: []LookupKey{q.Keys[0]}}}})
		require.NoError(t, err)
		require.Equal(t, tc.binding, r.Binding)
		require.Equal(t, tc.revision, r.NamespaceGeneration)
		f, err := ResolveFragment(ctx, q.Fragment, tc.headings)
		require.NoError(t, err)
		require.Equal(t, tc.fragmentState, f.State)
	}
}

func TestAcceptanceNamespaceChanges(t *testing.T) {
	ctx := t.Context()
	q, err := PlanTarget("start.md", Link{Form: "wiki", Target: "Topic"})
	require.NoError(t, err)
	row := Candidate{Binding: "v1", Path: "Topic.md", Kind: "note", Keys: []LookupKey{q.Keys[0], q.Keys[1]}}
	renamed := Candidate{Binding: "v1", Path: "else/Renamed.md", Kind: "note", Keys: []LookupKey{q.Keys[2]}}
	duplicate := Candidate{Binding: "other-v1", Path: "second/Topic.md", Kind: "note", Keys: []LookupKey{q.Keys[1]}}
	for _, tc := range []struct {
		name           string
		complete       bool
		rows           []Candidate
		state, binding string
	}{
		{"present", true, []Candidate{row}, "resolved", "v1"},
		{"removed", true, nil, "missing", ""},
		{"reappeared", true, []Candidate{{Binding: "v2", Path: "Topic.md", Kind: "note", Keys: row.Keys}}, "resolved", "v2"},
		{"rename-alias", true, []Candidate{renamed}, "resolved", "v1"},
		{"equal-content-other-identity", true, []Candidate{renamed, duplicate}, "ambiguous", ""},
		{"index-pending", false, []Candidate{renamed}, "pending", ""},
		{"terminal-empty", true, nil, "missing", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Resolve(ctx, q, CandidateSet{NamespaceGeneration: tc.name, Complete: tc.complete, Candidates: tc.rows})
			require.NoError(t, err)
			require.Equal(t, Resolution{State: tc.state, Binding: tc.binding, NamespaceGeneration: tc.name}, r)
		})
	}
}

// Cancellation becomes visible after deterministic checks during work. This
// avoids a race between a test timer and the parser's work checkpoints.
type cancellationChecks struct {
	context.Context

	remaining int
}

func (c *cancellationChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestAcceptanceResourceScale(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct{ name, src, resource string }{
		{"depth", strings.Repeat("[", 129), "ast_depth"},
		{"nodes", strings.Repeat("[]", 262144), "ast_nodes"},
		{"work", "text " + strings.Repeat("<x ", 12000), "work_steps"},
		{"strings", strings.Repeat("[x][r] ", 8192) + "\n\n[r]: " + strings.Repeat("a", 4096), "derived_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := Parse(ctx, []byte(tc.src))
			requireLimit(t, err, tc.resource)
			require.Equal(t, Note{}, n)
		})
	}
	c := &cancellationChecks{Context: ctx, remaining: 10}
	n, err := Parse(c, []byte(strings.Repeat("a", 100000)))
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, Note{}, n)
	require.Zero(t, c.remaining)
	// Exact maximum source, fragment, opaque identities and candidate row count.
	source := strings.Repeat("a", 4093) + ".md"
	q, err := PlanTarget(source, Link{Form: "markdown", Target: "#" + strings.Repeat("x", 4095)})
	require.NoError(t, err)
	require.Equal(t, source, q.Keys[0].Value)
	binding := strings.Repeat("b", 4096)
	generation := strings.Repeat("g", 4096)
	r, err := Resolve(ctx, q, CandidateSet{NamespaceGeneration: generation, Complete: true, Candidates: []Candidate{{Binding: binding, Path: source, Kind: "note", Keys: q.Keys}}})
	require.NoError(t, err)
	require.Equal(t, binding, r.Binding)
	q = TargetQuery{Kind: "note", Keys: []LookupKey{{"alias", "A"}, {"alias", "B"}, {"alias", "C"}, {"alias", "D"}}}
	var rows []Candidate
	for i, key := range q.Keys {
		for j := range 2 {
			rows = append(rows, Candidate{Binding: fmt.Sprintf("b%d-%d", i, j), Path: fmt.Sprintf("n%d-%d.md", i, j), Kind: "note", Keys: []LookupKey{key}})
		}
	}
	r, err = Resolve(ctx, q, CandidateSet{Complete: true, Candidates: rows})
	require.NoError(t, err)
	require.Equal(t, "ambiguous", r.State)
	n, err = Parse(ctx, []byte(strings.Repeat("# H\n", 4096)))
	require.NoError(t, err)
	require.Len(t, n.Headings, 4096)
	f, err := ResolveFragment(ctx, n.Headings[4095].Anchor, n.Headings)
	require.NoError(t, err)
	require.Equal(t, &n.Headings[4095].Span, f.Heading)
	encoded, err := CanonicalArtifact(ctx, n)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)
	n, err = Parse(ctx, []byte("# "+strings.Repeat("h", 4096)+"\n"))
	require.NoError(t, err)
	f, err = ResolveFragment(ctx, strings.Repeat("h", 4096), n.Headings)
	require.NoError(t, err)
	require.Equal(t, "resolved", f.State)
}

func TestHeadingDisplayCommonMarkTokens(t *testing.T) {
	for _, tc := range []struct{ markdown, text string }{
		{"a_b c_d", "a_b c_d"},
		{"**bold** _emphasis_ *soft* ~~literal~~", "bold emphasis soft ~~literal~~"},
		{"`` a ` b ``", "a ` b"},
		{"`a``b`", "a``b"},
		{"[outer **label**](n(a).md)", "outer label"},
		{"[[Target|Visible]]", "Visible"},
		{"<https://example.invalid>", "https://example.invalid"},
	} {
		t.Run(tc.markdown, func(t *testing.T) {
			n, err := Parse(t.Context(), []byte("# "+tc.markdown+"\n"))
			require.NoError(t, err)
			require.Equal(t, tc.text, n.Headings[0].Text)
		})
	}
}
