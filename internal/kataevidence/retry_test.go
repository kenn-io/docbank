package kataevidence

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

func TestAppendLateRetryRecognizesCommittedComment(t *testing.T) {
	server := katatest.New(t, katatest.WithToken("synthetic-key"))
	_, err := server.Client(t).InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	client := kataclient.New(config.KataConnection{Endpoint: server.Endpoint, APIKey: "synthetic-key", Project: "docbank"})
	ref := nativeReference()
	entry, err := json.Marshal(Entry{ID: ID(ref), Passage: PassageID(ref, "hello"), Reference: ref, Pending: &Pending{Quote: "hello", Label: "notes.txt"}})
	require.NoError(t, err)
	env := envelope{fields: map[string]jsontext.Value{"version": jsontext.Value("1")}, entries: []jsontext.Value{entry}}
	data, err := env.encode()
	require.NoError(t, err)
	issue, err := client.Create(t.Context(), "create", "Follow up", "", map[string]jsontext.Value{MetadataKey: data})
	require.NoError(t, err)
	// A committed comment with no active replay key models a retry after expiry.
	err = client.Comment(t.Context(), issue.QualifiedRef, "expired-key", "Evidence: notes.txt\n\n> hello\n")
	require.NoError(t, err)
	s := Service{Kata: client, Archive: &memoryArchive{prepareErr: ErrUnavailable}}
	got, err := s.Cite(t.Context(), CiteRequest{Issue: issue.QualifiedRef, Selector: Selector{NodeID: 2, EndRune: 5, ExpectedVersion: ref.Docbank.ContentVersionID}})
	require.NoError(t, err)
	require.Len(t, got.Comments, 1)
	require.NotContains(t, string(got.Metadata[MetadataKey]), `"pending"`)
}

func TestAppendReplayRejectsMovedIssue(t *testing.T) {
	server := katatest.New(t, katatest.WithToken("synthetic-key"))
	api := server.Client(t)
	project, err := api.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	client := kataclient.New(config.KataConnection{Endpoint: server.Endpoint, APIKey: "synthetic-key", Project: "docbank"})
	ref := nativeReference()
	entry, err := json.Marshal(Entry{ID: ID(ref), Passage: PassageID(ref, "hello"), Reference: ref, Pending: &Pending{Quote: "hello", Label: "notes.txt"}})
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
	require.Empty(t, got.Comments)
	require.Contains(t, string(got.Metadata[MetadataKey]), `"pending"`)
}
