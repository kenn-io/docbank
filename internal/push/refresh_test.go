package push

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	"go.kenn.io/docbank/internal/store"
)

func newPushTestDaemon(t *testing.T, beforeUpload func()) (*daemonconn.Connection, *store.Store) {
	t.Helper()
	vault := t.TempDir()
	s, err := store.Open(filepath.Join(vault, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	blobsDir := filepath.Join(vault, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(s), blobsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server := api.NewServer(api.Deps{Store: s, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
	t.Cleanup(server.Close)
	handler := server.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/push/uploads" && beforeUpload != nil {
			beforeUpload()
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	connection, err := daemonconn.NewPushConnection(ts.URL, "synthetic-key")
	require.NoError(t, err)
	return connection, s
}

func TestPushWatchStartsOneCollectionPerScanThatUploads(t *testing.T) {
	t.Parallel()
	connection, s := newPushTestDaemon(t, nil)
	dir := t.TempDir()
	p := &pusher{
		connection: connection, report: &Report{},
		opts: Options{Watch: true, Folder: config.WatchConfig{
			Name: "laptop", Source: dir, Destination: "/archive",
			SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
		}},
	}
	scanner, err := ingest.NewFolderScanner(p.opts.Folder, p.process, p.reject)
	require.NoError(t, err)
	// Admit both files together without depending on their settle times.
	for _, content := range []string{"first", "second"} {
		for _, name := range []string{"a.txt", "b.txt"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content+name), 0o600))
		}
		require.NoError(t, scanner.ScanOnce(t.Context(), 1))
	}
	assert.Equal(t, Report{Added: 2, Updated: 2}, *p.report)
	collections, total, err := s.Collections(t.Context(), 100, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total, "each scan creates one collection containing both files")
	require.Len(t, collections, 2)
	for _, collection := range collections {
		members, err := s.CollectionMembers(t.Context(), collection.ID, 100, 0)
		require.NoError(t, err)
		assert.Equal(t, 2, members.Total)
		var paths []string
		for _, member := range members.Items {
			paths = append(paths, member.Path)
		}
		assert.ElementsMatch(t, []string{"/archive/a.txt", "/archive/b.txt"}, paths)
	}
}

func TestPushReportsAnUploadReadFailureAndContinues(t *testing.T) {
	t.Parallel()
	var uploading atomic.Pointer[os.File]
	connection, s := newPushTestDaemon(t, func() {
		if file := uploading.Swap(nil); file != nil {
			// Invalidate the source after hashing, while the upload still needs
			// to read more bytes than the socket can buffer.
			assert.NoError(t, file.Close())
		}
	})
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), make([]byte, 32<<20), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("readable"), 0o600))
	cursors, err := connection.PushCursors(t.Context(), "laptop")
	require.NoError(t, err)
	var failed []string
	p := &pusher{
		connection: connection, report: &Report{}, cursors: cursors,
		opts: Options{Folder: config.WatchConfig{
			Name: "laptop", Source: dir, Destination: "/archive",
			SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second),
		}, Failure: func(ref string, err error) error {
			failed = append(failed, ref)
			assert.ErrorIs(t, err, os.ErrClosed)
			return nil
		}},
	}
	scanner, err := ingest.NewFolderScanner(p.opts.Folder, func(ctx context.Context, file ingest.FolderFile) error {
		if file.Ref == "a.txt" {
			uploading.Store(file.File)
		}
		return p.process(ctx, file)
	}, p.reject)
	require.NoError(t, err)
	require.NoError(t, scanner.ScanOnce(t.Context(), 1), "a source read failure must not abort the scan")
	assert.Equal(t, []string{"a.txt"}, failed)
	assert.Equal(t, Report{Added: 1, Failed: 1}, *p.report)
	_, err = s.NodeByPath(t.Context(), "/archive/b.txt")
	require.NoError(t, err, "the readable sibling must reach the archive")
}

// A failed cursor read defers the rest of its watch scan: the remaining files
// are neither hashed nor allowed to repeat the request. The next scan reads
// again. Files are fed straight to the push step so the scan boundary does not
// depend on when each file finished settling.
func TestFailedCursorReadDefersTheRestOfItsScan(t *testing.T) {
	t.Parallel()
	var listings atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case daemonauth.KeyChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof("synthetic-key", nonce)})
		case "/api/v1/push/sources":
			if listings.Add(1) == 1 {
				http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.MarshalWrite(w, map[string]any{"items": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	connection, err := daemonconn.NewPushConnection(ts.URL, "synthetic-key")
	require.NoError(t, err)
	p := &pusher{
		connection: connection, report: &Report{},
		opts: Options{Watch: true, Folder: config.WatchConfig{Name: "laptop"}},
	}
	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("synthetic"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)

	// No file is open: hashing any of them would fail as a source access error.
	for _, ref := range []string{"a.txt", "b.txt", "c.txt"} {
		_, err := p.push(t.Context(), ingest.FolderFile{Ref: ref, Info: info, Scan: 2})
		require.Error(t, err)
		assert.True(t, retryable(err), "the outage defers %s: %v", ref, err)
		_, hashed := errors.AsType[*ingest.SourceAccessError](err)
		assert.False(t, hashed, "%s must not be hashed after the scan's cursor read failed", ref)
	}
	assert.EqualValues(t, 1, listings.Load(), "one cursor read per scan, even when it fails")

	_, err = p.push(t.Context(), ingest.FolderFile{Ref: "a.txt", Info: info, Scan: 3})
	require.Error(t, err, "no file is open, so the next step fails after the read")
	assert.EqualValues(t, 2, listings.Load(), "the next scan reads the cursors again")
}
