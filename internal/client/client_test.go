package client_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

// serverKey is the fixed key every test server in this file is built with:
// production always has an effective key (see cmd/docbank/daemon.go and
// NewServer's refusal of an empty one), so the fake server must too.
const serverKey = "server-key"

type countedReader struct {
	reader io.Reader
	read   *atomic.Int64
}

func (r countedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read.Add(int64(n))
	return n, err
}

// newClient builds a real server keyed with serverKey and a client keyed
// with clientKey (which may differ from serverKey, to exercise the
// mismatched-key path — see TestWrongAPIKeyIsRejected).
func newClient(t *testing.T, clientKey string) (*client.Client, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	cfg := config.Default()
	cfg.Server.APIKey = serverKey
	blobsDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	srv := api.NewServer(api.Deps{Store: s, Blobs: blobs, VaultRoot: dir, Cfg: cfg})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return client.New(ts.URL, clientKey), s
}

func TestRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()

	dir, err := c.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	assert.Equal(t, "/docs", dir.Path)

	got, err := c.Stat(ctx, "/docs")
	require.NoError(t, err)
	assert.Equal(t, dir.ID, got.ID)

	renamed, err := c.Move(ctx, dir.ID, got.Revision, nil, new("papers"))
	require.NoError(t, err)
	assert.Equal(t, "/papers", renamed.Path)

	moved, err := c.MovePath(ctx, "/papers", "/docs")
	require.NoError(t, err)
	assert.Equal(t, "/docs", moved.Path)

	moved, err = c.MoveToPath(ctx, moved.ID, moved.Revision, "/filed")
	require.NoError(t, err)
	assert.Equal(t, "/filed", moved.Path)

	kids, err := c.Children(ctx, s.RootID())
	require.NoError(t, err)
	assert.Len(t, kids, 1)

	trashed, err := c.TrashPath(ctx, "/filed")
	require.NoError(t, err)
	restored, err := c.Restore(ctx, dir.ID, trashed.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/filed", restored.Path)
}

func TestSearchWithOptionsUsesStableTagIdentity(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()
	tag, err := s.CreateTag(ctx, "renewal")
	require.NoError(t, err)
	directory, err := s.Mkdir(ctx, s.RootID(), "policies")
	require.NoError(t, err)
	tagged, err := s.CreateFile(
		ctx, directory.ID, "insurance-renewal.pdf", strings.Repeat("a", 64), 1, "application/pdf",
	)
	require.NoError(t, err)
	_, err = s.CreateFile(
		ctx, s.RootID(), "insurance-draft.pdf", strings.Repeat("b", 64), 1, "application/pdf",
	)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, tag.ID, tagged.ID, tagged.Revision)
	require.NoError(t, err)

	report, err := c.SearchWithOptions(
		ctx, "insurance", 10, client.SearchOptions{
			TagID: tag.ID, MIMEType: "APPLICATION/PDF", UnderNodeID: directory.ID,
			ModifiedSince: "2000-01-01T00:00:00-05:00", ModifiedBefore: "2100-01-01T00:00:00Z",
		},
	)
	require.NoError(t, err)
	assert.Equal(t, tag.ID, report.TagID)
	assert.Equal(t, "application/pdf", report.MIMEType)
	assert.Equal(t, directory.ID, report.UnderNodeID)
	assert.Equal(t, "2000-01-01T05:00:00.000000000Z", report.ModifiedSince)
	assert.Equal(t, "2100-01-01T00:00:00.000000000Z", report.ModifiedBefore)
	require.Len(t, report.Hits, 1)
	assert.Equal(t, tagged.ID, report.Hits[0].Node.ID)

	_, err = c.SearchWithOptions(ctx, "insurance", 10, client.SearchOptions{TagID: "bad"})
	require.ErrorContains(t, err, "canonical UUIDv4")
	_, err = c.SearchWithOptions(ctx, "insurance", 10, client.SearchOptions{
		MIMEType: "application/pdf; version=1",
	})
	require.ErrorContains(t, err, "must not include parameters")
	_, err = c.SearchWithOptions(ctx, "insurance", 10, client.SearchOptions{UnderNodeID: -1})
	require.ErrorContains(t, err, "directory node ID must be positive")
	_, err = c.SearchWithOptions(ctx, "insurance", 10, client.SearchOptions{
		ModifiedSince: "2100-01-01T00:00:00Z", ModifiedBefore: "2000-01-01T00:00:00Z",
	})
	require.ErrorContains(t, err, "must be earlier")
}

func TestMoveToPathValidatesRequestBeforeTransport(t *testing.T) {
	c := client.New("http://127.0.0.1:1", serverKey)

	_, err := c.MoveToPath(t.Context(), 0, 1, "/filed")
	require.ErrorContains(t, err, "node ID must be positive")
	_, err = c.MoveToPath(t.Context(), 1, 0, "/filed")
	require.ErrorContains(t, err, "revision must be positive")
	_, err = c.MoveToPath(t.Context(), 1, 1, "filed")
	require.ErrorContains(t, err, "destination path must be absolute")
}

