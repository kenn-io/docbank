package kataevidence

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAppendRejectsConflictingSavedQuote(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			name := "completed"
			if pending {
				name = "pending"
			}
			if pinned {
				name += "_explicit_quote"
			}
			t.Run(name, func(t *testing.T) {
				ref := nativeReference()
				entry := Entry{ID: ID(ref), Reference: ref, Quote: "wrong", Label: "notes.txt"}
				if pending {
					entry.Pending = &Pending{SavedAt: time.Now()}
				}
				metadata, err := json.Marshal(map[string]any{"version": 1, "entries": []Entry{entry}})
				require.NoError(t, err)
				k := newTestKata(t, map[string]jsontext.Value{MetadataKey: metadata}, "")
				s := Service{Kata: k, Archive: &memoryArchive{}}
				selector := Selector{NodeID: 2, EndRune: 5}
				if pinned {
					selector.ExpectedVersion, selector.Quote = ref.Docbank.ContentVersionID, "hello"
				}

				_, err = s.Cite(t.Context(), CiteRequest{Issue: k.ref, Selector: selector})

				require.ErrorIs(t, err, ErrConflict)
				require.JSONEq(t, string(metadata), string(k.snapshot().Metadata[MetadataKey]))
				require.Zero(t, k.posts)
				require.Zero(t, k.commentCount())
			})
		}
	}
}

func TestAppendRejectsQuoteChangedDuringGuardedPatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reconcile bool
		pinned    bool
		completed bool
	}{
		{name: "append"},
		{name: "reconcile", reconcile: true},
		{name: "pinned_retry", reconcile: true, pinned: true},
		{name: "completed_on_reload", reconcile: true, completed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := nativeReference()
			entry := Entry{ID: ID(ref), Reference: ref, Quote: "hello", Label: "notes.txt", Pending: &Pending{SavedAt: time.Now()}}
			entries := []Entry{}
			if tc.reconcile {
				entries = append(entries, entry)
			}
			metadata, err := json.Marshal(map[string]any{"version": 1, "entries": entries})
			require.NoError(t, err)
			k := newTestKata(t, map[string]jsontext.Value{MetadataKey: metadata}, "")
			entry.Quote = "wrong"
			if tc.completed {
				entry.Pending = nil
			}
			k.concurrentMetadata, err = json.Marshal(map[string]any{"version": 1, "entries": []Entry{entry}})
			require.NoError(t, err)
			k.conflictOnce = true
			a := &memoryArchive{}
			selector := Selector{NodeID: 2, EndRune: 5}
			if tc.pinned {
				selector.ExpectedVersion = ref.Docbank.ContentVersionID
				a.prepareErr = ErrUnavailable
			}
			s := Service{Kata: k, Archive: a}

			_, err = s.Cite(t.Context(), CiteRequest{Issue: k.ref, Selector: selector})

			require.ErrorIs(t, err, ErrConflict)
			require.JSONEq(t, string(k.concurrentMetadata), string(k.snapshot().Metadata[MetadataKey]))
			comments, err := k.GetComments(t.Context(), k.ref)
			require.NoError(t, err)
			if tc.reconcile {
				require.Len(t, comments, 1)
				require.Contains(t, comments[0], "> hello\n")
				require.NotContains(t, comments[0], "wrong")
				require.Equal(t, 1, k.posts)
			} else {
				require.Empty(t, comments)
				require.Zero(t, k.posts)
			}
		})
	}
}
