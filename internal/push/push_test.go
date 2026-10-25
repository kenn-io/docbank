package push_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/push"
	"go.kenn.io/docbank/internal/store"
	"uuid"
)

type fixture struct {
	connection *daemonconn.Connection
	baseURL    string
	store      *store.Store
	blobs      *blob.Store
	vaultRoot  string
	uploads    atomic.Int64
	drop       atomic.Bool
	// truncateReceipt sends success headers and only part of a committed receipt.
	truncateReceipt atomic.Bool
	// listings counts cursor list requests. In watch mode every scan with
	// files to push makes one before it reads any file.
	listings atomic.Int64
	// afterDroppedCommit runs once the daemon has answered a dropped upload,
	// with that answer's status, and before the connection is closed.
	afterDroppedCommit atomic.Pointer[func(status int)]
	// unavailable answers that many upcoming push uploads with 503.
	unavailable atomic.Int64
	// impostor makes the endpoint behave like a listener that captured the
	// port: it cannot prove the key and records what it receives.
	impostor     atomic.Bool
	stolenKeys   atomic.Int64
	stolenBodies atomic.Int64
	server       *httptest.Server
	// challengeOutages fails that many upcoming key challenges: the first with
	// a 503 that closes its connection, later ones by closing the socket
	// without a response.
	challengeOutages atomic.Int64
	challengeFails   atomic.Int64
	// onUpload runs once, when the next push upload reaches the daemon and
	// before the daemon reads its body.
	onUpload atomic.Pointer[func()]
	// afterUpload runs once after the daemon handles an upload, before the
	// client receives its response.
	afterUpload atomic.Pointer[func(*httptest.ResponseRecorder)]
}

func newFixture(t *testing.T, configure ...func(*config.Config)) *fixture {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	blobsDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0700))
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	for _, apply := range configure {
		apply(&cfg)
	}
	server := api.NewServer(api.Deps{Store: s, Blobs: blobs, VaultRoot: dir, Cfg: cfg})
	t.Cleanup(server.Close)
	f := &fixture{store: s, blobs: blobs, vaultRoot: dir}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.impostor.Load() {
			f.serveImpostor(w, r)
			return
		}
		if r.URL.Path == daemonauth.KeyChallengePath && f.challengeOutages.Load() > 0 {
			f.challengeOutages.Add(-1)
			if f.challengeFails.Add(1) == 1 {
				w.Header().Set("Connection", "close")
				http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
				return
			}
			hijacker, ok := w.(http.Hijacker)
			if !assert.True(t, ok) {
				return
			}
			conn, _, err := hijacker.Hijack()
			if assert.NoError(t, err) {
				assert.NoError(t, conn.Close())
			}
			return
		}
		if r.URL.Path == "/api/v1/push/sources" {
			f.listings.Add(1)
		}
		if r.URL.Path == "/api/v1/push/uploads" {
			f.uploads.Add(1)
			if hook := f.onUpload.Swap(nil); hook != nil {
				(*hook)()
			}
			if f.unavailable.Load() > 0 {
				f.unavailable.Add(-1)
				http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
				return
			}
			if hook := f.afterUpload.Swap(nil); hook != nil {
				recorded := httptest.NewRecorder()
				server.Handler().ServeHTTP(recorded, r)
				(*hook)(recorded)
				maps.Copy(w.Header(), recorded.Header())
				w.WriteHeader(recorded.Code)
				_, err := io.Copy(w, recorded.Body)
				assert.NoError(t, err)
				return
			}
			truncateReceipt := f.truncateReceipt.Swap(false)
			if f.drop.Swap(false) || truncateReceipt {
				recorded := httptest.NewRecorder()
				server.Handler().ServeHTTP(recorded, r)
				if hook := f.afterDroppedCommit.Swap(nil); hook != nil {
					(*hook)(recorded.Code)
				}
				hijacker, ok := w.(http.Hijacker)
				if !assert.True(t, ok) {
					return
				}
				conn, buffered, err := hijacker.Hijack()
				if !assert.NoError(t, err) {
					return
				}
				if truncateReceipt {
					_, err := fmt.Fprintf(buffered, "HTTP/1.1 %d OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n",
						recorded.Code, recorded.Body.Len())
					assert.NoError(t, err)
					_, err = buffered.Write(recorded.Body.Bytes()[:recorded.Body.Len()/2])
					assert.NoError(t, err)
					assert.NoError(t, buffered.Flush())
				}
				assert.NoError(t, conn.Close())
				return
			}
		}
		server.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	f.server = ts
	f.baseURL = ts.URL
	f.connection, err = daemonconn.NewPushConnection(ts.URL, "synthetic-key")
	require.NoError(t, err)
	return f
}

