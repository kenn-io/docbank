package kataevidence

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

func TestAppendPendingReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		zero      bool
		existing  bool
		oversized bool
		wantReads int
		wantPosts int
	}{
		{name: "inside_window", age: 6*24*time.Hour - time.Second, existing: true, wantPosts: 1},
		{name: "zero_save_time", age: 7 * 24 * time.Hour, zero: true, existing: true, wantPosts: 1},
		{name: "future_save_time", age: -time.Hour, existing: true, wantPosts: 1},
		{name: "stale_exact_comment", age: 6 * 24 * time.Hour, existing: true, wantReads: 1},
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
			savedAt := issue.UpdatedAt
			if tc.zero {
				savedAt = time.Time{}
			}
			ref := nativeReference()
			entry, err := json.Marshal(Entry{ID: ID(ref), Passage: PassageID(ref, "hello"), Reference: ref, Quote: "hello", Label: "notes.txt", Pending: &Pending{SavedAt: savedAt}})
			require.NoError(t, err)
			env := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}, entries: []jsontext.Value{entry}}
			data, err := env.encode()
			require.NoError(t, err)
			_, err = client.Patch(t.Context(), issue.QualifiedRef, MetadataKey, nil, data)
			require.NoError(t, err)
			if tc.existing {
				key := commentKey(issue.UID, PassageID(ref, "hello"), "Evidence: notes.txt\n\n> hello\n")
				if tc.wantReads > 0 {
					key = "expired-key"
				}
				require.NoError(t, client.Comment(t.Context(), issue.QualifiedRef, key, "Evidence: notes.txt\n\n> hello\n"))
			} else {
				// Similar words under a different label aren't an exact match.
				require.NoError(t, client.Comment(t.Context(), issue.QualifiedRef, "other", "Evidence: other.txt\n\n> hello\n"))
			}
			if tc.oversized {
				for range 34 {
					_, err = api.CreateComment(t.Context(), &generated.CreateCommentRequestOptions{
						PathParams: &generated.CreateCommentPath{ProjectID: strconv.FormatInt(project.Project.ID, 10), Ref: issue.UID},
						Body:       &generated.CreateCommentBody{Actor: new("reader"), Body: strings.Repeat("x", 64<<10)},
					})
					require.NoError(t, err)
				}
			}
			before, err := api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: project.Project.ID, Ref: issue.UID}})
			require.NoError(t, err)
			reads, posts = 0, 0
			s := Service{Kata: client, Archive: &memoryArchive{prepareErr: ErrUnavailable}, Now: func() time.Time { return issue.UpdatedAt.Add(tc.age) }}
			_, err = s.Cite(t.Context(), CiteRequest{Issue: issue.QualifiedRef, Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: ref.Docbank.ContentVersionID}})
			if tc.oversized {
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
			if tc.oversized {
				require.JSONEq(t, string(data), string(saved.Metadata[MetadataKey]))
			} else {
				require.NotContains(t, string(saved.Metadata[MetadataKey]), `"pending"`)
			}
		})
	}
}

func TestAppendReplayRejectsMovedIssue(t *testing.T) {
	server := katatest.New(t, katatest.WithToken("synthetic-key"))
	api := server.Client(t)
	project, err := api.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	client := kataclient.New(config.KataConnection{Endpoint: server.Endpoint, APIKey: "synthetic-key", Project: "docbank"})
	ref := nativeReference()
	entry, err := json.Marshal(Entry{ID: ID(ref), Passage: PassageID(ref, "hello"), Reference: ref, Quote: "hello", Label: "notes.txt", Pending: &Pending{}})
	require.NoError(t, err)
	env := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}, entries: []jsontext.Value{entry}}
	data, err := env.encode()
	require.NoError(t, err)
	issue, err := client.Create(t.Context(), "create", "Follow up", "", map[string]jsontext.Value{MetadataKey: data})
	require.NoError(t, err)
	other, err := api.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("other"), Actor: new("docbank")}})
	require.NoError(t, err)
	moved, err := api.MoveIssue(t.Context(), &generated.MoveIssueRequestOptions{PathParams: &generated.MoveIssuePath{ProjectID: project.Project.ID, Ref: issue.UID}, Body: &generated.MoveIssueBody{Actor: new("docbank"), ToProjectUID: other.Project.UID}, Header: &generated.MoveIssueHeaders{IfMatch: new(`"rev-1"`)}})
	require.NoError(t, err)
	movedRef := "other#" + moved.Issue.ShortID
	s := Service{Kata: client, Archive: &memoryArchive{prepareErr: ErrUnavailable}}
	_, err = s.Cite(t.Context(), CiteRequest{Issue: movedRef, Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: ref.Docbank.ContentVersionID}})
	require.Error(t, err)
	failure, ok := errors.AsType[*kataclient.Error](err)
	require.True(t, ok)
	require.Equal(t, "wrong_project", failure.Code)
	got, err := client.Get(t.Context(), movedRef)
	require.NoError(t, err)
	posted, err := api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: other.Project.ID, Ref: moved.Issue.UID}})
	require.NoError(t, err)
	require.Empty(t, posted.Comments)
	require.Contains(t, string(got.Metadata[MetadataKey]), `"pending"`)
}

