package push

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
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
	"go.kenn.io/docbank/internal/ingest"
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

func TestPublicationStatusSeparatesInventoryAndUploadCounts(t *testing.T) {
	t.Parallel()
	c, _ := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	var events []PublicationStatus
	opts.Status = func(_ context.Context, s PublicationStatus) error { events = append(events, s); return nil }
	first, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.NotNil(t, first.Publication)
	require.Equal(t, "complete", first.Publication.Event)
	require.EqualValues(t, 1, first.Publication.Observed)
	require.EqualValues(t, 1, first.Publication.Accepted)
	require.Zero(t, first.Publication.Unchanged)
	require.EqualValues(t, 1, first.Publication.Current.Present)
	require.False(t, first.Publication.Current.Incomplete)
	require.NotEmpty(t, first.Publication.Current.LastCompleteAt)
	require.Contains(t, func() []string {
		var out []string
		for _, e := range events {
			out = append(out, e.Event)
		}
		return out
	}(), "completing")
	for _, e := range events {
		require.NotEqual(t, "complete", e.Event)
	}
	second, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Zero(t, second.Added+second.Updated)
	require.EqualValues(t, 1, second.Publication.Unchanged)
	require.Zero(t, second.Publication.Accepted)
	require.EqualValues(t, 1, second.Publication.Observed)
}
func TestPublicationStatusCapsReasons(t *testing.T) {
	t.Parallel()
	secret := "synthetic-do-not-log-secret"
	p := &publicationPublisher{opts: publicationOptions("/synthetic/private-root")}
	var snapshots []PublicationStatus
	p.opts.Status = func(_ context.Context, s PublicationStatus) error {
		saved := s
		saved.Errors = append([]PublicationFailure(nil), s.Errors...)
		if s.Current != nil {
			current := *s.Current
			if current.ActiveScan != nil {
				scan := *current.ActiveScan
				current.ActiveScan = &scan
			}
			saved.Current = &current
		}
		snapshots = append(snapshots, saved)
		if len(s.Errors) > 0 {
			s.Errors[0].Code = secret
		}
		if s.Current != nil {
			s.Current.Present = 900
			if s.Current.ActiveScan != nil {
				s.Current.ActiveScan.Epoch = 900
			}
		}
		return nil
	}
	p.current = &api.NotePublicationStatus{Present: 3, ActiveScan: &api.NotePublicationScan{Epoch: 1}}
	for i := range 12 {
		require.NoError(t, p.reject(t.Context(), fmt.Sprintf("note-%02d.md", i), fmt.Errorf("%s at /synthetic/private-root", secret)))
	}
	require.NoError(t, p.reject(t.Context(), "/synthetic/private-root", errors.New(secret)))
	last := snapshots[len(snapshots)-1]
	require.Len(t, last.Errors, 8)
	require.EqualValues(t, 5, last.ErrorsOmitted)
	require.EqualValues(t, 13, last.Failed)
	require.EqualValues(t, 3, last.Current.Present)
	require.EqualValues(t, 1, last.Current.ActiveScan.Epoch)
	encoded, err := json.Marshal(last, jsonv1.FormatDurationAsNano(true))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
	require.NotContains(t, string(encoded), "/synthetic/private-root")
	for _, e := range last.Errors {
		require.Equal(t, "source_access", e.Code)
	}
}
func TestPublicationIdleIsFreshWithoutUploads(t *testing.T) {
	t.Parallel()
	c, _ := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	var events []PublicationStatus
	opts.Status = func(_ context.Context, s PublicationStatus) error { events = append(events, s); return nil }
	report, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Zero(t, report.Added+report.Updated)
	require.NotEmpty(t, report.Publication.Current.LastCompleteAt)
	require.EqualValues(t, 1, report.Publication.Current.Present)
	require.True(t, len(events) > 0)
	require.NotNil(t, events[len(events)-1].Current)
	require.NotEmpty(t, events[len(events)-1].Current.LastCompleteAt)
	p := &publicationPublisher{opts: opts}
	due := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, p.scanStatus(t.Context(), ingest.FolderScanStatus{Degraded: true, VerificationDueAt: due, VerificationLag: 7 * time.Second}))
	last := events[len(events)-1]
	require.True(t, last.Degraded)
	require.Equal(t, due, last.VerificationDueAt)
	require.Equal(t, 7*time.Second, last.VerificationLag)
}
func TestPublicationStatusCallbackFailurePreventsComplete(t *testing.T) {
	t.Parallel()
	for _, event := range []string{"begin", "observed", "accepted", "verification", "completing"} {
		t.Run(event, func(t *testing.T) {
			completions := 0
			c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				if strings.HasSuffix(r.URL.Path, "/complete") {
					completions++
				}
				next.ServeHTTP(w, r)
			})
			root := t.TempDir()
			writePublicationFile(t, root, "a.md")
			opts := publicationOptions(root)
			sentinel := errors.New("synthetic sink unavailable")
			called := false
			opts.Status = func(_ context.Context, s PublicationStatus) error {
				if s.Event == event {
					called = true
					return sentinel
				}
				return nil
			}
			report, err := Run(t.Context(), c, opts)
			require.ErrorIs(t, err, sentinel)
			require.True(t, called)
			require.Zero(t, completions)
			require.Nil(t, report.Publication)
			status, err := s.NotePublicationStatus(t.Context(), "research")
			require.NoError(t, err)
			require.True(t, status.Incomplete)
		})
	}
}

