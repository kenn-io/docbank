package kataevidence

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestAppendLeavesUnrelatedStalePendingInLargeDiscussion(t *testing.T) {
	old := nativeReference()
	old.Docbank.NodeID = 3
	entry := Entry{ID: ID(old), Passage: PassageID(old, "older"), Reference: old, Quote: "older", Label: "older.txt", Pending: &Pending{SavedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}}
	data, err := json.Marshal(map[string]any{"version": 1, "entries": []Entry{entry}})
	require.NoError(t, err)
	k := newTestKata(t, map[string]jsontext.Value{MetadataKey: data}, "")
	addLargeDiscussion(t, k.api, k.projectID, k.ref)
	s := Service{Kata: k, Archive: &memoryArchive{}}
	for _, pinned := range []bool{false, true} {
		selector := Selector{NodeID: 2, EndRune: 5}
		if pinned {
			selector.ExpectedVersion = nativeReference().Docbank.ContentVersionID
		}
		_, err = s.Cite(t.Context(), CiteRequest{Issue: k.ref, Selector: selector})
		require.NoError(t, err)
	}
	var saved struct {
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(k.snapshot().Metadata[MetadataKey], &saved))
	require.Len(t, saved.Entries, 2)
	require.Equal(t, entry, saved.Entries[0])
	require.Nil(t, saved.Entries[1].Pending)
	posted, err := k.api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: k.projectID, Ref: k.ref}})
	require.NoError(t, err)
	require.Len(t, posted.Comments, 35)
	require.Equal(t, "Evidence: notes.txt\n\n> hello\n", posted.Comments[34].Body)
}

func TestAppendIdenticalWordsFromNewVersionPostsComment(t *testing.T) {
	k := newTestKata(t, map[string]jsontext.Value{}, "")
	a := &memoryArchive{}
	s := Service{Kata: k, Archive: a}
	r := CiteRequest{Issue: k.ref, Selector: Selector{NodeID: 2, EndRune: 5}}
	_, err := s.Cite(t.Context(), r)
	require.NoError(t, err)
	ref := nativeReference()
	ref.Docbank.ContentVersionID = "33333333-3333-4333-8333-333333333333"
	evidence := NewEvidence(ref, "hello", "notes.txt")
	a.evidence = &evidence
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	var saved struct {
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(k.snapshot().Metadata[MetadataKey], &saved))
	require.Len(t, saved.Entries, 2)
	require.NotEqual(t, saved.Entries[0].ID, saved.Entries[1].ID)
	require.Equal(t, 2, k.commentCount())
	_, err = s.Cite(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, 2, k.commentCount())
}
