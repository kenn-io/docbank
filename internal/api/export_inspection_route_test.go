package api_test

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestExportInspectionRoutesBoundDetailsAndOwner(t *testing.T) {
	ts, s := newTestServer(t, nil)
	token := issueWebSession(t, ts)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: token}
	owner := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	var members []bundle.Member
	for i := range 55 {
		n := createFileWithContent(t, ts, s, fmt.Sprintf("/synthetic-%d.txt", i), "original")
		members = append(members, bundle.Member{NodeID: n.ID, VersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size})
	}
	source, err := s.CreateExportSource(t.Context(), owner, bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "explicit", Members: members}, nil)
	require.NoError(t, err)
	recipes := "/api/v1/exports/sources/" + source.ID + "/email-pdf-recipes"
	resp, body := do(t, ts, http.MethodGet, recipes, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `"recipes":[]`)
	p, err := s.CreateExportPlan(t.Context(), owner, bundle.PlanRequest{OperationID: uuid.NewString(), SourceID: source.ID, MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "text", AllowUnavailable: true}}})
	require.NoError(t, err)
	path := "/api/v1/exports/plans/" + p.ID + "/problems"
	resp, body = do(t, ts, http.MethodGet, path, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var page struct {
		Total int              `json:"total"`
		Next  int              `json:"next"`
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Equal(t, 55, page.Total)
	require.Equal(t, 50, page.Next)
	require.Len(t, page.Items, 50)
	resp, body = do(t, ts, http.MethodGet, path+"?after=50", headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Zero(t, page.Next)
	require.Len(t, page.Items, 5)
	for _, target := range []string{recipes, path} {
		other := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
		resp, body = do(t, ts, http.MethodGet, target, other, nil)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, body)
		resp, body = do(t, ts, http.MethodGet, target+"?other=1", headers, nil)
		require.NotEqual(t, http.StatusOK, resp.StatusCode, body)
	}
	for _, suffix := range []string{"?after=-1", "?after=50&after=0", "?after=9999999"} {
		resp, body = do(t, ts, http.MethodGet, path+suffix, headers, nil)
		require.NotEqual(t, http.StatusOK, resp.StatusCode, body)
	}
}

func TestExportCollectionResolvesVaultMailboxWithoutChangingSessionOwnership(t *testing.T) {
	ts, s := newTestServer(t, nil)
	n := createFileWithContent(t, ts, s, "/source.mbox", "synthetic")
	owner := "vault:" + s.VaultID()
	_, err := s.BeginMailboxContainer(t.Context(), store.MailboxContainerRequest{ID: "container", Owner: owner, SHA256: n.BlobHash, Size: n.Size, Format: "mbox"})
	require.NoError(t, err)
	require.NoError(t, s.PutMailboxChunk(t.Context(), owner, "container", store.MailboxChunk{Index: 0, SHA256: n.BlobHash, Size: n.Size}))
	_, err = s.SealMailboxContainer(t.Context(), owner, "container", n.BlobHash, n.Size)
	require.NoError(t, err)
	j, err := s.BeginMailboxJob(t.Context(), owner, store.MailboxJobRequest{ID: "job", ContainerID: "container", ContainerSHA256: n.BlobHash, Settings: store.MailboxSettings{DestinationID: s.RootID()}})
	require.NoError(t, err)
	headers := map[string]string{"X-Api-Key": "", api.WebSessionHeader: issueWebSession(t, ts)}
	resp, body := do(t, ts, http.MethodPost, "/api/v1/exports/sources", headers, bundle.SourceRequest{OperationID: uuid.NewString(), Kind: "mailbox_collection", CollectionID: j.CollectionID})
	// It must find the vault-owned import and reject its incomplete state, not
	// incorrectly search for a browser-owned mailbox job and return not found.
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "export_conflict")
}