func TestPublicationStatusCountersResetAcrossScans(t *testing.T) {
	t.Parallel()
	c, _ := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	cfg, policy, err := publicationPolicy(opts.Folder)
	require.NoError(t, err)
	opts.Folder = cfg
	p := &publicationPublisher{connection: c, opts: opts, policy: policy}
	scanner, err := ingest.NewFolderScanner(cfg, p.process, p.reject)
	require.NoError(t, err)
	scanner.BeforeScan = p.beforeScan
	scanner.ObservePresent = p.observePresent
	scanner.AfterScan = p.afterScan
	scanner.ScanStatus = p.scanStatus
	scanner.VerifyInterval = time.Minute
	require.NoError(t, scanner.ScanOnce(t.Context(), 1))
	first := p.report.Publication
	require.NoError(t, scanner.ScanOnce(t.Context(), 1))
	require.Equal(t, 1, p.report.Added)
	require.Equal(t, 1, p.report.Skipped)
	require.EqualValues(t, 1, p.report.Publication.Observed)
	require.Zero(t, p.report.Publication.Accepted)
	require.EqualValues(t, 1, p.report.Publication.Unchanged)
	require.EqualValues(t, 1, first.Accepted)
	// Mutating a delayed callback does not mutate the stored complete report.
	p.opts.Status = func(_ context.Context, s PublicationStatus) error {
		if s.Event == "complete" {
			s.Current.Present = 999
			return errors.New("synthetic deferred output error")
		}
		return nil
	}
	require.ErrorContains(t, scanner.ScanOnce(t.Context(), 1), "deferred output error")
	require.EqualValues(t, 1, p.report.Publication.Current.Present)
}
func TestPublicationStatusLostCompleteDoesNotInvokePostcommitGate(t *testing.T) {
	t.Parallel()
	completions := 0
	c, _ := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			completions++
			if completions == 1 {
				discardCommittedPublicationResponse(w, r, next)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	opts := publicationOptions(t.TempDir())
	retries := 0
	opts.Status = func(_ context.Context, s PublicationStatus) error {
		if s.Event == "retry" {
			retries++
			return errors.New("synthetic postcommit sink failure")
		}
		return nil
	}
	report, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Equal(t, 2, completions)
	require.Zero(t, retries)
	require.NotNil(t, report.Publication)
	require.False(t, report.Publication.Current.Incomplete)
}