func TestAuditStatusBindsOnlyScopeTargetToEnrollmentBaseline(t *testing.T) {
	const (
		vaultID     = "11111111-1111-4111-8111-111111111111"
		lineageID   = "22222222-2222-4222-8222-222222222222"
		scopeID     = "33333333-3333-4333-8333-333333333333"
		operationID = "44444444-4444-4444-8444-444444444444"
	)
	enrollmentDigest := strings.Repeat("a", 64)
	inheritedDigest := strings.Repeat("b", 64)
	base := api.AuditStatus{
		Enabled: true, VaultID: vaultID, LineageID: lineageID,
		OperationSequenceHighWater: 2, AllocationEntryCount: 2,
		AllocationHead: strings.Repeat("c", 64),
		Scopes: []api.AuditScopeStatus{{
			ID: scopeID, TargetNodeID: 7, TargetPath: "/Taxes",
			EnableOperationID: operationID, BaselineDigest: enrollmentDigest,
			MemberCount: 2, EntryCount: 2, ChainHead: strings.Repeat("d", 64),
		}},
	}
	tests := []struct {
		name    string
		nodeID  int64
		path    string
		digest  string
		wantErr bool
	}{
		{name: "scope target", nodeID: 7, path: "/Taxes", digest: enrollmentDigest},
		{name: "inherited node", nodeID: 8, path: "/Taxes/2027", digest: inheritedDigest},
		{name: "mismatched scope target", nodeID: 7, path: "/Taxes", digest: inheritedDigest, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := base
			status.Membership = &api.AuditMembershipStatus{
				NodeID: test.nodeID, Path: test.path, Protected: true,
				ScopeIDs: []string{scopeID}, BaselineDigests: []string{test.digest},
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(status)
			}))
			t.Cleanup(ts.Close)
			_, err := client.New(ts.URL, "key").AuditStatus(t.Context(), "", test.nodeID)
			if test.wantErr {
				require.ErrorContains(t, err, "invalid node membership binding")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestTagRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()
	node, err := c.Mkdir(ctx, s.RootID(), "records")
	require.NoError(t, err)

	tag, err := c.CreateTag(ctx, "cafe\u0301")
	require.NoError(t, err)
	assert.Equal(t, "café", tag.Name)
	assert.Equal(t, int64(1), tag.Revision)
	assert.Zero(t, tag.AssignmentCount)
	byName, err := c.TagByName(ctx, "café")
	require.NoError(t, err)
	assert.Equal(t, tag, byName)
	byID, err := c.Tag(ctx, tag.ID)
	require.NoError(t, err)
	assert.Equal(t, tag, byID)
	_, err = c.AssignTag(ctx, tag.ID, node.ID, 0)
	require.ErrorContains(t, err, "revision must be positive")

	receipt, err := c.AssignTagPath(ctx, tag.ID, "/records")
	require.NoError(t, err)
	assert.True(t, receipt.Changed)
	assert.Equal(t, node.Revision+1, receipt.Node.Revision)
	assert.Equal(t, 1, receipt.Tag.AssignmentCount)
	assert.Equal(t, int64(2), receipt.Tag.Revision)

	receipt, err = c.AssignTag(ctx, tag.ID, node.ID, receipt.Node.Revision)
	require.NoError(t, err)
	assert.False(t, receipt.Changed)
	assert.Equal(t, int64(2), receipt.Node.Revision)

	page, err := c.Tags(ctx, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, tag.ID, page.Items[0].ID)
	page, err = c.NodeTags(ctx, node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, page.Total)

	nodes, err := c.TaggedNodes(ctx, tag.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, nodes.Total)
	require.Len(t, nodes.Items, 1)
	assert.Equal(t, "/records", nodes.Items[0].Path)

	tag, err = c.RenameTag(ctx, tag.ID, receipt.Tag.Revision, "archive")
	require.NoError(t, err)
	assert.Equal(t, "archive", tag.Name)
	assert.Equal(t, int64(3), tag.Revision)
	node, err = c.Stat(ctx, "/records")
	require.NoError(t, err)
	assert.Equal(t, int64(3), node.Revision)

	receipt, err = c.UnassignTagPath(ctx, tag.ID, "/records")
	require.NoError(t, err)
	assert.True(t, receipt.Changed)
	assert.Zero(t, receipt.Tag.AssignmentCount)
	assert.Equal(t, int64(4), receipt.Tag.Revision)
	deleted, err := c.DeleteTag(ctx, tag.ID, receipt.Tag.Revision)
	require.NoError(t, err)
	assert.Zero(t, deleted.RemovedAssignments)
	_, err = c.Tag(ctx, tag.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestErrorMapping(t *testing.T) {
	c, s := newClient(t, serverKey)
	ctx := t.Context()

	_, err := c.Stat(ctx, "/missing")
	require.ErrorIs(t, err, store.ErrNotFound)

	d, err := c.Mkdir(ctx, s.RootID(), "dup")
	require.NoError(t, err)
	_, err = c.Mkdir(ctx, s.RootID(), "dup")
	require.ErrorIs(t, err, store.ErrExists)

	_, err = c.Move(ctx, d.ID, d.Revision+99, nil, new("x"))
	require.ErrorIs(t, err, store.ErrStaleRevision)
}

func TestDeferredRetirementProblemKeepsTypedClientError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if err := json.NewEncoder(w).Encode(api.Error{
			Title: http.StatusText(http.StatusServiceUnavailable), Status: http.StatusServiceUnavailable,
			Code: "pack_retirement_deferred", Detail: "replacement committed; run storage pack",
		}); err != nil {
			t.Errorf("encoding problem response: %v", err)
		}
	}))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, "key")
	_, err := c.StorageRepack(t.Context(), 0, 0, 0)
	require.ErrorIs(t, err, packstore.ErrPackRetirementDeferred)
}

func TestAPIKeySent(t *testing.T) {
	c, _ := newClient(t, serverKey)
	require.NoError(t, c.Health(t.Context()))
	_, err := c.TrashList(t.Context()) // authed route succeeds only with the key
	require.NoError(t, err)
}

func TestJobsRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	items, err := c.Jobs(t.Context())
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestStoragePackRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	src := filepath.Join(t.TempDir(), "pack.txt")
	require.NoError(t, os.WriteFile(src, []byte("pack me"), 0o600))
	ingested, err := c.Ingest(t.Context(), []string{src}, "/inbox")
	require.NoError(t, err)
	require.Equal(t, 1, ingested.Added)

	report, err := c.StoragePack(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, report.BlobsPacked)
	assert.True(t, report.BudgetExhausted)
}

func TestIngestStreamRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	src := filepath.Join(t.TempDir(), "stream.txt")
	content := []byte("stream this ingest")
	require.NoError(t, os.WriteFile(src, content, 0o600))

	var events []api.IngestProgress
	report, err := c.IngestStream(t.Context(), []string{src}, "/inbox", nil,
		func(event api.IngestProgress) { events = append(events, event) })
	require.NoError(t, err)
	assert.Equal(t, 1, report.Added)
	assert.Empty(t, report.Failed)
	require.NotEmpty(t, events)

	finalStages := map[string]api.IngestProgress{}
	for _, event := range events {
		if event.Final {
			finalStages[event.Stage] = event
		}
	}
	require.Contains(t, finalStages, "scan")
	require.Contains(t, finalStages, "ingest")
	assert.Equal(t, int64(1), finalStages["ingest"].Done)
	assert.Equal(t, int64(len(content)), finalStages["ingest"].BytesDone)
}

func TestProgressStreamPreservesProblemCode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(api.IngestEvent{Type: "error", Error: &api.Error{
			Title: "Validation failed", Status: http.StatusUnprocessableEntity,
			Code: "validation", Detail: "invalid ingest request",
		}})
	}))
	t.Cleanup(ts.Close)

	_, err := client.New(ts.URL, "key").IngestStream(
		t.Context(), []string{"/source"}, "/inbox", nil, nil)
	require.Error(t, err)
	code, ok := client.ProblemCode(err)
	assert.True(t, ok)
	assert.Equal(t, "validation", code)
}

func TestMaintenanceBusyProblemIsRetryableAndTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(api.Error{
			Title: "Service Unavailable", Status: http.StatusServiceUnavailable,
			Code: "maintenance_busy", Detail: "vault maintenance is running",
		})
	}))
	t.Cleanup(ts.Close)

	_, err := client.New(ts.URL, "key").Info(t.Context())
	require.ErrorIs(t, err, client.ErrMaintenanceBusy)
	code, ok := client.ProblemCode(err)
	assert.True(t, ok)
	assert.Equal(t, "maintenance_busy", code)
}

func TestJSONMethodsRejectInvalidUTF8BeforeRequest(t *testing.T) {
	var requests atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(ts.Close)
	c := client.New(ts.URL, "key")
	invalidPath := string([]byte{'/', 'b', 'a', 'd', 0xff})

	tests := []struct {
		name string
		call func() error
	}{
		{name: "ordinary", call: func() error {
			_, err := c.IngestWithOptions(t.Context(), []string{invalidPath}, "/inbox", nil)
			return err
		}},
		{name: "stream", call: func() error {
			_, err := c.IngestStream(t.Context(), []string{invalidPath}, "/inbox", nil, nil)
			return err
		}},
		{name: "preflight", call: func() error {
			_, err := c.PreflightIngest(t.Context(), []string{invalidPath}, nil)
			return err
		}},
		{name: "mkdir name", call: func() error {
			_, err := c.Mkdir(t.Context(), 1, invalidPath)
			return err
		}},
		{name: "move name", call: func() error {
			_, err := c.Move(t.Context(), 2, 1, nil, &invalidPath)
			return err
		}},
		{name: "move source path", call: func() error {
			_, err := c.MovePath(t.Context(), invalidPath, "/dest")
			return err
		}},
		{name: "move destination path", call: func() error {
			_, err := c.MovePath(t.Context(), "/source", invalidPath)
			return err
		}},
		{name: "trash path", call: func() error {
			_, err := c.TrashPath(t.Context(), invalidPath)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := requests.Load()
			err := tt.call()
			require.ErrorContains(t, err, "is not valid UTF-8")
			assert.Equal(t, before, requests.Load(), "invalid text must not reach JSON or HTTP")
		})
	}
}