func TestDiscussionGrowthAndUnrelatedPurgeKeepFreshAppendReadable(t *testing.T) {
	backend := katatest.New(t, katatest.WithToken("synthetic-key"))
	api := backend.Client(t)
	project, err := api.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	projectReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" {
			projectReads++
		}
		backend.Service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := config.KataConnection{Endpoint: server.URL, APIKey: "synthetic-key", Project: "docbank"}
	client := kataclient.New(cfg)
	issue, err := client.Create(t.Context(), "seed", "Follow up", "", nil)
	require.NoError(t, err)
	// More than 2 MiB of unrelated discussion used to exhaust the issue read.
	for range 34 {
		_, err = api.CreateComment(t.Context(), &generated.CreateCommentRequestOptions{
			PathParams: &generated.CreateCommentPath{ProjectID: strconv.FormatInt(project.Project.ID, 10), Ref: issue.UID},
			Body:       &generated.CreateCommentBody{Actor: new("reader"), Body: strings.Repeat("x", 64<<10)},
		})
		require.NoError(t, err)
	}
	neighbor, err := client.Create(t.Context(), "neighbor", "Neighbor", strings.Repeat("x", 900<<10), nil)
	require.NoError(t, err)
	_, err = api.DeleteIssue(t.Context(), &generated.DeleteIssueRequestOptions{PathParams: &generated.DeleteIssuePath{ProjectID: project.Project.ID, Ref: neighbor.UID}, Body: &generated.DeleteIssueBody{Actor: "docbank"}, Header: &generated.DeleteIssueHeaders{XKataConfirm: new("DELETE " + neighbor.QualifiedRef)}})
	require.NoError(t, err)
	_, err = api.PurgeIssue(t.Context(), &generated.PurgeIssueRequestOptions{PathParams: &generated.PurgeIssuePath{ProjectID: project.Project.ID, Ref: neighbor.UID}, Body: &generated.PurgeIssueBody{Actor: "docbank"}, Header: &generated.PurgeIssueHeaders{XKataConfirm: new("PURGE " + neighbor.QualifiedRef)}})
	require.NoError(t, err)
	s := Service{Kata: kataclient.New(cfg), Archive: &memoryArchive{}}
	_, err = s.Cite(t.Context(), CiteRequest{Issue: issue.QualifiedRef, Selector: Selector{NodeID: 2, EndRune: 5}})
	require.NoError(t, err)
	page, err := s.Context(t.Context(), ContextRequest{Issue: issue.QualifiedRef})
	require.NoError(t, err)
	require.Len(t, page.Passages, 1)
	require.Equal(t, "hello", page.Passages[0].SavedQuote)
	require.Equal(t, "notes.txt", page.Passages[0].Evidence.Label)
	// Restore the pending marker inside the replay window. Even this large
	// discussion must not prevent Kata from replaying the same comment request.
	var env struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(page.Issue.Metadata[MetadataKey], &env))
	env.Entries[0].Pending = &Pending{SavedAt: page.Issue.UpdatedAt}
	pending, err := json.Marshal(env)
	require.NoError(t, err)
	_, err = client.Patch(t.Context(), issue.QualifiedRef, MetadataKey, page.Issue.Metadata[MetadataKey], pending)
	require.NoError(t, err)
	projectReads = 0
	s = Service{Kata: kataclient.New(cfg), Archive: &memoryArchive{prepareErr: ErrUnavailable}}
	got, err := s.Cite(t.Context(), CiteRequest{Issue: issue.QualifiedRef, Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: nativeReference().Docbank.ContentVersionID}})
	require.NoError(t, err)
	require.Equal(t, 1, projectReads, "one project resolution per operation")
	require.NotContains(t, string(got.Metadata[MetadataKey]), `"pending"`)
	posted, err := api.ShowIssue(t.Context(), &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: project.Project.ID, Ref: issue.UID}})
	require.NoError(t, err)
	require.Len(t, posted.Comments, 35, "reconciliation must not post a duplicate")
}
