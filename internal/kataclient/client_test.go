package kataclient

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/kata/pkg/client"
	wire "go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

func realClient(t *testing.T) (*Client, *client.Client, int64) {
	t.Helper()
	server := katatest.New(t, katatest.WithToken("synthetic-key"))
	api := server.Client(t)
	project, err := api.InitProject(t.Context(), &wire.InitProjectRequestOptions{Body: &wire.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	return New(config.KataConnection{Endpoint: server.Endpoint, APIKey: "synthetic-key", Project: "docbank"}), api, project.Project.ID
}

func TestCreateAndCommentReplay(t *testing.T) {
	c, api, projectID := realClient(t)
	issue, err := c.Create(t.Context(), "create-key", "Synthetic issue", "Details", map[string]jsontext.Value{"docbank.action": jsontext.Value(`"marker"`)})
	require.NoError(t, err)
	replay, err := c.Create(t.Context(), "create-key", "Synthetic issue", "Details", map[string]jsontext.Value{"docbank.action": jsontext.Value(`"marker"`)})
	require.NoError(t, err)
	require.Equal(t, issue.UID, replay.UID)
	require.Equal(t, "Details", issue.Body)
	require.Equal(t, `"marker"`, string(issue.Metadata["docbank.action"]))
	for range 2 {
		require.NoError(t, c.Comment(t.Context(), issue.QualifiedRef, "comment-key", "Evidence added"))
	}
	got, err := api.ShowIssue(t.Context(), &wire.ShowIssueRequestOptions{PathParams: &wire.ShowIssuePath{ProjectID: projectID, Ref: issue.UID}})
	require.NoError(t, err)
	require.Len(t, got.Comments, 1)
	require.Equal(t, "Evidence added", got.Comments[0].Body)
	marker, err := c.FindMarker(t.Context(), "marker")
	require.NoError(t, err)
	require.NotNil(t, marker)
	require.Equal(t, issue.UID, marker.UID)
}

func TestPatchGuardsPreserveConcurrentMetadata(t *testing.T) {
	c, _, _ := realClient(t)
	issue, err := c.Create(t.Context(), "create-key", "Synthetic issue", "", map[string]jsontext.Value{"unrelated": jsontext.Value(`"keep"`), "future": jsontext.Value(`9007199254740993`)})
	require.NoError(t, err)
	first, err := c.Patch(t.Context(), issue.QualifiedRef, "docbank.evidence", nil, jsontext.Value(`{"version":1,"entries":[]}`))
	require.NoError(t, err)
	_, err = c.Patch(t.Context(), issue.QualifiedRef, "docbank.evidence", nil, jsontext.Value(`[]`))
	require.True(t, IsGuardConflict(err), "%v", err)
	_, err = c.Patch(t.Context(), issue.QualifiedRef, "docbank.evidence", jsontext.Value(`{"version":2,"entries":[]}`), jsontext.Value(`[]`))
	require.True(t, IsGuardConflict(err), "%v", err)
	final, err := c.Patch(t.Context(), issue.QualifiedRef, "docbank.evidence", first.Metadata["docbank.evidence"], jsontext.Value(`{"version":1,"entries":[{"future":true}]}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"version":1,"entries":[{"future":true}]}`, string(final.Metadata["docbank.evidence"]))
	require.Equal(t, `"keep"`, string(final.Metadata["unrelated"]))
	require.Equal(t, `9007199254740993`, string(final.Metadata["future"]))
	require.False(t, IsGuardConflict(&Error{Status: 409, Code: "idempotency_conflict"}))
}

func TestFindMarkerFollowsMovedIssue(t *testing.T) {
	c, api, id := realClient(t)
	issue, err := c.Create(t.Context(), "create-key", "Synthetic issue", "", map[string]jsontext.Value{"docbank.action": jsontext.Value(`"marker"`)})
	require.NoError(t, err)
	project, err := api.InitProject(t.Context(), &wire.InitProjectRequestOptions{Body: &wire.InitProjectBody{Name: new("context"), Actor: new("docbank")}})
	require.NoError(t, err)
	_, err = api.MoveIssue(t.Context(), &wire.MoveIssueRequestOptions{PathParams: &wire.MoveIssuePath{ProjectID: id, Ref: issue.UID}, Body: &wire.MoveIssueBody{Actor: new("docbank"), ToProjectUID: project.Project.UID}, Header: &wire.MoveIssueHeaders{IfMatch: new(`"rev-1"`)}})
	require.NoError(t, err)
	moved, err := c.FindMarker(t.Context(), "marker")
	require.NoError(t, err)
	require.NotNil(t, moved)
	require.True(t, strings.HasPrefix(moved.QualifiedRef, "context#"))
	require.Equal(t, issue.UID, moved.UID)
	got, err := c.Get(t.Context(), moved.QualifiedRef)
	require.NoError(t, err)
	require.Equal(t, moved.UID, got.UID)
}

func TestInvalidReferencesAndCrossProjectWritesDoNotSendRequests(t *testing.T) {
	c := New(config.KataConnection{Endpoint: "https://example.com", APIKey: "synthetic-key", Project: "docbank"})
	var requests atomic.Int32
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected transport request")
	})
	for _, ref := range []string{"", "../abcd", "a/b", "a%2fb", "a?x", "a#b#c", "a\\b", "a\x00b", ".", ".."} {
		_, err := c.Get(t.Context(), ref)
		require.Error(t, err)
	}
	_, err := c.Patch(t.Context(), "context#abcd", "k", nil, jsontext.Value(`[]`))
	require.Error(t, err)
	require.Error(t, c.Comment(t.Context(), "context#abcd", "key", "body"))
	_, err = c.Create(t.Context(), "", "title", "body", nil)
	require.Error(t, err)
	require.Error(t, c.Comment(t.Context(), "abcd", "", "body"))
	require.Zero(t, requests.Load())
}

func TestClientClassifiesKataAuthenticationAndMissingIssue(t *testing.T) {
	c, _, _ := realClient(t)
	_, err := c.Get(t.Context(), "missing")
	failure, ok := errors.AsType[*Error](err)
	require.True(t, ok)
	require.Equal(t, 404, failure.Status)
	require.NotContains(t, err.Error(), "missing")
	server := katatest.New(t, katatest.WithToken("different-key"))
	c = New(config.KataConnection{Endpoint: server.Endpoint, APIKey: "synthetic-key", Project: "docbank"})
	_, err = c.Get(t.Context(), "missing")
	failure, ok = errors.AsType[*Error](err)
	require.True(t, ok)
	require.Equal(t, 403, failure.Status)
	require.NotContains(t, err.Error(), "synthetic-key")
}

func TestClientBoundsRequests(t *testing.T) {
	c, api, _ := realClient(t)
	_, err := c.Create(t.Context(), "key", "title", strings.Repeat("x", 1<<20), nil)
	require.Error(t, err)
	list, err := api.ListAllIssues(t.Context(), &wire.ListAllIssuesRequestOptions{})
	require.NoError(t, err)
	require.Empty(t, list.Issues)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientCancellationKeepsSafeCause(t *testing.T) {
	c, _, _ := realClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Get(ctx, "abcd")
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "127.0.0.1")
}

func TestClientDoesNotRetryUnknownWriteOutcome(t *testing.T) {
	backend := katatest.New(t, katatest.WithToken("synthetic-key"))
	_, err := backend.Client(t).InitProject(t.Context(), &wire.InitProjectRequestOptions{Body: &wire.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			writes.Add(1)
			committed := httptest.NewRecorder()
			backend.Service.Handler().ServeHTTP(committed, r)
			assert.Equal(t, http.StatusOK, committed.Code)
			connection, _, err := http.NewResponseController(w).Hijack()
			if !assert.NoError(t, err) {
				return
			}
			_ = connection.Close()
			return
		}
		backend.Service.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	c := New(config.KataConnection{Endpoint: server.URL, APIKey: "synthetic-key", Project: "docbank"})
	issue, err := c.Create(t.Context(), "create", "Synthetic issue", "", nil)
	require.NoError(t, err)
	err = c.Comment(t.Context(), issue.QualifiedRef, "comment", "Evidence added")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "127.0.0.1")
	require.EqualValues(t, 1, writes.Load())
	posted, err := c.GetComments(t.Context(), issue.QualifiedRef)
	require.NoError(t, err)
	require.Equal(t, []string{"Evidence added"}, posted)
}