func TestBackupCreateStreamRoundTripAndTypedError(t *testing.T) {
	c, _ := newClient(t, serverKey)
	repoPath := filepath.Join(t.TempDir(), "repo")
	_, err := c.BackupInit(t.Context(), repoPath)
	require.NoError(t, err)
	src := filepath.Join(t.TempDir(), "backup.txt")
	require.NoError(t, os.WriteFile(src, []byte("stream this backup"), 0o600))
	_, err = c.Ingest(t.Context(), []string{src}, "/inbox")
	require.NoError(t, err)

	var events []api.BackupProgress
	snapshot, err := c.BackupCreateStream(t.Context(), client.BackupCreateOptions{
		Repo: repoPath, Tag: "client", Jobs: 1,
	}, func(event api.BackupProgress) { events = append(events, event) })
	require.NoError(t, err)
	assert.Equal(t, "client", snapshot.Tag)
	assert.Equal(t, int64(1), snapshot.Files)
	finalStages := map[string]bool{}
	for _, event := range events {
		if event.Final {
			finalStages[event.Stage] = true
		}
	}
	for _, stage := range []string{"freeze", "metadata", "attachments", "seal"} {
		assert.True(t, finalStages[stage], "stage %q emitted a final event", stage)
	}

	cancelCtx, cancel := context.WithCancel(t.Context())
	_, err = c.BackupCreateStream(cancelCtx, client.BackupCreateOptions{Repo: repoPath},
		func(api.BackupProgress) { cancel() })
	require.Error(t, err)

	repo, err := backup.Open(repoPath)
	require.NoError(t, err)
	var lock *backup.RepoLock
	var lockErr error
	require.Eventually(t, func() bool {
		lock, lockErr = repo.AcquireExclusiveLock("test", false)
		return lockErr == nil
	}, 5*time.Second, 10*time.Millisecond,
		"server-side cancellation must release the repository lock")
	t.Cleanup(func() { _ = lock.Release() })
	snapshots, err := c.BackupList(t.Context(), repoPath)
	require.NoError(t, err)
	assert.Len(t, snapshots, 1, "a disconnected progress client must not publish a snapshot")
	_, err = c.BackupCreateStream(t.Context(), client.BackupCreateOptions{Repo: repoPath}, nil)
	require.ErrorIs(t, err, backup.ErrRepoLocked)
	require.NoError(t, lock.Release())

	var verifyEvents []api.BackupProgress
	verified, err := c.BackupVerifyStream(t.Context(), client.BackupVerifyOptions{
		Repo: repoPath, Jobs: 1,
	}, func(event api.BackupProgress) { verifyEvents = append(verifyEvents, event) })
	require.NoError(t, err)
	assert.Equal(t, []string{snapshot.ID}, verified.Snapshots)
	assert.Positive(t, verified.BlobsChecked)
	assert.Positive(t, verified.BytesRead)
	assert.Empty(t, verified.Problems)
	require.NotEmpty(t, verifyEvents)
	assert.Equal(t, "verify", verifyEvents[len(verifyEvents)-1].Stage)
	assert.True(t, verifyEvents[len(verifyEvents)-1].Final)

	quick, err := c.BackupVerify(t.Context(), client.BackupVerifyOptions{
		Repo: repoPath, SnapshotID: snapshot.ID, Quick: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{snapshot.ID}, quick.Snapshots)
	assert.Positive(t, quick.BytesRead, "quick verification still reads metadata")

	cancelledTarget := filepath.Join(t.TempDir(), "cancelled-restore")
	restoreCtx, cancelRestore := context.WithCancel(t.Context())
	_, err = c.BackupRestoreStream(restoreCtx, client.BackupRestoreOptions{
		Repo: repoPath, Target: cancelledTarget, SnapshotID: snapshot.ID, Jobs: 1,
	}, func(api.BackupProgress) { cancelRestore() })
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(cancelledTarget, "docbank.db"))
	require.ErrorIs(t, err, os.ErrNotExist, "cancelled restore must not publish its database")
	require.Eventually(t, func() bool {
		lock, lockErr = repo.AcquireExclusiveLock("restore-cancel-test", false)
		return lockErr == nil
	}, 5*time.Second, 10*time.Millisecond,
		"server-side cancellation must release the restore repository lock")
	require.NoError(t, lock.Release())

	var restoreEvents []api.BackupProgress
	restored, err := c.BackupRestoreStream(t.Context(), client.BackupRestoreOptions{
		Repo: repoPath, Target: filepath.Join(t.TempDir(), "stream-restore"),
		SnapshotID: snapshot.ID, Jobs: 1,
	}, func(event api.BackupProgress) { restoreEvents = append(restoreEvents, event) })
	require.NoError(t, err)
	assert.Equal(t, snapshot.ID, restored.SnapshotID)
	assert.Equal(t, int64(1), restored.DocumentBlobs)
	assert.Equal(t, int64(1), restored.PackedBlobs)
	assert.True(t, restored.Proof.ContentVerified)
	require.NotEmpty(t, restoreEvents)
	assert.Equal(t, "restore_stats", restoreEvents[len(restoreEvents)-1].Stage)
	assert.True(t, restoreEvents[len(restoreEvents)-1].Final)

	restored, err = c.BackupRestore(t.Context(), client.BackupRestoreOptions{
		Repo: repoPath, Target: filepath.Join(t.TempDir(), "json-restore"), Jobs: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, snapshot.ID, restored.SnapshotID)
	assert.True(t, restored.Proof.SQLiteIntegrity)
}

func TestContentIdentityAndVerificationRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	content := []byte("remote writer evidence")
	src := filepath.Join(t.TempDir(), "evidence.txt")
	require.NoError(t, os.WriteFile(src, content, 0o600))
	report, err := c.Ingest(t.Context(), []string{src}, "/inbox")
	require.NoError(t, err)
	require.Equal(t, 1, report.Added)

	node, err := c.Stat(t.Context(), "/inbox/evidence.txt")
	require.NoError(t, err)
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])
	assert.Equal(t, wantHash, node.BlobHash)
	require.NotEmpty(t, node.CurrentVersionID)

	page, err := c.Versions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, page.Total)
	require.Len(t, page.Items, 1)
	assert.Equal(t, node.CurrentVersionID, page.Items[0].ID)
	version, err := c.Version(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, page.Items[0], version)
	looseRefs, err := c.ContentReferences(t.Context(), wantHash, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, looseRefs.Total)
	require.Len(t, looseRefs.Items, 1)
	assert.Equal(t, node.ID, looseRefs.Items[0].Node.ID)
	assert.Equal(t, node.CurrentVersionID, looseRefs.Items[0].Version.ID)
	assert.True(t, looseRefs.Items[0].IsCurrent)
	assert.Equal(t, "/inbox/evidence.txt", looseRefs.Items[0].Path)
	exhaustedRefs, err := c.ContentReferences(t.Context(), wantHash, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, exhaustedRefs.Total)
	assert.Empty(t, exhaustedRefs.Items)

	stream, err := c.Content(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.CurrentVersionID, stream.VersionID)
	assert.Equal(t, wantHash, stream.BlobHash)
	assert.Equal(t, int64(len(content)), stream.Size)
	var got bytes.Buffer
	_, err = stream.CopyVerified(&got)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, content, got.Bytes())
	assert.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":", stream.ContentDigest())

	verified, err := c.VerifyNodeContent(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	assert.True(t, verified.Verified)
	assert.Equal(t, wantHash, verified.ComputedHash)

	packed, err := c.StoragePack(t.Context(), 0)
	require.NoError(t, err)
	require.Equal(t, 1, packed.BlobsPacked)
	packedRefs, err := c.ContentReferences(t.Context(), wantHash, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, looseRefs, packedRefs)
	versionStream, err := c.VersionContent(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, node.CurrentVersionID, versionStream.VersionID)
	var packedBytes bytes.Buffer
	_, err = versionStream.CopyVerified(&packedBytes)
	require.NoError(t, err)
	require.NoError(t, versionStream.Close())
	assert.Equal(t, content, packedBytes.Bytes())
	assert.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":",
		versionStream.ContentDigest())
	verified, err = c.VerifyNodeContent(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	assert.True(t, verified.Verified, "the same evidence contract reads packed authority")
	assert.Equal(t, wantHash, verified.ComputedHash)
	_, err = c.ContentReferences(t.Context(), "ABC", 10, 0)
	require.ErrorContains(t, err, "canonical lowercase SHA-256")
	_, err = c.ContentReferences(t.Context(), wantHash, 0, 0)
	require.ErrorContains(t, err, "between 1 and 1000")
	_, err = c.ContentReferences(t.Context(), wantHash, 1, -1)
	require.ErrorContains(t, err, "must not be negative")
}