func (f *fixture) serveImpostor(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Api-Key") != "" {
		f.stolenKeys.Add(1)
	}
	if n, _ := io.Copy(io.Discard, r.Body); n > 0 {
		f.stolenBodies.Add(1)
	}
	if r.URL.Path == daemonauth.KeyChallengePath {
		nonce, _ := hex.DecodeString(r.URL.Query().Get("nonce"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof("guessed-key", nonce)})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// keyChallenge answers the daemon's API key challenge for key and passes every
// other request to next.
func keyChallenge(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != daemonauth.KeyChallengePath {
			next.ServeHTTP(w, r)
			return
		}
		nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof(key, nonce)})
	})
}

func options(dir string) push.Options {
	return push.Options{Folder: config.WatchConfig{Name: "laptop", Source: dir, Destination: "/archive", SettleTime: config.Duration(30 * time.Second), ScanInterval: config.Duration(5 * time.Second)}}
}

func TestPushFolderResumesWithoutUploadsAndKeepsDeletedDocuments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0700))
	file := filepath.Join(dir, "nested", "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("initial"), 0600))
	opts := options(dir)
	report, err := push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Added)
	assert.EqualValues(t, 1, f.uploads.Load())
	node, err := f.store.NodeByPath(t.Context(), "/archive/nested/notes.txt")
	require.NoError(t, err)
	// A new client on a relocated folder has no cursor. All authority is remote.
	resumed, err := daemonconn.NewPushConnection(f.baseURL, "synthetic-key")
	require.NoError(t, err)
	secondDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(secondDir, "nested"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(secondDir, "nested", "notes.txt"), []byte("initial"), 0600))
	resumedOpts := opts
	resumedOpts.Folder.Source = secondDir
	report, err = push.Run(t.Context(), resumed, resumedOpts)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Skipped)
	assert.EqualValues(t, 1, f.uploads.Load(), "unchanged re-push must send zero uploads")
	organized, err := f.store.MkdirAll(t.Context(), "/organized")
	require.NoError(t, err)
	_, _, err = f.store.Move(t.Context(), node.ID, organized.ID, "archive-name.txt", store.UnconditionalRev)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, []byte("changed"), 0600))
	report, err = push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Updated)
	changed, err := f.store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.NotEqual(t, node.CurrentVersionID, changed.CurrentVersionID)
	assert.Equal(t, "archive-name.txt", changed.Name)
	assert.Equal(t, organized.ID, *changed.ParentID)
	require.NoError(t, os.Rename(file, filepath.Join(dir, "nested", "renamed.txt")))
	report, err = push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Added)
	renamed, err := f.store.NodeByPath(t.Context(), "/archive/nested/renamed.txt")
	require.NoError(t, err)
	assert.NotEqual(t, node.ID, renamed.ID)
	require.NoError(t, os.Remove(filepath.Join(dir, "nested", "renamed.txt")))
	report, err = push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	assert.Zero(t, report.Added)
	kept, err := f.store.NodeByID(t.Context(), renamed.ID)
	require.NoError(t, err)
	assert.Nil(t, kept.TrashedAt)
}

func TestPushIdenticalFilesBecomeSeparateDocumentsInOneCollection(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("identical"), 0600))
	}
	report, err := push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	assert.Equal(t, 3, report.Added)
	first, err := f.store.NodeByPath(t.Context(), "/archive/a.txt")
	require.NoError(t, err)
	second, err := f.store.NodeByPath(t.Context(), "/archive/b.txt")
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)
	_, collections, err := f.store.Collections(t.Context(), 100, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, collections, "one push run is one collection")

	before := f.uploads.Load()
	report, err = push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	assert.Equal(t, 3, report.Skipped)
	assert.Equal(t, before, f.uploads.Load())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0600))
	report, err = push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	assert.Equal(t, 1, report.Updated)
	assert.Equal(t, "identical", readNode(t, f, second.ID), "an edit to one file never changes another document")
	_, collections, err = f.store.Collections(t.Context(), 100, 0)
	require.NoError(t, err)
	assert.Equal(t, 2, collections)
}

