package kataevidence

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/kataclient"
	"go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/kata/pkg/katatest"
)

// Capture the fixture database so its replay events can be aged without sleeping.
type kataReplayFixture struct {
	*testing.T

	dir string
}

func (f *kataReplayFixture) TempDir() string {
	f.dir = f.T.TempDir()
	return f.dir
}

func TestCreateReplayRejectsMovedIssueAfterWindow(t *testing.T) {
	fixture := &kataReplayFixture{T: t}
	server := katatest.New(fixture, katatest.WithToken("synthetic-key"))
	sdk := server.Client(t)
	project, err := sdk.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("docbank"), Actor: new("docbank")}})
	require.NoError(t, err)
	other, err := sdk.InitProject(t.Context(), &generated.InitProjectRequestOptions{Body: &generated.InitProjectBody{Name: new("other"), Actor: new("docbank")}})
	require.NoError(t, err)
	client := kataclient.New(config.KataConnection{Endpoint: server.Endpoint, APIKey: "synthetic-key", Project: "docbank"})
	service := Service{Kata: client, Archive: &memoryArchive{}}
	request := CiteRequest{Title: "Review", IdempotencyKey: "moved-create", Selector: Selector{NodeID: 2, EndRune: 5}}
	issue, err := service.Cite(t.Context(), request)
	require.NoError(t, err)
	_, err = sdk.MoveIssue(t.Context(), &generated.MoveIssueRequestOptions{PathParams: &generated.MoveIssuePath{ProjectID: project.Project.ID, Ref: issue.UID}, Body: &generated.MoveIssueBody{Actor: new("docbank"), ToProjectUID: other.Project.UID}, Header: &generated.MoveIssueHeaders{IfMatch: new(`"rev-1"`)}})
	require.NoError(t, err)
	db, err := sql.Open("sqlite", filepath.Join(fixture.dir, "kata.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	result, err := db.ExecContext(t.Context(), `UPDATE events SET created_at = ? WHERE type = 'issue.created' AND issue_uid = ?`, time.Now().Add(-8*24*time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"), issue.UID)
	require.NoError(t, err)
	count, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	_, err = service.Cite(t.Context(), request)
	failure, ok := errors.AsType[*kataclient.Error](err)
	require.True(t, ok, "expected moved-issue error, got %v", err)
	require.Equal(t, "wrong_project", failure.Code)
	all, err := sdk.ListAllIssues(t.Context(), &generated.ListAllIssuesRequestOptions{})
	require.NoError(t, err)
	require.Len(t, all.Issues, 1)
	require.Equal(t, issue.UID, all.Issues[0].UID)
	require.Equal(t, other.Project.ID, all.Issues[0].ProjectID)
}