func TestContentReferencesRejectsInconsistentResponses(t *testing.T) {
	const (
		versionID   = "11111111-1111-4111-8111-111111111111"
		operationID = "22222222-2222-4222-8222-222222222222"
	)
	hash := strings.Repeat("a", 64)
	base := api.ContentReferencePage{
		Items: []api.ContentReference{{
			Version: api.ContentVersion{ID: versionID, NodeID: 7, BlobHash: hash,
				Size: 3, MimeType: "text/plain", NodeRevision: 1,
				IntroducedOperationID: operationID, TransitionKind: "content_create"},
			Node: api.Node{ID: 7, Name: "doc.txt", Kind: "file", CurrentVersionID: versionID,
				BlobHash: hash, Size: 3, MimeType: "text/plain", Revision: 1},
			Path: "/doc.txt", IsCurrent: true,
		}},
		Total: 1, Limit: 10, Offset: 0,
	}
	tests := map[string]func(*api.ContentReferencePage){
		"requested hash": func(page *api.ContentReferencePage) {
			page.Items[0].Version.BlobHash = strings.Repeat("b", 64)
		},
		"node identity":     func(page *api.ContentReferencePage) { page.Items[0].Node.ID++ },
		"current state":     func(page *api.ContentReferencePage) { page.Items[0].IsCurrent = false },
		"current authority": func(page *api.ContentReferencePage) { page.Items[0].Node.Size++ },
		"path state":        func(page *api.ContentReferencePage) { page.Items[0].Path = "" },
		"trashed path": func(page *api.ContentReferencePage) {
			page.Items[0].Node.TrashedAt = "2026-07-15T12:00:00.000000000Z"
			page.Items[0].Path = "doc.txt"
		},
		"pagination": func(page *api.ContentReferencePage) { page.Total = 0 },
		"missing page": func(page *api.ContentReferencePage) {
			page.Total = 2
			page.Items = nil
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			page := base
			page.Items = append([]api.ContentReference(nil), base.Items...)
			mutate(&page)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(page); err != nil {
					t.Errorf("encoding response: %v", err)
				}
			}))
			t.Cleanup(ts.Close)
			_, err := client.New(ts.URL, "").ContentReferences(t.Context(), hash, 10, 0)
			require.ErrorContains(t, err, "content-reference response")
		})
	}
}

func TestVersionContentRejectsUnprovenResponses(t *testing.T) {
	const (
		requestedVersion = "11111111-1111-4111-8111-111111111111"
		otherVersion     = "22222222-2222-4222-8222-222222222222"
	)
	content := []byte("version bytes")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	digest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
	tests := []struct {
		name      string
		versionID string
		size      int64
		hash      string
		digest    string
		wantError string
	}{
		{name: "different version", versionID: otherVersion, size: int64(len(content)), hash: hash,
			digest: digest, wantError: "returned version identity"},
		{name: "wrong size", versionID: requestedVersion, size: int64(len(content) + 1), hash: hash,
			digest: digest, wantError: "received 13 bytes, expected 14"},
		{name: "wrong hash", versionID: requestedVersion, size: int64(len(content)), hash: strings.Repeat("0", 64),
			digest: digest, wantError: "computed SHA-256"},
		{name: "missing digest", versionID: requestedVersion, size: int64(len(content)), hash: hash,
			wantError: "lacks terminal Content-Digest"},
		{name: "wrong digest", versionID: requestedVersion, size: int64(len(content)), hash: hash,
			digest:    "sha-256=:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=:",
			wantError: "terminal Content-Digest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(api.ContentVersionHeader, tt.versionID)
				w.Header().Set(api.BlobHashHeader, tt.hash)
				w.Header().Set(api.BlobSizeHeader, strconv.FormatInt(tt.size, 10))
				w.Header().Set("Trailer", "Content-Digest")
				_, _ = w.Write(content)
				if tt.digest != "" {
					w.Header().Set("Content-Digest", tt.digest)
				}
			}))
			t.Cleanup(ts.Close)
			stream, err := client.New(ts.URL, "key").VersionContent(t.Context(), requestedVersion)
			if err != nil {
				require.ErrorContains(t, err, tt.wantError)
				assert.ErrorIs(t, err, client.ErrIntegrity)
				return
			}
			defer func() { _ = stream.Close() }()
			_, err = stream.CopyVerified(io.Discard)
			require.ErrorContains(t, err, tt.wantError)
			assert.ErrorIs(t, err, client.ErrIntegrity)
		})
	}
}

func TestDigestCheckedUploadRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	content := "client upload"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])

	added, err := c.Upload(t.Context(), s.RootID(), "upload.txt", "text/plain",
		hash, int64(len(content)), strings.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, "added", added.Status)
	assert.Equal(t, hash, added.ComputedHash)
	assert.Equal(t, hash, added.Node.BlobHash)

	skipped, err := c.Upload(t.Context(), s.RootID(), "upload.txt", "text/plain",
		hash, int64(len(content)), strings.NewReader(content))
	require.NoError(t, err)
	assert.Equal(t, "skipped", skipped.Status)
	assert.Equal(t, added.Node.ID, skipped.Node.ID)

	wrong := sha256.Sum256([]byte("wrong"))
	_, err = c.Upload(t.Context(), s.RootID(), "rejected.txt", "text/plain",
		hex.EncodeToString(wrong[:]), int64(len(content)), strings.NewReader(content))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest_mismatch")
}

func TestContentReplacementRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	initial := "initial content"
	initialSum := sha256.Sum256([]byte(initial))
	created, err := c.Upload(t.Context(), s.RootID(), "versioned.txt", "text/plain",
		hex.EncodeToString(initialSum[:]), int64(len(initial)), strings.NewReader(initial))
	require.NoError(t, err)

	replacement := []byte{0x00, 0xff, 'n', 'e', 'w'}
	replacementSum := sha256.Sum256(replacement)
	receipt, err := c.ReplaceContent(t.Context(), created.Node.ID, created.Node.Revision,
		"application/octet-stream", hex.EncodeToString(replacementSum[:]),
		int64(len(replacement)), bytes.NewReader(replacement))
	require.NoError(t, err)
	assert.Equal(t, created.Node.Revision+1, receipt.Node.Revision)
	assert.Equal(t, receipt.Version.ID, receipt.Node.CurrentVersionID)
	assert.Equal(t, "content_replace", receipt.Version.TransitionKind)
	assert.Equal(t, hex.EncodeToString(replacementSum[:]), receipt.ComputedHash)

	var staleBytesRead atomic.Int64
	_, err = c.ReplaceContent(t.Context(), created.Node.ID, created.Node.Revision,
		"application/octet-stream", hex.EncodeToString(replacementSum[:]),
		int64(len(replacement)), countedReader{
			reader: bytes.NewReader(replacement), read: &staleBytesRead,
		})
	require.ErrorIs(t, err, store.ErrStaleRevision)
	assert.Zero(t, staleBytesRead.Load(), "Expect: 100-continue must avoid sending a known-stale body")
	versions, err := c.Versions(t.Context(), created.Node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, versions.Total)
}

func TestContentReplacementRejectsUnprovenReceipt(t *testing.T) {
	content := []byte("replacement")
	hash := sha256.Sum256(content)
	expectedHash := hex.EncodeToString(hash[:])
	base := api.ContentReplacementReceipt{
		Node: api.Node{ID: 7, Revision: 4, CurrentVersionID: "11111111-1111-4111-8111-111111111111",
			BlobHash: expectedHash, Size: int64(len(content)), MimeType: "text/plain"},
		Version: api.ContentVersion{ID: "11111111-1111-4111-8111-111111111111", NodeID: 7,
			NodeRevision: 4, BlobHash: expectedHash, Size: int64(len(content)), MimeType: "text/plain",
			TransitionKind: "content_replace"},
		ComputedHash: expectedHash, ComputedSize: int64(len(content)),
	}
	tests := map[string]func(*api.ContentReplacementReceipt){
		"computed hash": func(r *api.ContentReplacementReceipt) { r.ComputedHash = strings.Repeat("0", 64) },
		"node":          func(r *api.ContentReplacementReceipt) { r.Node.ID++ },
		"version":       func(r *api.ContentReplacementReceipt) { r.Node.CurrentVersionID = "other" },
		"authority":     func(r *api.ContentReplacementReceipt) { r.Version.Size++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			receipt := base
			mutate(&receipt)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("ETag", `"4"`)
				_ = json.NewEncoder(w).Encode(receipt)
			}))
			t.Cleanup(ts.Close)
			_, err := client.New(ts.URL, "key").ReplaceContent(t.Context(), 7, 3, "text/plain",
				expectedHash, int64(len(content)), bytes.NewReader(content))
			require.Error(t, err)
		})
	}
}

func TestContentReversionRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	initial := "initial content"
	initialSum := sha256.Sum256([]byte(initial))
	created, err := c.Upload(t.Context(), s.RootID(), "versioned.txt", "text/plain",
		hex.EncodeToString(initialSum[:]), int64(len(initial)), strings.NewReader(initial))
	require.NoError(t, err)
	replacement := "replacement"
	replacementSum := sha256.Sum256([]byte(replacement))
	replaced, err := c.ReplaceContent(t.Context(), created.Node.ID, created.Node.Revision,
		"text/markdown", hex.EncodeToString(replacementSum[:]), int64(len(replacement)),
		strings.NewReader(replacement))
	require.NoError(t, err)

	receipt, err := c.RevertContent(t.Context(), created.Node.ID, replaced.Node.Revision,
		created.Node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, created.Node.CurrentVersionID, receipt.SourceVersion.ID)
	assert.Equal(t, "content_revert", receipt.Version.TransitionKind)
	assert.Equal(t, created.Node.BlobHash, receipt.Node.BlobHash)
	assert.Equal(t, "text/plain", receipt.Node.MimeType)

	_, err = c.RevertContent(t.Context(), created.Node.ID, replaced.Node.Revision,
		created.Node.CurrentVersionID)
	require.ErrorIs(t, err, store.ErrStaleRevision)
	_, err = c.RevertContent(t.Context(), created.Node.ID, receipt.Node.Revision, receipt.Version.ID)
	require.ErrorIs(t, err, store.ErrVersionAlreadyCurrent)
	other := "other content"
	otherSum := sha256.Sum256([]byte(other))
	otherNode, err := c.Upload(t.Context(), s.RootID(), "other.txt", "text/plain",
		hex.EncodeToString(otherSum[:]), int64(len(other)), strings.NewReader(other))
	require.NoError(t, err)
	_, err = c.RevertContent(t.Context(), created.Node.ID, receipt.Node.Revision,
		otherNode.Node.CurrentVersionID)
	require.ErrorIs(t, err, store.ErrVersionNodeMismatch)
}

