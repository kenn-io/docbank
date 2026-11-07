package notemarkdown

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestPlanTarget(t *testing.T) {
	cases := []struct {
		form, target, kind, fragment string
		keys                         []LookupKey
	}{
		{"markdown", "../other.md#A", "note", "A", []LookupKey{{"path", "other.md"}}},
		{"wiki", "Other", "note", "", []LookupKey{{"path", "area/Other.md"}, {"basename", "Other"}, {"alias", "Other"}}},
		{"wiki", "/Other", "note", "", []LookupKey{{"path", "Other.md"}}},
		{"wiki", "sub/Other", "note", "", []LookupKey{{"path", "area/sub/Other.md"}}},
		{"wiki", "Other.markdown", "note", "", []LookupKey{{"path", "area/Other.markdown"}}},
		{"wiki", "#A", "note", "A", []LookupKey{{"path", "area/start.md"}}},
		{"markdown", "", "note", "", []LookupKey{{"path", "area/start.md"}}},
		{"markdown", "next", "note", "", []LookupKey{{"path", "area/next"}}},
		{"wiki", ".", "invalid", "", nil},
		{"wiki", "..", "invalid", "", nil},
		{"wiki", "sub/..", "invalid", "", nil},
		{"markdown", "./folder/../next.MD", "note", "", []LookupKey{{"path", "area/next.MD"}}},
		{"markdown", "image.png", "attachment", "", nil},
		{"wiki", "image.png", "attachment", "", nil},
		{"markdown", "https://example.invalid/x", "external", "", nil},
		{"markdown", "mailto:a@example.invalid", "external", "", nil},
		{"markdown", "//example.invalid/x", "external", "", nil},
		{"markdown", "/etc/note.md", "invalid", "", nil},
		{"markdown", "C:/note.md", "invalid", "", nil},
		{"markdown", `C:note.md`, "invalid", "", nil},
		{"markdown", `\\host\file.md`, "invalid", "", nil},
		{"markdown", "../../escape.md", "invalid", "", nil},
		{"markdown", "a%2Fb.md", "invalid", "", nil},
		{"markdown", "a%5Cb.md", "invalid", "", nil},
		{"markdown", "a%00b.md", "invalid", "", nil},
		{"markdown", "a%FF.md", "invalid", "", nil},
		{"markdown", "a%.md", "invalid", "", nil},
		{"markdown", "other.md?q=x", "unsupported", "", nil},
		{"markdown", "other.md#^block", "unsupported", "^block", nil},
		{"markdown", "a%252Fb.md", "note", "", []LookupKey{{"path", "area/a%2Fb.md"}}},
		{"markdown", "a%23b.md#H%20I", "note", "H I", []LookupKey{{"path", "area/a#b.md"}}},
		{"markdown", "a%3Fb.md", "note", "", []LookupKey{{"path", "area/a?b.md"}}},
		{"markdown", `a\b.md`, "note", "", []LookupKey{{"path", `area/a\b.md`}}},
		{"markdown", "Next.md", "note", "", []LookupKey{{"path", "area/Next.md"}}},
		{"markdown", "next.md", "note", "", []LookupKey{{"path", "area/next.md"}}},
		{"markdown", "é.md", "note", "", []LookupKey{{"path", "area/é.md"}}},
		{"markdown", "e\u0301.md", "note", "", []LookupKey{{"path", "area/e\u0301.md"}}},
	}
	for _, tc := range cases {
		t.Run(tc.form+":"+tc.target, func(t *testing.T) {
			q, err := PlanTarget("area/start.md", Link{Form: tc.form, Target: tc.target})
			require.NoError(t, err)
			require.Equal(t, TargetQuery{Kind: tc.kind, Fragment: tc.fragment, Keys: tc.keys}, q)
		})
	}
}
func TestPlanTargetAdmission(t *testing.T) {
	for _, source := range []string{"", "/abs.md", "../x.md", "a/./b.md", "a//b.md", `C:\x.md`, "nul\x00.md", "bad\xff.md"} {
		_, err := PlanTarget(source, Link{Form: "wiki", Target: "X"})
		require.ErrorIs(t, err, ErrInvalidInput)
	}
	_, err := PlanTarget("a.md", Link{Form: "other", Target: "x"})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = PlanTarget("a.md", Link{Form: "wiki", Target: strings.Repeat("a", 4097)})
	requireLimit(t, err, "target_bytes")
	_, err = PlanTarget(strings.Repeat("a", 4097), Link{Form: "wiki", Target: "x"})
	requireLimit(t, err, "target_bytes")
	_, err = PlanTarget(strings.Repeat("a", 4090)+"/a.md", Link{Form: "wiki", Target: "longname"})
	requireLimit(t, err, "target_bytes")
}
func TestPlanTargetNoFilesystem(t *testing.T) {
	q, err := PlanTarget("unmounted/never-created/start.md", Link{Form: "wiki", Target: "../Target"})
	require.NoError(t, err)
	require.Equal(t, []LookupKey{{"path", "unmounted/Target.md"}}, q.Keys)
}