func TestPushResumesAfterServerCommitsButResponseIsLost(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("durable"), 0600))
	f.drop.Store(true)
	report, err := push.Run(t.Context(), f.connection, options(dir))
	require.Error(t, err)
	assert.Zero(t, report.Added, "a lost acknowledgment is not success")
	report, err = push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	assert.Equal(t, 1, report.Skipped)
	assert.EqualValues(t, 1, f.uploads.Load())
}

func TestPushWatchRecoversAfterCommittedReceiptIsInterrupted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("durable"), 0o600))
	f.truncateReceipt.Store(true)
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var outcomes []string
	opts.Progress = func(_ string, outcome string) error {
		outcomes = append(outcomes, outcome)
		cancel()
		return nil
	}
	report, err := push.Run(ctx, f.connection, opts)
	require.ErrorIs(t, err, context.Canceled, "watch must recover by refreshing the committed cursor")
	assert.Equal(t, []string{"skipped"}, outcomes)
	assert.Equal(t, push.Report{Skipped: 1}, report)
	assert.EqualValues(t, 1, f.uploads.Load(), "the committed upload must not be repeated")
	node, err := f.store.NodeByPath(t.Context(), "/archive/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "durable", readNode(t, f, node.ID))
}

func TestPushRejectsMalformedSuccessAndDigestMismatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	sum := sha256.Sum256([]byte("expected"))
	source := store.PushSource{Name: "laptop", Ref: "bad.txt"}
	content := store.PushContent{RunID: uuid.NewV4().String(), ParentPath: "/", Name: "bad.txt", Hash: hex.EncodeToString(sum[:]), Size: 8, MIMEType: "text/plain"}
	_, err := f.connection.PushUpload(t.Context(), source, content, strings.NewReader("mismatch"))
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok, "unexpected error: %v", err)
	assert.Equal(t, "digest_mismatch", code, "equal sizes must reach the digest check")
	cursors, err := f.store.PushCursors(t.Context(), "laptop", "", store.MaxPushCursorPage)
	require.NoError(t, err)
	assert.Empty(t, cursors, "rejected bytes grant no source authority")
	for _, body := range []string{`{"status":`, `{}`, `{"status":"added","node":{"id":1,"kind":"file","revision":1},"computed_hash":"wrong","computed_size":8}`} {
		t.Run(body, func(t *testing.T) {
			ts := httptest.NewServer(keyChallenge("synthetic-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})))
			t.Cleanup(ts.Close)
			c, err := daemonconn.NewPushConnection(ts.URL, "synthetic-key")
			require.NoError(t, err)
			_, err = c.PushUpload(t.Context(), source, content, strings.NewReader("expected"))
			require.Error(t, err)
			assert.True(t, daemonconn.IsResponseDecodeError(err), "a malformed receipt is an unknown outcome")
		})
	}
}

func TestPushCancellationDoesNotReportSuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report, err := push.Run(ctx, f.connection, options(t.TempDir()))
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, report.Added)
	assert.Zero(t, f.uploads.Load())
}

func TestPushWatchUploadsSettledChangesAndStopsOnCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(time.Second)
	opts.Folder.ScanInterval = config.Duration(100 * time.Millisecond)
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "added", <-events)
	require.NoError(t, os.WriteFile(file, []byte("changed"), 0600))
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "updated", <-events)
	cancel()
	require.Eventually(t, func() bool { return len(done) > 0 }, 30*time.Second, 20*time.Millisecond)
	require.ErrorIs(t, <-done, context.Canceled)
	assert.EqualValues(t, 2, f.uploads.Load())
}