func TestContentReversionRejectsUnprovenReceipt(t *testing.T) {
	const (
		sourceID = "11111111-1111-4111-8111-111111111111"
		newID    = "22222222-2222-4222-8222-222222222222"
	)
	hash := strings.Repeat("a", 64)
	base := api.ContentReversionReceipt{
		Node: api.Node{ID: 7, Revision: 4, CurrentVersionID: newID,
			BlobHash: hash, Size: 12, MimeType: "text/plain"},
		Version: api.ContentVersion{ID: newID, NodeID: 7, NodeRevision: 4,
			BlobHash: hash, Size: 12, MimeType: "text/plain", TransitionKind: "content_revert",
			SourceVersionID: new(string)},
		SourceVersion: api.ContentVersion{ID: sourceID, NodeID: 7, NodeRevision: 1,
			BlobHash: hash, Size: 12, MimeType: "text/plain", TransitionKind: "content_create"},
	}
	*base.Version.SourceVersionID = sourceID
	tests := map[string]func(*api.ContentReversionReceipt){
		"node":       func(r *api.ContentReversionReceipt) { r.Node.ID++ },
		"revision":   func(r *api.ContentReversionReceipt) { r.Version.NodeRevision++ },
		"new head":   func(r *api.ContentReversionReceipt) { r.Node.CurrentVersionID = sourceID },
		"source":     func(r *api.ContentReversionReceipt) { r.SourceVersion.ID = newID },
		"transition": func(r *api.ContentReversionReceipt) { r.Version.TransitionKind = "content_replace" },
		"binding": func(r *api.ContentReversionReceipt) {
			other := newID
			r.Version.SourceVersionID = &other
		},
		"authority": func(r *api.ContentReversionReceipt) { r.Version.Size++ },
		"etag":      func(_ *api.ContentReversionReceipt) {},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			receipt := base
			source := sourceID
			receipt.Version.SourceVersionID = &source
			mutate(&receipt)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if name != "etag" {
					w.Header().Set("ETag", `"4"`)
				}
				_ = json.NewEncoder(w).Encode(receipt)
			}))
			t.Cleanup(ts.Close)
			_, err := client.New(ts.URL, "key").RevertContent(t.Context(), 7, 3, sourceID)
			require.Error(t, err)
		})
	}
}

func TestContentVersionPruneRoundTrip(t *testing.T) {
	c, s := newClient(t, serverKey)
	initial := "initial content"
	initialSum := sha256.Sum256([]byte(initial))
	created, err := c.Upload(t.Context(), s.RootID(), "versioned.txt", "text/plain",
		hex.EncodeToString(initialSum[:]), int64(len(initial)), strings.NewReader(initial))
	require.NoError(t, err)
	replacement := "replacement content"
	replacementSum := sha256.Sum256([]byte(replacement))
	replaced, err := c.ReplaceContent(t.Context(), created.Node.ID, created.Node.Revision,
		"text/plain", hex.EncodeToString(replacementSum[:]), int64(len(replacement)),
		strings.NewReader(replacement))
	require.NoError(t, err)

	request := api.VersionPruneRequest{KeepNewest: 1}
	preview, err := c.PruneContentVersions(
		t.Context(), created.Node.ID, replaced.Node.Revision, request,
	)
	require.NoError(t, err)
	assert.False(t, preview.Changed)
	require.Len(t, preview.Candidates, 1)
	assert.Equal(t, created.Node.CurrentVersionID, preview.Candidates[0].ID)

	request.Run = true
	receipt, err := c.PruneContentVersions(
		t.Context(), created.Node.ID, replaced.Node.Revision, request,
	)
	require.NoError(t, err)
	assert.True(t, receipt.Changed)
	assert.Equal(t, 1, receipt.DeletedVersions)
	assert.Equal(t, replaced.Node.Revision+1, receipt.Node.Revision)
	versions, err := c.Versions(t.Context(), created.Node.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, versions.Total)
	_, err = c.PruneContentVersions(t.Context(), created.Node.ID, replaced.Node.Revision, request)
	require.ErrorIs(t, err, store.ErrStaleRevision)
}

func TestContentVersionPruneAllPriorCheckpointsCurrentRevert(t *testing.T) {
	c, s := newClient(t, serverKey)
	initial := "initial content"
	initialSum := sha256.Sum256([]byte(initial))
	created, err := c.Upload(t.Context(), s.RootID(), "reverted.txt", "text/plain",
		hex.EncodeToString(initialSum[:]), int64(len(initial)), strings.NewReader(initial))
	require.NoError(t, err)
	replacement := "replacement content"
	replacementSum := sha256.Sum256([]byte(replacement))
	replaced, err := c.ReplaceContent(t.Context(), created.Node.ID, created.Node.Revision,
		"text/plain", hex.EncodeToString(replacementSum[:]), int64(len(replacement)),
		strings.NewReader(replacement))
	require.NoError(t, err)
	reverted, err := c.RevertContent(t.Context(), created.Node.ID, replaced.Node.Revision,
		created.Node.CurrentVersionID)
	require.NoError(t, err)

	request := api.VersionPruneRequest{AllPrior: true}
	preview, err := c.PruneContentVersions(
		t.Context(), created.Node.ID, reverted.Node.Revision, request,
	)
	require.NoError(t, err)
	assert.True(t, preview.CheckpointRequired)
	assert.Nil(t, preview.Checkpoint)
	require.Len(t, preview.Candidates, 3)

	request.Run = true
	receipt, err := c.PruneContentVersions(
		t.Context(), created.Node.ID, reverted.Node.Revision, request,
	)
	require.NoError(t, err)
	require.NotNil(t, receipt.Checkpoint)
	assert.Equal(t, "content_replace", receipt.Checkpoint.TransitionKind)
	assert.Nil(t, receipt.Checkpoint.SourceVersionID)
	assert.Equal(t, receipt.Checkpoint.ID, receipt.Node.CurrentVersionID)
	assert.Equal(t, created.Node.BlobHash, receipt.Node.BlobHash)
	versions, err := c.Versions(t.Context(), created.Node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, versions.Items, 1)
	assert.Equal(t, receipt.Checkpoint.ID, versions.Items[0].ID)
}

