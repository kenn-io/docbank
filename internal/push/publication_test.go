package push

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var publicationTestPolicy = strings.Repeat("a", 64)

func publicationTestUUID() string { return uuid.NewV4().String() }
func publicationOptions(root string) Options {
	return Options{Publication: true, Folder: config.WatchConfig{Name: "research", Source: root, Destination: "/notes", SettleTime: config.Duration(2 * time.Second), ScanInterval: config.Duration(5 * time.Second)}}
}
func writePublicationFile(t *testing.T, root, ref string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(ref))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
	require.NoError(t, os.WriteFile(p, []byte("# Synthetic note\n"), 0600))
}
func publicationSources(t *testing.T, s *store.Store) map[string]store.NotePublicationSource {
	t.Helper()
	scan, err := s.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: publicationTestPolicy, OperationID: publicationTestUUID()})
	require.NoError(t, err)
	page, err := s.NotePublicationSources(t.Context(), store.NotePublicationSourcesRequest{Name: "research", ScanID: scan.ScanID, Limit: 1000})
	require.NoError(t, err)
	out := map[string]store.NotePublicationSource{}
	for _, v := range page.Items {
		out[v.Identity.SourceRef] = v
	}
	return out
}
func TestPublicationPublishesOnlyManagedScope(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	opts := publicationOptions(root)
	opts.Folder.Exclude = []string{"private"}
	for _, ref := range []string{"A.MD", "b.MarkDown", "editor/note.md", ".obsidian/x.md", ".git/x.md", ".hg/x.md", ".svn/x.md", ".trash/x.md", "private/x.md", "attachment.png"} {
		writePublicationFile(t, root, ref)
	}
	report, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Equal(t, 3, report.Added)
	sources := publicationSources(t, s)
	require.Len(t, sources, 3)
	for _, ref := range []string{"A.MD", "b.MarkDown", "editor/note.md"} {
		require.Contains(t, sources, ref)
	}
}
func TestPublicationQuietEpochAcknowledgesEverySource(t *testing.T) {
	t.Parallel()
	var batches []int
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if strings.HasSuffix(r.URL.Path, "/seen") {
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var req api.NotePublicationSeenRequest
			require.NoError(t, json.Unmarshal(body, &req))
			batches = append(batches, len(req.Bindings))
			r.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	opts := publicationOptions(root)
	for i := range 201 {
		writePublicationFile(t, root, fmt.Sprintf("note-%03d.md", i))
	}
	first, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Equal(t, 201, first.Added)
	before, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	second, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Zero(t, second.Added+second.Updated)
	require.Equal(t, 201, second.Skipped)
	after, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 201, after.Present)
	require.Zero(t, after.Absent)
	require.Equal(t, before.NamespaceRevision, after.NamespaceRevision)
	require.Equal(t, []int{200, 1}, batches)
	sources := publicationSources(t, s)
	for _, source := range sources {
		_, total, err := s.ContentVersions(t.Context(), source.Identity.NodeID, 10, 0)
		require.NoError(t, err)
		require.Equal(t, 1, total)
	}
}
func TestPublicationEmptyRenameRemoveReappear(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	opts := publicationOptions(root)
	writePublicationFile(t, root, "a.md")
	writePublicationFile(t, root, "same.md")
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	first := publicationSources(t, s)
	require.NotEqual(t, first["a.md"].Identity.NodeID, first["same.md"].Identity.NodeID)
	require.NoError(t, os.Rename(filepath.Join(root, "a.md"), filepath.Join(root, "b.md")))
	_, err = Run(t.Context(), c, opts)
	require.NoError(t, err)
	renamed := publicationSources(t, s)
	require.Equal(t, "absent", renamed["a.md"].Presence)
	require.Equal(t, "present", renamed["b.md"].Presence)
	require.NoError(t, os.Remove(filepath.Join(root, "b.md")))
	require.NoError(t, os.Remove(filepath.Join(root, "same.md")))
	_, err = Run(t.Context(), c, opts)
	require.NoError(t, err)
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.Zero(t, status.Present)
	require.EqualValues(t, 3, status.Absent)
	writePublicationFile(t, root, "a.md")
	_, err = Run(t.Context(), c, opts)
	require.NoError(t, err)
	again := publicationSources(t, s)
	require.Equal(t, first["a.md"].Identity.NodeID, again["a.md"].Identity.NodeID)
	require.Equal(t, first["a.md"].Identity.ContentVersionID, again["a.md"].Identity.ContentVersionID)
	require.Equal(t, "present", again["a.md"].Presence)
}
func TestPublicationPolicyFingerprintCanonical(t *testing.T) {
	t.Parallel()
	cfg := publicationOptions("/synthetic/root").Folder
	cfg.Exclude = []string{"z", "a", "z"}
	_, a, err := publicationPolicy(cfg)
	require.NoError(t, err)
	cfg.Source = "/synthetic/other"
	cfg.Destination = "/other"
	cfg.Exclude = []string{"a", "z"}
	_, b, err := publicationPolicy(cfg)
	require.NoError(t, err)
	require.Equal(t, a, b)
	cfg.Exclude = append(cfg.Exclude, "different")
	_, b, err = publicationPolicy(cfg)
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

// All fault injection surrounds the actual keyed daemon and real synthetic store.
func newPublicationTestDaemon(t *testing.T, hook func(http.ResponseWriter, *http.Request, http.Handler)) (*daemonconn.Connection, *store.Store) {
	t.Helper()
	vault := t.TempDir()
	s, err := store.Open(t.Context(), filepath.Join(vault, "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	dir := filepath.Join(vault, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tmp"), 0700))
	blobs, err := blob.New(store.NewPackCatalog(s), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server := api.NewServer(api.Deps{Store: s, Blobs: blobs, VaultRoot: vault, Cfg: cfg})
	t.Cleanup(server.Close)
	handler := server.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			hook(w, r, handler)
		} else {
			handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	c, err := daemonconn.NewPushConnection(ts.URL, "synthetic-key")
	require.NoError(t, err)
	return c, s
}