func TestPushWatchUploadsEqualLengthEditsWithRestoredMtime(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0o600))
	before, err := os.Stat(file)
	require.NoError(t, err)
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	events := make(chan string, 8)
	opts.Progress = func(ref string, outcome string) error {
		if ref == "z-sentinel.txt" {
			// The scanner has finished notes.txt and cannot observe its temporary mtime.
			if err := os.WriteFile(file, []byte("other"), 0o600); err != nil {
				return err
			}
			if err := os.Chtimes(file, before.ModTime(), before.ModTime()); err != nil {
				return err
			}
		}
		events <- outcome
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case outcome := <-events:
		require.Equal(t, "added", outcome)
	case <-ctx.Done():
		t.Fatal("initial file was not uploaded")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "z-sentinel.txt"), []byte("sentinel"), 0o600))
	select {
	case outcome := <-events:
		require.Equal(t, "added", outcome)
	case <-ctx.Done():
		t.Fatal("sentinel was not uploaded")
	}
	after, err := os.Stat(file)
	require.NoError(t, err)
	require.Equal(t, before.Size(), after.Size())
	require.Equal(t, before.ModTime(), after.ModTime())
	require.True(t, os.SameFile(before, after))
	select {
	case outcome := <-events:
		require.Equal(t, "updated", outcome)
	case <-ctx.Done():
		t.Fatal("same-size edit with restored mtime was not uploaded")
	}
	node, err := f.store.NodeByPath(t.Context(), "/archive/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, "other", readNode(t, f, node.ID))
	assert.EqualValues(t, 3, f.uploads.Load())
}

func TestPushRefusesMalformedKnownSourceInsteadOfSkipping(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := []byte("synthetic")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), content, 0600))
	sum := sha256.Sum256(content)
	uploaded := false
	ts := httptest.NewServer(keyChallenge("synthetic-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			uploaded = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"items": []map[string]any{{
			"source_ref": "file.txt", "node_id": 0, "hash": hex.EncodeToString(sum[:]), "size": 9, "trashed": false,
		}}})
	})))
	t.Cleanup(ts.Close)
	c, err := daemonconn.NewPushConnection(ts.URL, "synthetic-key")
	require.NoError(t, err)
	report, err := push.Run(t.Context(), c, options(dir))
	require.ErrorContains(t, err, "invalid push cursor page")
	assert.Zero(t, report.Skipped)
	assert.False(t, uploaded)
}

func TestPushTakesOverDaemonWatchWithZeroUploads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	watchDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(watchDir, "nested"), 0700))
	watchFile := filepath.Join(watchDir, "nested", "notes.txt")
	require.NoError(t, os.WriteFile(watchFile, []byte("watch initial"), 0600))
	cfg := options(watchDir).Folder
	cfg.Destination = "/watched"
	cfg.SettleTime = config.Duration(time.Second)
	cfg.ScanInterval = config.Duration(100 * time.Millisecond)
	w, err := ingest.NewWatcher(&ingest.Ingester{Store: f.store, Blobs: f.blobs}, f.vaultRoot, cfg,
		func(fn func() error) error { return fn() }, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var node store.Node
	require.Eventually(t, func() bool {
		var lookupErr error
		node, lookupErr = f.store.NodeByPath(t.Context(), "/watched/nested/notes.txt")
		return lookupErr == nil
	}, 30*time.Second, 20*time.Millisecond)
	content := []byte("watch last accepted")
	sum := sha256.Sum256(content)
	require.NoError(t, os.WriteFile(watchFile, content, 0600))
	require.Eventually(t, func() bool {
		var lookupErr error
		node, lookupErr = f.store.NodeByID(t.Context(), node.ID)
		return lookupErr == nil && node.BlobHash == hex.EncodeToString(sum[:])
	}, 30*time.Second, 20*time.Millisecond)
	stop()
	require.Eventually(t, func() bool { return len(done) > 0 }, 30*time.Second, 20*time.Millisecond)
	require.ErrorIs(t, <-done, context.Canceled)

	localDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(localDir, "nested"), 0700))
	localFile := filepath.Join(localDir, "nested", "notes.txt")
	require.NoError(t, os.WriteFile(localFile, content, 0600))
	opts := options(localDir)
	report, err := push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Skipped)
	assert.Zero(t, report.Added)
	assert.Zero(t, f.uploads.Load(), "switch-over must upload no unchanged bytes")
	unchanged, err := f.store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, node.Revision, unchanged.Revision)
	assert.Equal(t, node.CurrentVersionID, unchanged.CurrentVersionID)
	_, err = f.store.NodeByPath(t.Context(), "/archive")
	require.ErrorIs(t, err, store.ErrNotFound, "switch-over must not create a destination or new node")

	require.NoError(t, os.WriteFile(localFile, []byte("push changed"), 0600))
	report, err = push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Updated)
	assert.EqualValues(t, 1, f.uploads.Load())
	changed, err := f.store.NodeByPath(t.Context(), "/watched/nested/notes.txt")
	require.NoError(t, err)
	assert.Equal(t, node.ID, changed.ID)
	assert.NotEqual(t, node.CurrentVersionID, changed.CurrentVersionID)
	_, err = f.store.NodeByPath(t.Context(), "/archive")
	require.ErrorIs(t, err, store.ErrNotFound, "changed identities must keep their existing destination")
	report, err = push.Run(t.Context(), f.connection, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, report.Skipped)
	assert.EqualValues(t, 1, f.uploads.Load(), "push's accepted hash must replace the old watch cursor for resume")
}