func TestContentVersionPruneRejectsUnprovenReceipt(t *testing.T) {
	const (
		currentID = "11111111-1111-4111-8111-111111111111"
		oldID     = "22222222-2222-4222-8222-222222222222"
	)
	base := api.VersionPruneReport{
		Node: api.Node{ID: 7, Revision: 4, CurrentVersionID: currentID},
		Candidates: []api.ContentVersion{{
			ID: oldID, NodeID: 7, BlobHash: strings.Repeat("a", 64), Size: 12,
			NodeRevision: 2, IntroducedOperationID: "33333333-3333-4333-8333-333333333333",
			TransitionKind: "content_replace",
		}},
		DependencyRetained: []api.ContentVersion{},
		LogicalBytes:       12, UniqueBlobs: 1, ReleasableBlobs: 1,
		ReleasableBytes: 12, LooseBlobsPendingGC: 1, LooseBytesPendingGC: 12,
	}
	tests := map[string]func(*api.VersionPruneReport){
		"node":      func(r *api.VersionPruneReport) { r.Node.ID++ },
		"revision":  func(r *api.VersionPruneReport) { r.Node.Revision++ },
		"current":   func(r *api.VersionPruneReport) { r.Node.CurrentVersionID = oldID },
		"logical":   func(r *api.VersionPruneReport) { r.LogicalBytes++ },
		"unique":    func(r *api.VersionPruneReport) { r.UniqueBlobs++ },
		"negative":  func(r *api.VersionPruneReport) { r.ReleasableBytes = -1 },
		"duplicate": func(r *api.VersionPruneReport) { r.DependencyRetained = r.Candidates },
		"etag":      func(_ *api.VersionPruneReport) {},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			report := base
			report.Candidates = append([]api.ContentVersion(nil), base.Candidates...)
			mutate(&report)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if name != "etag" {
					w.Header().Set("ETag", `"4"`)
				}
				_ = json.NewEncoder(w).Encode(report)
			}))
			t.Cleanup(ts.Close)
			_, err := client.New(ts.URL, "key").PruneContentVersions(
				t.Context(), 7, 4, api.VersionPruneRequest{AllPrior: true},
			)
			require.Error(t, err)
		})
	}
}

func TestStorageRepackRoundTrip(t *testing.T) {
	c, _ := newClient(t, serverKey)
	for name, content := range map[string]string{
		"keep.txt": "keep", "drop-a.txt": "drop a", "drop-b.txt": "drop b",
	} {
		src := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(src, []byte(content), 0o600))
		ingested, err := c.Ingest(t.Context(), []string{src}, "/inbox")
		require.NoError(t, err)
		require.Equal(t, 1, ingested.Added)
	}
	_, err := c.StoragePack(t.Context(), 0)
	require.NoError(t, err)
	for _, path := range []string{"/inbox/drop-a.txt", "/inbox/drop-b.txt"} {
		drop, statErr := c.Stat(t.Context(), path)
		require.NoError(t, statErr)
		_, err = c.Trash(t.Context(), drop.ID, drop.Revision)
		require.NoError(t, err)
	}
	_, err = c.TrashEmpty(t.Context(), "", true)
	require.NoError(t, err)
	_, err = c.GC(t.Context(), true)
	require.NoError(t, err)

	var report api.StorageRepackReport
	var repackErr error
	require.Eventually(t, func() bool {
		report, repackErr = c.StorageRepack(t.Context(), 0, time.Nanosecond, 1)
		return repackErr == nil && report.PacksRewritten == 1
	}, time.Second, 10*time.Millisecond,
		"pack should become eligible after the platform clock advances")
	require.NoError(t, repackErr)
	assert.Equal(t, 1, report.PacksRewritten)
	assert.Equal(t, 1, report.PacksRemoved)
}

// TestWrongAPIKeyIsRejected is the client-side half of the keyless-loopback
// regression: a client holding the wrong key must get a clearly readable
// 401, never silent success and never an opaque envelope dump.
func TestWrongAPIKeyIsRejected(t *testing.T) {
	c, _ := newClient(t, "wrong-key")
	_, err := c.TrashList(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "unauthorized")
	code, ok := client.ProblemCode(err)
	assert.True(t, ok)
	assert.Equal(t, "unauthorized", code)
}

func TestRestoreTargetContentionRemainsTyped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(api.Error{
			Title: "Conflict", Status: http.StatusConflict,
			Code: "backup_restore_target_active", Detail: "restore target is active",
		})
	}))
	t.Cleanup(ts.Close)

	_, err := client.New(ts.URL, "key").TrashList(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, home.ErrVaultLocked)
	code, ok := client.ProblemCode(err)
	assert.True(t, ok)
	assert.Equal(t, "backup_restore_target_active", code)
}
