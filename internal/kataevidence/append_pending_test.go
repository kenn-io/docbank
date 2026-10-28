package kataevidence

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestAppendPendingReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		existing  bool
		edited    bool
		oversized bool
		wantReads int
		wantPosts int
	}{
		{name: "inside_window", age: 6*24*time.Hour - time.Second, existing: true, wantPosts: 1},
		{name: "inside_window_oversized_discussion", age: 6*24*time.Hour - time.Second, existing: true, oversized: true, wantPosts: 1},
		{name: "future_save_time", age: -time.Hour, existing: true, wantPosts: 1},
		{name: "stale_exact_comment", age: 6 * 24 * time.Hour, existing: true, wantReads: 1},
		{name: "stale_edited_comment", age: 6 * 24 * time.Hour, existing: true, edited: true, wantReads: 1},
		{name: "stale_absent_comment", age: 7 * 24 * time.Hour, wantReads: 1, wantPosts: 1},
		{name: "stale_oversized_discussion", age: 7 * 24 * time.Hour, oversized: true, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savedAt := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			ref := nativeReference()
			entry, err := json.Marshal(Entry{ID: ID(ref), Reference: ref, Quote: "hello", Label: "notes.txt", Pending: &Pending{SavedAt: savedAt}})
			require.NoError(t, err)
			env := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}, entries: []jsontext.Value{entry}}
			data, err := env.encode()
			require.NoError(t, err)
			k := newTestKata(t, map[string]jsontext.Value{MetadataKey: data}, "")
			issue := k.snapshot()
			if tc.existing {
				body := "Evidence: notes.txt\n\n> hello\n\n<!-- docbank-evidence:" + ID(ref) + " -->\n"
				key := commentKey(issue.UID, ID(ref), body)
				if tc.wantReads > 0 {
					key = "expired-key"
				}
				if tc.edited {
					body = "Edited discussion text.\n\n<!-- docbank-evidence:" + ID(ref) + " -->\n"
				}
				require.NoError(t, k.Comment(t.Context(), issue.QualifiedRef, key, body))
			} else {
				// Similar words under a different label aren't an exact match.
				require.NoError(t, k.Comment(t.Context(), issue.QualifiedRef, "other", "Evidence: other.txt\n\n> hello\n"))
			}
			if tc.oversized {
				addLargeDiscussion(t, k.api, k.projectID, issue.UID)
			}
			before, err := k.api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: k.projectID, Ref: issue.UID}})
			require.NoError(t, err)
			k.reads, k.posts = 0, 0
			s := Service{Kata: k, Archive: &memoryArchive{prepareErr: ErrUnavailable}, Now: func() time.Time { return savedAt.Add(tc.age) }}
			_, err = s.Cite(t.Context(), CiteRequest{Issue: issue.QualifiedRef, Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: ref.Docbank.ContentVersionID}})
			if tc.oversized && tc.wantReads > 0 {
				require.ErrorIs(t, err, ErrUnsupported)
				require.Contains(t, err.Error(), "stale pending quote can't be verified against the discussion")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantReads, k.reads)
			require.Equal(t, tc.wantPosts, k.posts)
			after, err := k.api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: k.projectID, Ref: issue.UID}})
			require.NoError(t, err)
			wantComments := len(before.Comments)
			if !tc.existing && !tc.oversized {
				wantComments++
			}
			require.Len(t, after.Comments, wantComments)
			saved, err := k.Get(t.Context(), issue.QualifiedRef)
			require.NoError(t, err)
			if tc.oversized && tc.wantReads > 0 {
				require.JSONEq(t, string(data), string(saved.Metadata[MetadataKey]))
			} else {
				require.NotContains(t, string(saved.Metadata[MetadataKey]), `"pending"`)
			}
		})
	}
}

// addLargeDiscussion exceeds the client's 2 MiB discussion read limit.
func addLargeDiscussion(t *testing.T, api *client.Client, projectID int64, ref string) {
	t.Helper()
	for range 34 {
		_, err := api.CreateComment(t.Context(), &generated.CreateCommentRequestOptions{
			PathParams: &generated.CreateCommentPath{ProjectID: strconv.FormatInt(projectID, 10), Ref: ref},
			Body:       &generated.CreateCommentBody{Actor: new("reader"), Body: strings.Repeat("x", 64<<10)},
		})
		require.NoError(t, err)
	}
}
