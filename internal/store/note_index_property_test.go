//go:build !darwin || arm64

package store

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// This ledger is an authored relation, not an invocation of the selector,
// SQL builder or resolver under test. Each query must expose the first two
// distinct bindings per key and preserve all their matching queried keys.
func TestNoteIndexEvidencePermutationProperty(t *testing.T) {
	type row struct {
		ref, body string
		keys      []NoteLookupKey
	}
	ledger := []row{
		{"a.md", "---\naliases: [red, common]\n---\nnote", []NoteLookupKey{{"path", "a.md"}, {"basename", "a"}, {"alias", "red"}, {"alias", "common"}}},
		{"b.md", "---\naliases: [red, common]\n---\nnote", []NoteLookupKey{{"path", "b.md"}, {"basename", "b"}, {"alias", "red"}, {"alias", "common"}}},
		{"c.md", "---\naliases: [green, common]\n---\nnote", []NoteLookupKey{{"path", "c.md"}, {"basename", "c"}, {"alias", "green"}, {"alias", "common"}}},
		{"d.md", "---\naliases: [green]\n---\nnote", []NoteLookupKey{{"path", "d.md"}, {"basename", "d"}, {"alias", "green"}}},
		{"e.md", "---\naliases: [blue]\n---\nnote", []NoteLookupKey{{"path", "e.md"}, {"basename", "e"}, {"alias", "blue"}}},
		{"f.md", "---\naliases: [blue]\n---\nnote", []NoteLookupKey{{"path", "f.md"}, {"basename", "f"}, {"alias", "blue"}}},
		{"g.md", "---\naliases: [紫]\n---\nnote", []NoteLookupKey{{"path", "g.md"}, {"basename", "g"}, {"alias", "紫"}}},
		{"h.md", "---\naliases: [紫]\n---\nnote", []NoteLookupKey{{"path", "h.md"}, {"basename", "h"}, {"alias", "紫"}}},
	}
	s := newTestStore(t)
	specs := make([]indexNoteSpec, len(ledger))
	for i, r := range ledger {
		specs[i] = indexNoteSpec{ref: r.ref, body: r.body, keys: r.keys}
	}
	_, notes := noteIndexFixture(t, s, specs...)
	g := beginIndexFixture(t, s)
	stageIndexFixture(t, s, g, notes)
	all := []NoteLookupKey{{"alias", "red"}, {"alias", "green"}, {"alias", "blue"}, {"alias", "紫"}, {"alias", "common"}, {"alias", "missing"}, {"alias", "Red"}, {"path", "a.md"}, {"path", "missing.md"}, {"basename", "a"}, {"basename", "h"}, {"alias", "a"}}
	// Four independent keys can legitimately yield all eight bounded rows.
	full, err := s.NoteIndexCandidates(t.Context(), g, all[:4])
	require.NoError(t, err)
	require.Len(t, full, 8)
	hegel.Test(t, func(ht *hegel.T) {
		drawn := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, len(all)-1)))
		keys := []NoteLookupKey{}
		for _, index := range drawn {
			if !slices.Contains(keys, all[index]) {
				keys = append(keys, all[index])
			}
			if len(keys) == 4 {
				break
			}
		}
		want := map[string][]NoteLookupKey{}
		for _, key := range keys {
			matches := 0
			for _, r := range ledger {
				if slices.Contains(r.keys, key) {
					want[r.ref] = append(want[r.ref], key)
					matches++
					if matches == 2 {
						break
					}
				}
			}
		}
		normalize := func(m map[string][]NoteLookupKey) {
			for ref := range m {
				slices.SortFunc(m[ref], func(a, b NoteLookupKey) int {
					if a.Kind < b.Kind {
						return -1
					}
					if a.Kind > b.Kind {
						return 1
					}
					if a.Value < b.Value {
						return -1
					}
					if a.Value > b.Value {
						return 1
					}
					return 0
				})
			}
		}
		normalize(want)
		bindings := map[string]string{}
		for permutation := 0; permutation < 2; permutation++ {
			rows, err := s.NoteIndexCandidates(ht.Context(), g, keys)
			require.NoError(ht, err)
			require.LessOrEqual(ht, len(rows), 8)
			got := map[string][]NoteLookupKey{}
			used := map[string]bool{}
			for _, r := range rows {
				require.Equal(ht, "note", r.Kind)
				require.NotEmpty(ht, r.Binding)
				require.False(ht, used[r.Binding], "one row per exact binding")
				used[r.Binding] = true
				if previous, ok := bindings[r.SourceRef]; ok {
					require.Equal(ht, previous, r.Binding)
				} else {
					bindings[r.SourceRef] = r.Binding
				}
				got[r.SourceRef] = r.Keys
			}
			normalize(got)
			require.Equal(ht, want, got)
			slices.Reverse(keys)
		}
	}, hegel.WithDatabase(t.TempDir()), hegel.WithTestCases(2000))
}