func TestPushNeverSendsKeyOrBytesToUnprovenEndpoint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	report, err := push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	require.Equal(t, 1, report.Added)

	// The forwarded port is captured after the tunnel exits: pooled sockets are
	// gone and the replacement listener cannot prove the key.
	f.impostor.Store(true)
	f.server.CloseClientConnections()
	require.NoError(t, os.WriteFile(file, []byte("second"), 0600))
	report, err = push.Run(t.Context(), f.connection, options(dir))
	require.Error(t, err)
	assert.True(t, daemonconn.IsKeyProofError(err), "unexpected error: %v", err)
	assert.Zero(t, report.Updated)
	assert.Zero(t, f.stolenKeys.Load(), "the replacement listener must not receive the API key")
	assert.Zero(t, f.stolenBodies.Load(), "the replacement listener must not receive document bytes")
	assert.EqualValues(t, 1, f.uploads.Load())
}

func TestPushContinuesPastARejectedFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	archive, err := f.store.MkdirAll(t.Context(), "/archive")
	require.NoError(t, err)
	run, err := f.store.BeginIngest(t.Context(), "cli", "unrelated import")
	require.NoError(t, err)
	_, err = f.store.IngestFileExact(t.Context(), run, archive.ID, "b.txt", strings.Repeat("ab", 32), 3, "text/plain", "b.txt", "")
	require.NoError(t, err)
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("local "+name), 0600))
	}
	opts := options(dir)
	var failed []string
	opts.Failure = func(ref string, err error) error {
		failed = append(failed, ref)
		assert.ErrorIs(t, err, store.ErrExists)
		return nil
	}
	report, err := push.Run(t.Context(), f.connection, opts)
	require.ErrorIs(t, err, push.ErrFilesFailed)
	assert.Equal(t, 2, report.Added, "files after the rejected one are still pushed")
	assert.Equal(t, 1, report.Failed)
	assert.Equal(t, []string{"b.txt"}, failed)
}

func TestPushWatchRetriesAfterTheDaemonIsUnavailable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.unavailable.Store(1)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("first"), 0600))
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(200 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(50 * time.Millisecond)
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "added", <-events)
	assert.EqualValues(t, 2, f.uploads.Load(), "the refused upload is retried")
	assert.Empty(t, done, "an unavailable daemon does not end watch mode")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestPushRefusesTheNameOfAConfiguredDaemonWatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(cfg *config.Config) {
		cfg.Watches = []config.WatchConfig{{Name: "laptop", Source: t.TempDir(), Destination: "/watched"}}
	})
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("synthetic"), 0600))
	for _, watch := range []bool{false, true} {
		// The default 30-second settle window still applies: the refusal must
		// come from the startup check, not from the first settled file.
		opts := options(dir)
		opts.Watch = watch
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := push.Run(ctx, f.connection, opts)
		cancel()
		code, ok := daemonconn.ProblemCode(err)
		require.True(t, ok, "unexpected error: %v", err)
		assert.Equal(t, "push_name_in_use", code)
	}
	assert.Zero(t, f.uploads.Load())
}

