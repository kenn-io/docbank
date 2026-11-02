package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRebaseKeepsLiteralsAndNumbersPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
		last int
	}{
		{
			name: "quote escape and ordered placeholders",
			in:   "name = ? AND note = 'what?' AND raw = ?? AND other = ?",
			want: "name = $1 AND note = 'what?' AND raw = ? AND other = $2",
			last: 2,
		},
		{
			name: "doubled mark inside a string stays doubled",
			in:   "note = '??' AND a = ?",
			want: "note = '??' AND a = $1",
			last: 1,
		},
		{
			name: "doubled mark does not consume a number",
			in:   "???",
			want: "?$1",
			last: 1,
		},
		{
			name: "continues from the last used index",
			in:   "a = ? AND b = ??",
			n:    3,
			want: "a = $4 AND b = ?",
			last: 4,
		},
		{
			name: "quoted identifier",
			in:   `"col?" = ?`,
			want: `"col?" = $1`,
			last: 1,
		},
		{
			name: "escaped quote",
			in:   "note = 'it''s ?' AND a = ?",
			want: "note = 'it''s ?' AND a = $1",
			last: 1,
		},
		{
			name: "escape string keeps a backslash quote",
			in:   `note = E'it\'s a ?' AND a = ?`,
			want: `note = E'it\'s a ?' AND a = $1`,
			last: 1,
		},
		{
			name: "dollar quote",
			in:   "$$what?$$ = ?",
			want: "$$what?$$ = $1",
			last: 1,
		},
		{
			name: "tagged dollar quote",
			in:   "$tag$what?$tag$ = ?",
			want: "$tag$what?$tag$ = $1",
			last: 1,
		},
		{
			name: "line comment",
			in:   "a = ? -- what?\nAND b = ?",
			want: "a = $1 -- what?\nAND b = $2",
			last: 2,
		},
		{
			name: "block comment",
			in:   "a = ? /* what? */ AND b = ?",
			want: "a = $1 /* what? */ AND b = $2",
			last: 2,
		},
		{
			name: "nested block comment",
			in:   "a = ? /* outer /* inner */ still? */ AND b = ?",
			want: "a = $1 /* outer /* inner */ still? */ AND b = $2",
			last: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, last := rebase(tt.in, tt.n)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.last, last)
		})
	}
}

func TestLexicalConfigAllowsOneSchemaQualifier(t *testing.T) {
	base := LexicalRequest{
		Mapping:        LexicalMapping{SourceTable: "docs", SourceKey: "id", Vector: "d.body_tsv"},
		Text:           "alpha",
		CandidateLimit: 1,
	}
	accepted := base
	accepted.Config = "pg_catalog.simple"
	q, err := BuildLexical(accepted)
	require.NoError(t, err)
	assert.Contains(t, q.SQL, "plainto_tsquery($1::regconfig, $2)")
	assert.NotContains(t, q.SQL, "pg_catalog.simple")
	assert.Equal(t, []any{"pg_catalog.simple", "alpha", 1}, q.Args)

	for _, cfg := range []string{"a.b.c", "simple;drop"} {
		rejected := base
		rejected.Config = cfg
		_, err := BuildLexical(rejected)
		require.Error(t, err, cfg)
	}
}
