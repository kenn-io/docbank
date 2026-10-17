package api_test

import (
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestMailboxMappedTagDeletionReturnsConflict(t *testing.T) {
	t.Parallel()
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	c := store.MailboxContainerRequest{ID: "synthetic-source", Owner: "vault:" + s.VaultID(), SHA256: strings.Repeat("a", 64), Size: 3, Format: "mbox"}
	_, err := s.BeginMailboxContainer(ctx, c)
	require.NoError(t, err)
	require.NoError(t, s.RecordRenditionBlob(ctx, c.SHA256, c.Size, store.BlobPhysical{Encoding: "raw", StoredBytes: c.Size, PackEligible: true, Created: true}))
	require.NoError(t, s.PutMailboxChunk(ctx, c.Owner, c.ID, store.MailboxChunk{Index: 0, SHA256: c.SHA256, Size: c.Size}))
	_, err = s.SealMailboxContainer(ctx, c.Owner, c.ID, c.SHA256, c.Size)
	require.NoError(t, err)
	tag, err := s.CreateTag(ctx, "Synthetic project")
	require.NoError(t, err)
	_, err = s.BeginMailboxJob(ctx, c.Owner, store.MailboxJobRequest{
		ID: "synthetic-job", ContainerID: c.ID, ContainerSHA256: c.SHA256,
		Settings: store.MailboxSettings{DestinationID: s.RootID(), LabelTags: map[string]string{"Project": tag.ID}},
	})
	require.NoError(t, err)
	response, body := do(t, ts, http.MethodDelete, "/api/v1/tags/"+tag.ID,
		map[string]string{"If-Match": strconv.Quote(strconv.FormatInt(tag.Revision, 10))}, nil)
	require.Equal(t, http.StatusConflict, response.StatusCode, body)
	var problem api.Error
	require.NoError(t, json.Unmarshal([]byte(body), &problem))
	require.Equal(t, "mailbox_conflict", problem.Code)
}