func TestPushUpdatesAMovedDocumentAfterItsOldFolderIsTrashed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0700))
	file := filepath.Join(dir, "nested", "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	_, err := push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	node, err := f.store.NodeByPath(t.Context(), "/archive/nested/notes.txt")
	require.NoError(t, err)
	organized, err := f.store.MkdirAll(t.Context(), "/organized")
	require.NoError(t, err)
	_, _, err = f.store.Move(t.Context(), node.ID, organized.ID, "notes.txt", store.UnconditionalRev)
	require.NoError(t, err)
	archive, err := f.store.NodeByPath(t.Context(), "/archive")
	require.NoError(t, err)
	_, _, err = f.store.Trash(t.Context(), archive.ID, archive.Revision)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(file, []byte("second"), 0600))
	report, err := push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	assert.Equal(t, 1, report.Updated)
	changed, err := f.store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	assert.Equal(t, organized.ID, *changed.ParentID)
	_, err = f.store.NodeByPath(t.Context(), "/archive")
	require.ErrorIs(t, err, store.ErrNotFound, "an existing source does not recreate its old destination")
}

func TestPushRetriesAKeyChallengeThatFailedInTransit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("first"), 0600))

	f.challengeOutages.Store(1)
	_, err := push.Run(t.Context(), f.connection, options(dir))
	require.Error(t, err)
	assert.False(t, daemonconn.IsKeyProofError(err), "a 503 challenge is not a failed proof: %v", err)
	assert.True(t, daemonconn.IsTransportError(err), "unexpected error: %v", err)

	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "added", <-events)

	f.challengeFails.Store(0)
	f.challengeOutages.Store(2)
	f.server.CloseClientConnections()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("second"), 0600))
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "updated", <-events)
	assert.EqualValues(t, 2, f.challengeFails.Load(), "both interrupted challenges were retried")
	assert.Empty(t, done, "an interrupted challenge does not end watch mode")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestPushTreatsAnEditDuringUploadAsASourceChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		preserveMetadata    bool
		truncateThenRewrite bool
	}{
		{name: "size changes"},
		{name: "size and mtime preserved", preserveMetadata: true},
		{name: "truncated then restored size and mtime", preserveMetadata: true, truncateThenRewrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			dir := t.TempDir()
			file := filepath.Join(dir, "file.txt")
			// The file exceeds the socket buffers, so the upload still reads it
			// when the daemon receives the request and the edit lands.
			require.NoError(t, os.WriteFile(file, bytes.Repeat([]byte("x"), 32<<20), 0600))
			before, err := os.Stat(file)
			require.NoError(t, err)
			edited := []byte("edited after hashing")
			if tc.preserveMetadata {
				edited = bytes.Repeat([]byte("y"), 32<<20)
			}
			edit := func() {
				// Overwrite in place without briefly truncating to zero bytes.
				source, err := os.OpenFile(file, os.O_WRONLY, 0)
				if err != nil {
					t.Errorf("opening source for edit: %v", err)
					return
				}
				_, err = source.WriteAt(edited, 0)
				assert.NoError(t, err)
				assert.NoError(t, source.Truncate(int64(len(edited))))
				assert.NoError(t, source.Close())
				if tc.preserveMetadata {
					if err := os.Chtimes(file, before.ModTime(), before.ModTime()); err != nil {
						t.Errorf("restoring source mtime: %v", err)
						return
					}
					after, err := os.Stat(file)
					if assert.NoError(t, err) {
						assert.Equal(t, before.Size(), after.Size())
						assert.Equal(t, before.ModTime(), after.ModTime())
						assert.True(t, os.SameFile(before, after))
					}
				}
			}
			if tc.truncateThenRewrite {
				truncate := func() { assert.NoError(t, os.Truncate(file, 0)) }
				f.onUpload.Store(&truncate)
				rewrite := func(response *httptest.ResponseRecorder) {
					assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
					var problem struct {
						Code string `json:"code"`
					}
					if assert.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem)) {
						assert.Equal(t, "size_mismatch", problem.Code)
					}
					// The upload has finished reading the truncated source. Restore
					// its metadata before the client can classify the rejection.
					edit()
				}
				f.afterUpload.Store(&rewrite)
			} else {
				f.onUpload.Store(&edit)
			}

			opts := options(dir)
			opts.Watch = true
			opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
			opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
			var failures atomic.Int64
			opts.Failure = func(string, error) error { failures.Add(1); return nil }
			events := make(chan string, 4)
			opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
			ctx, cancel := context.WithCancel(t.Context())
			var report push.Report
			var runErr error
			done := make(chan struct{})
			go func() { report, runErr = push.Run(ctx, f.connection, opts); close(done) }()
			t.Cleanup(func() { cancel(); <-done })
			// Two real uploads and durable writes are bounded by the suite's
			// deadline, not an additional filesystem throughput requirement.
			select {
			case outcome := <-events:
				assert.Equal(t, "added", outcome)
			case <-done:
				require.FailNow(t, "watch stopped before adding the edited file", "%v", runErr)
			}
			cancel()
			<-done
			require.ErrorIs(t, runErr, context.Canceled)
			require.NotErrorIs(t, runErr, push.ErrFilesFailed)
			assert.Equal(t, 1, report.Added)
			assert.Zero(t, report.Failed)
			assert.Zero(t, failures.Load())
			assert.EqualValues(t, 2, f.uploads.Load(), "the mismatched upload was retried with the edited bytes")
			node, err := f.store.NodeByPath(t.Context(), "/archive/file.txt")
			require.NoError(t, err)
			assert.Equal(t, string(edited), readNode(t, f, node.ID))
		})
	}
}

