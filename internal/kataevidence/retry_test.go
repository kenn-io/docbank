package kataevidence

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
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
			backend := katatest.New(t, katatest.WithToken("synthetic-key"))
			api := backend.Client(t)
			project, err := api.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
			require.NoError(t, err)
			reads, posts := 0, 0
			var issue kataclient.Issue
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/"+issue.ShortID) {
					reads++
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
					posts++
				}
				backend.Service.Handler().ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			client := kataclient.New(config.KataConnection{Endpoint: server.URL, APIKey: "synthetic-key", Project: "docbank"})
			issue, err = client.Create(t.Context(), "create", "Follow up", "", nil)
			require.NoError(t, err)
			savedAt := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
			ref := nativeReference()
			entry, err := json.Marshal(Entry{ID: ID(ref), Reference: ref, Quote: "hello", Label: "notes.txt", Pending: &Pending{SavedAt: savedAt}})
			require.NoError(t, err)
			env := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}, entries: []jsontext.Value{entry}}
			data, err := env.encode()
			require.NoError(t, err)
			_, err = client.Patch(t.Context(), issue.QualifiedRef, MetadataKey, nil, data)
			require.NoError(t, err)
			if tc.existing {
				body := "Evidence: notes.txt\n\n> hello\n\n<!-- docbank-evidence:" + ID(ref) + " -->\n"
				key := commentKey(issue.UID, ID(ref), body)
				if tc.wantReads > 0 {
					key = "expired-key"
				}
				if tc.edited {
					body = "Edited discussion text.\n\n<!-- docbank-evidence:" + ID(ref) + " -->\n"
				}
				require.NoError(t, client.Comment(t.Context(), issue.QualifiedRef, key, body))
			} else {
				// Similar words under a different label aren't an exact match.
				require.NoError(t, client.Comment(t.Context(), issue.QualifiedRef, "other", "Evidence: other.txt\n\n> hello\n"))
			}
			if tc.oversized {
				addLargeDiscussion(t, api, project.Project.ID, issue.UID)
			}
			before, err := api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: project.Project.ID, Ref: issue.UID}})
			require.NoError(t, err)
			reads, posts = 0, 0
			s := Service{Kata: client, Archive: &memoryArchive{prepareErr: ErrUnavailable}, Now: func() time.Time { return savedAt.Add(tc.age) }}
			_, err = s.Cite(t.Context(), CiteRequest{Issue: issue.QualifiedRef, Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: ref.Docbank.ContentVersionID}})
			if tc.oversized && tc.wantReads > 0 {
				require.ErrorIs(t, err, ErrUnsupported)
				require.Contains(t, err.Error(), "stale pending quote can't be verified against the discussion")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantReads, reads)
			require.Equal(t, tc.wantPosts, posts)
			after, err := api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: project.Project.ID, Ref: issue.UID}})
			require.NoError(t, err)
			wantComments := len(before.Comments)
			if !tc.existing && !tc.oversized {
				wantComments++
			}
			require.Len(t, after.Comments, wantComments)
			saved, err := client.Get(t.Context(), issue.QualifiedRef)
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