func TestPushReportsRejectionWhenSourceBytesAreUnchanged(t *testing.T) {
	t.Parallel()
	for _, rejection := range []string{"digest_mismatch", "size_mismatch"} {
		t.Run(rejection, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("unchanged"), 0o600))
			ts := httptest.NewServer(keyChallenge("synthetic-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/push/sources":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"items":[]}`)
				case "/api/v1/push/uploads":
					_, err := io.Copy(io.Discard, r.Body)
					assert.NoError(t, err)
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = fmt.Fprintf(w, `{"status":422,"code":%q,"detail":"rejected content"}`, rejection)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})))
			t.Cleanup(ts.Close)
			connection, err := daemonconn.NewPushConnection(ts.URL, "synthetic-key")
			require.NoError(t, err)
			opts := options(dir)
			var failures []error
			opts.Failure = func(_ string, err error) error { failures = append(failures, err); return nil }
			report, err := push.Run(t.Context(), connection, opts)
			require.ErrorIs(t, err, push.ErrFilesFailed)
			assert.Equal(t, push.Report{Failed: 1}, report)
			require.Len(t, failures, 1)
			code, ok := daemonconn.ProblemCode(failures[0])
			require.True(t, ok)
			assert.Equal(t, rejection, code)
		})
	}
}

func readNode(t *testing.T, f *fixture, id int64) string {
	t.Helper()
	node, err := f.store.NodeByID(t.Context(), id)
	require.NoError(t, err)
	reader, err := f.blobs.Open(node.BlobHash)
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	return string(content)
}

func TestPushReportsAFileWhoseDocumentIsInTrash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("first "+name), 0600))
	}
	_, err := push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	node, err := f.store.NodeByPath(t.Context(), "/archive/a.txt")
	require.NoError(t, err)
	_, _, err = f.store.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	for _, name := range []string{"a.txt", "b.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("second "+name), 0600))
	}
	opts := options(dir)
	var failed []string
	opts.Failure = func(ref string, err error) error {
		failed = append(failed, ref)
		assert.ErrorContains(t, err, "in trash")
		return nil
	}
	before := f.uploads.Load()
	report, err := push.Run(t.Context(), f.connection, opts)
	require.ErrorIs(t, err, push.ErrFilesFailed)
	assert.Equal(t, []string{"a.txt"}, failed)
	assert.Equal(t, 1, report.Updated)
	assert.Equal(t, before+1, f.uploads.Load(), "a trashed document's file is never uploaded")
}

func TestPushWatchPushesADocumentRestoredFromTrash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	_, err := push.Run(t.Context(), f.connection, options(dir))
	require.NoError(t, err)
	node, err := f.store.NodeByPath(t.Context(), "/archive/a.txt")
	require.NoError(t, err)
	_, _, err = f.store.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, []byte("second"), 0600))

	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	failures := make(chan string, 4)
	opts.Failure = func(ref string, _ error) error { failures <- ref; return nil }
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(failures) > 0 }, 30*time.Second, 20*time.Millisecond)

	trashed, err := f.store.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	restored, _, err := f.store.Restore(t.Context(), node.ID, trashed.Revision)
	require.NoError(t, err)
	require.Equal(t, node.ID, restored.ID)
	require.NoError(t, os.WriteFile(file, []byte("third"), 0600))
	// A scan can settle on a partial write before "third" lands and push that
	// state first, so drain events until the node holds the final content.
	timeout := time.After(30 * time.Second)
	for readNode(t, f, node.ID) != "third" {
		select {
		case outcome := <-events:
			assert.Equal(t, "updated", outcome)
		case <-timeout:
			require.FailNow(t, "the restored document never received the third version")
		}
	}
	cancel()
	<-done
}

// TestPushWatchAsksTheDaemonAfterAnUncertainUpload covers A -> B committed with
// a lost response -> A. The client must not trust its cached A cursor, because
// the daemon may already hold B.
func TestPushWatchAsksTheDaemonAfterAnUncertainUpload(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(time.Second)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, "added", <-events)

	// Hold the dropped response until the second version is committed and the
	// source is back at the first version, so the edit cannot race the upload.
	committed := make(chan int, 1)
	restored := make(chan struct{})
	release := sync.OnceFunc(func() { close(restored) })
	defer release()
	hold := func(status int) { committed <- status; <-restored }
	f.afterDroppedCommit.Store(&hold)
	f.drop.Store(true)
	require.NoError(t, os.WriteFile(file, []byte("second"), 0600))
	select {
	case status := <-committed:
		require.Equal(t, http.StatusOK, status, "the daemon commits the second version")
	case <-time.After(30 * time.Second):
		t.Fatal("the second version never reached the daemon")
	}
	node, err := f.store.NodeByPath(t.Context(), "/archive/a.txt")
	require.NoError(t, err)
	require.Equal(t, "second", readNode(t, f, node.ID))
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	release()

	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "updated", <-events, "the daemon holds the second version, so the first must be uploaded again")
	assert.Equal(t, "first", readNode(t, f, node.ID))
	cancel()
	<-done
}

func TestPushWatchSeesTrashAndPurgeAfterStartup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("first"), 0600))
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	failures := make(chan string, 4)
	opts.Failure = func(ref string, _ error) error { failures <- ref; return nil }
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, "added", <-events)
	node, err := f.store.NodeByPath(t.Context(), "/archive/a.txt")
	require.NoError(t, err)

	_, _, err = f.store.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	touch := func() {
		later := time.Now().Add(time.Duration(len(failures)+len(events)+1) * time.Second)
		require.NoError(t, os.Chtimes(file, later, later))
	}
	touch()
	require.Eventually(t, func() bool { return len(failures) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "a.txt", <-failures, "unchanged bytes of a trashed document are an error, not unchanged")

	_, err = f.store.TrashEmpty(t.Context(), 0, true)
	require.NoError(t, err)
	touch()
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "added", <-events, "a purged document's file is imported again")
	cancel()
	<-done
}

func TestPushWatchSurvivesAnOutageAtStartup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.challengeOutages.Store(1)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("first"), 0600))
	opts := options(dir)
	opts.Watch = true
	opts.Folder.SettleTime = config.Duration(100 * time.Millisecond)
	opts.Folder.ScanInterval = config.Duration(20 * time.Millisecond)
	events := make(chan string, 4)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	require.Eventually(t, func() bool { return len(events) > 0 }, 30*time.Second, 20*time.Millisecond)
	assert.Equal(t, "added", <-events)
	assert.EqualValues(t, 1, f.challengeFails.Load(), "the startup read met the outage")
	assert.Empty(t, done)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestPushWatchCaptureOnChangeReachesFolderScanner(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("first"), 0600))
	opts := options(dir)
	opts.Watch = true
	opts.Folder.CaptureOnChange = true
	opts.Folder.SettleTime = config.Duration(time.Hour)
	opts.Folder.ScanInterval = config.Duration(time.Hour)
	events := make(chan string, 2)
	opts.Progress = func(_ string, outcome string) error { events <- outcome; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := push.Run(ctx, f.connection, opts); done <- err }()
	defer func() { cancel(); <-done }()
	select {
	case outcome := <-events:
		require.Equal(t, "added", outcome)
	case <-time.After(10 * time.Second):
		t.Fatal("prompt push waited for the hour-long settle window")
	}
	require.EqualValues(t, 1, f.uploads.Load())
}
