package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

func publicationProblem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q,"title":"Synthetic failure"}`, status, code)
}
func discardCommittedPublicationResponse(w http.ResponseWriter, r *http.Request, next http.Handler) {
	recorder := httptest.NewRecorder()
	next.ServeHTTP(recorder, r)
	if recorder.Code >= 200 && recorder.Code < 300 {
		panic(http.ErrAbortHandler)
	}
	for key, values := range recorder.Header() {
		w.Header()[key] = values
	}
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(recorder.Body.Bytes())
}
func TestPublicationLostUploadReceiptRefreshesWithoutDuplicateVersion(t *testing.T) {
	t.Parallel()
	var lost atomic.Bool
	var requestsMu sync.Mutex
	var trace []string
	var operations []string
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		requestsMu.Lock()
		trace = append(trace, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/uploads") {
			operations = append(operations, r.URL.Query().Get("operation_id"))
		}
		requestsMu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/uploads") && !lost.Swap(true) {
			discardCommittedPublicationResponse(w, r, next)
			return
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	report, err := Run(t.Context(), c, publicationOptions(root))
	require.NoError(t, err)
	require.Equal(t, 1, report.Added)
	requestsMu.Lock()
	require.Len(t, operations, 2)
	require.Equal(t, operations[0], operations[1])
	require.Equal(t, 2, strings.Count(strings.Join(trace, "\n"), "GET /api/v1/note-publications/research/sources"))
	requestsMu.Unlock()
	sources := publicationSources(t, s)
	_, total, err := s.ContentVersions(t.Context(), sources["a.md"].Identity.NodeID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
}
func TestPublicationSourceStaleRetriesWithinEpoch(t *testing.T) {
	t.Parallel()
	var failed atomic.Bool
	var begins atomic.Int64
	var scansMu sync.Mutex
	var scans []string
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scans") {
			begins.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/uploads") {
			scansMu.Lock()
			scans = append(scans, r.URL.Query().Get("scan_id"))
			scansMu.Unlock()
			if !failed.Swap(true) {
				_, _ = io.Copy(io.Discard, r.Body)
				publicationProblem(w, 409, "publication_source_stale")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	_, err := Run(t.Context(), c, publicationOptions(root))
	require.NoError(t, err)
	require.EqualValues(t, 1, begins.Load())
	scansMu.Lock()
	require.Len(t, scans, 2)
	require.Equal(t, scans[0], scans[1])
	scansMu.Unlock()
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Present)
}
func TestPublicationSupersededStopsWithoutRebegin(t *testing.T) {
	t.Parallel()
	var begins atomic.Int64
	var first atomic.Bool
	var s *store.Store
	c, db := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scans") {
			begins.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/uploads") && !first.Swap(true) {
			_, err := s.BeginNotePublicationScan(r.Context(), store.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: publicationTestPolicy, OperationID: publicationTestUUID()})
			require.NoError(t, err)
		}
		next.ServeHTTP(w, r)
	})
	s = db
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	_, err := Run(t.Context(), c, publicationOptions(root))
	require.Error(t, err)
	code, ok := daemonconn.ProblemCode(err)
	require.True(t, ok)
	require.Equal(t, "publication_scan_superseded", code)
	require.EqualValues(t, 1, begins.Load())
}
func TestPublicationReconnectPreservesEpochAndOperation(t *testing.T) {
	t.Parallel()
	var begins, reconnect atomic.Int64
	var dropped atomic.Bool
	var origin string
	var mu sync.Mutex
	var scans, ops, events []string
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		mu.Lock()
		origin = "http://" + r.Host
		events = append(events, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/uploads") {
			scans = append(scans, r.URL.Query().Get("scan_id"))
			ops = append(ops, r.URL.Query().Get("operation_id"))
		}
		mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scans") {
			begins.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/uploads") && !dropped.Swap(true) {
			_, _ = io.Copy(io.Discard, r.Body)
			panic(http.ErrAbortHandler)
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	opts.Reconnect = func(context.Context) (*daemonconn.Connection, error) {
		reconnect.Add(1)
		mu.Lock()
		url := origin
		mu.Unlock()
		return daemonconn.NewPushConnection(url, "synthetic-key")
	}
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.EqualValues(t, 1, reconnect.Load())
	require.EqualValues(t, 1, begins.Load())
	mu.Lock()
	require.Len(t, scans, 2)
	require.Equal(t, scans[0], scans[1])
	require.Equal(t, ops[0], ops[1])
	first, second := -1, -1
	for i, e := range events {
		if strings.HasSuffix(e, "/uploads") {
			if first < 0 {
				first = i
			} else {
				second = i
			}
		}
	}
	require.Contains(t, events[first+1:second], "/api/v1/note-publications/research/sources")
	mu.Unlock()
	src := publicationSources(t, s)
	_, n, err := s.ContentVersions(t.Context(), src["a.md"].Identity.NodeID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
func TestPublicationReconnectSupersededStops(t *testing.T) {
	t.Parallel()
	var dropped atomic.Bool
	var begins, reconnect atomic.Int64
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scans") {
			begins.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/uploads") && !dropped.Swap(true) {
			_, _ = io.Copy(io.Discard, r.Body)
			panic(http.ErrAbortHandler)
		}
		next.ServeHTTP(w, r)
	})
	opts := publicationOptions(t.TempDir())
	writePublicationFile(t, opts.Folder.Source, "a.md")
	opts.Reconnect = func(ctx context.Context) (*daemonconn.Connection, error) {
		reconnect.Add(1)
		_, err := s.BeginNotePublicationScan(ctx, store.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: publicationTestPolicy, OperationID: publicationTestUUID()})
		return c, err
	}
	_, err := Run(t.Context(), c, opts)
	require.Error(t, err)
	code, _ := daemonconn.ProblemCode(err)
	require.Equal(t, "publication_scan_superseded", code)
	require.EqualValues(t, 1, reconnect.Load())
	require.EqualValues(t, 1, begins.Load())
}
func TestPublicationReconnectRejectsTerminalErrors(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{{"auth", 401, `{"status":401,"code":"unauthorized"}`}, {"proof", 403, `{"status":403,"code":"forbidden"}`}, {"capability", 404, `{"status":404,"code":"not_found"}`}, {"decode", 200, `{"invalid":true}`}} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			c, _ := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				if strings.Contains(r.URL.Path, "/note-publications/") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.status)
					_, _ = w.Write([]byte(test.body))
					return
				}
				next.ServeHTTP(w, r)
			})
			opts := publicationOptions(t.TempDir())
			opts.Reconnect = func(context.Context) (*daemonconn.Connection, error) { calls.Add(1); return c, nil }
			_, err := Run(t.Context(), c, opts)
			require.Error(t, err)
			require.Zero(t, calls.Load())
		})
	}
}
func TestPublicationCompletionReplayCannotMutateSealedScan(t *testing.T) {
	t.Parallel()
	var dropped atomic.Bool
	var mu sync.Mutex
	var completed []string
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if strings.HasSuffix(r.URL.Path, "/complete") {
			mu.Lock()
			completed = append(completed, r.URL.Path)
			mu.Unlock()
			if !dropped.Swap(true) {
				discardCommittedPublicationResponse(w, r, next)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	_, err := Run(t.Context(), c, publicationOptions(root))
	require.NoError(t, err)
	mu.Lock()
	require.Len(t, completed, 2)
	require.Equal(t, completed[0], completed[1])
	mu.Unlock()
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.False(t, status.Incomplete)
	require.EqualValues(t, 1, status.Present)
}
func TestPublicationRestartConvergesAfterOutage(t *testing.T) {
	t.Parallel()
	var offline atomic.Bool
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if offline.Load() && strings.Contains(r.URL.Path, "/note-publications/") {
			publicationProblem(w, 503, "unavailable")
			return
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	writePublicationFile(t, root, "b.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	offline.Store(true)
	require.NoError(t, os.Remove(filepath.Join(root, "a.md")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.md"), []byte("# New bytes\n"), 0600))
	_, err = Run(t.Context(), c, opts)
	require.Error(t, err)
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 2, status.Present)
	offline.Store(false)
	_, err = Run(t.Context(), c, opts)
	require.NoError(t, err)
	status, err = s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Present)
	require.EqualValues(t, 1, status.Absent)
}
func TestPublicationIncompleteEvidenceNeverRetires(t *testing.T) {
	for _, failure := range []string{"seen", "upload", "complete", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			var fail atomic.Bool
			c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
				if fail.Load() && (strings.HasSuffix(r.URL.Path, "/"+failure) || failure == "seen" && strings.HasSuffix(r.URL.Path, "/seen") || failure == "upload" && strings.HasSuffix(r.URL.Path, "/uploads")) {
					_, _ = io.Copy(io.Discard, r.Body)
					publicationProblem(w, 503, "unavailable")
					return
				}
				next.ServeHTTP(w, r)
			})
			root := t.TempDir()
			writePublicationFile(t, root, "retained.md")
			writePublicationFile(t, root, "current.md")
			opts := publicationOptions(root)
			_, err := Run(t.Context(), c, opts)
			require.NoError(t, err)
			require.NoError(t, os.Remove(filepath.Join(root, "retained.md")))
			if failure == "upload" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "current.md"), []byte("changed"), 0600))
			}
			fail.Store(true)
			ctx := t.Context()
			if failure == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err = Run(ctx, c, opts)
			require.Error(t, err)
			status, err := s.NotePublicationStatus(t.Context(), "research")
			require.NoError(t, err)
			require.EqualValues(t, 2, status.Present)
			require.Zero(t, status.Absent)
		})
	}
}

func TestPublicationUnprocessedObservationDoesNotDoubleAcknowledge(t *testing.T) {
	t.Parallel()
	c, _ := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	cfg, policy, err := publicationPolicy(opts.Folder)
	require.NoError(t, err)
	opts.Folder = cfg
	p := &publicationPublisher{connection: c, opts: opts, policy: policy}
	require.NoError(t, p.beforeScan(t.Context(), ingest.FolderScan{Number: 1}))
	info, err := os.Stat(filepath.Join(root, "a.md"))
	require.NoError(t, err)
	force, err := p.observePresent(t.Context(), ingest.FolderPresence{Ref: "a.md", Info: info, Stable: true, Processed: false, Digest: p.sources["a.md"].Identity.Hash})
	require.NoError(t, err)
	require.True(t, force)
	require.Empty(t, p.seen, "an unprocessed touch must be acknowledged only after its processor")
}
func TestPublicationReconnectFailureWaitsForSameOperation(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	c := daemonconn.New(url, "synthetic-key")
	_, transportErr := c.NotePublicationStatus(t.Context(), "research")
	require.True(t, daemonconn.IsTransportError(transportErr))
	var reconnects, waits, actions int
	p := &publicationPublisher{connection: c, opts: publicationOptions(t.TempDir())}
	p.opts.Watch = true
	p.opts.Reconnect = func(context.Context) (*daemonconn.Connection, error) {
		reconnects++
		if reconnects < 8 {
			return nil, transportErr
		}
		return c, nil
	}
	p.wait = func(context.Context, time.Duration) error { waits++; return nil }
	err := p.retry(t.Context(), func() error {
		actions++
		if actions == 1 {
			return transportErr
		}
		return nil
	}, nil)
	require.NoError(t, err)
	require.Equal(t, 8, reconnects)
	require.Equal(t, 1, waits)
	require.Equal(t, 2, actions)
}
func TestPublicationWatchRecoveryBurstWaitsWithoutRebegin(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	c := daemonconn.New(url, "synthetic-key")
	_, failure := c.NotePublicationStatus(t.Context(), "research")
	require.True(t, daemonconn.IsTransportError(failure))
	opts := publicationOptions(t.TempDir())
	opts.Watch = true
	p := &publicationPublisher{connection: c, opts: opts}
	actions, waits := 0, 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.wait = func(waitCtx context.Context, delay time.Duration) error {
		waits++
		require.Equal(t, opts.Folder.ScanInterval.Std(), delay)
		require.Equal(t, 4, actions)
		cancel()
		return waitCtx.Err()
	}
	err := p.retry(ctx, func() error { actions++; return failure }, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, waits)
	require.Equal(t, 4, actions)
}

func TestPublicationDelayedOlderUploadCannotWin(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	scan, err := c.BeginNotePublicationScan(t.Context(), api.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: publicationTestPolicy, OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
	makeRequest := func(content string, revision int64) api.NotePublicationUploadRequest {
		sum := sha256.Sum256([]byte(content))
		return api.NotePublicationUploadRequest{Name: "research", ScanID: scan.ScanID, SourceRef: "a.md", FileName: "a.md", ParentPath: "/notes", OperationID: uuid.NewV4().String(), Hash: hex.EncodeToString(sum[:]), Size: int64(len(content)), ExpectedSourceRevision: revision}
	}
	first := makeRequest("first", 0)
	a, err := c.UploadPublishedNote(t.Context(), first, strings.NewReader("first"))
	require.NoError(t, err)
	second := makeRequest("newer", a.Source.Identity.SourceRevision)
	b, err := c.UploadPublishedNote(t.Context(), second, strings.NewReader("newer"))
	require.NoError(t, err)
	replay, err := c.UploadPublishedNote(t.Context(), first, strings.NewReader("first"))
	require.NoError(t, err)
	require.Equal(t, a.Source.Identity.ContentVersionID, replay.Source.Identity.ContentVersionID)
	stale := first
	stale.OperationID = uuid.NewV4().String()
	_, err = c.UploadPublishedNote(t.Context(), stale, strings.NewReader("first"))
	code, _ := daemonconn.ProblemCode(err)
	require.Equal(t, "publication_source_stale", code)
	node, err := s.NodeByID(t.Context(), b.Source.Identity.NodeID)
	require.NoError(t, err)
	require.Equal(t, b.Source.Identity.ContentVersionID, node.CurrentVersionID)
	_, total, err := s.ContentVersions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	_, err = c.CompleteNotePublicationScan(t.Context(), api.NotePublicationCompleteRequest{Name: "research", ScanID: scan.ScanID, PolicyFingerprint: publicationTestPolicy, InventoryComplete: true})
	require.NoError(t, err)
	_, err = c.AcknowledgePublishedNotes(t.Context(), api.NotePublicationSeenRequest{Name: "research", ScanID: scan.ScanID, Bindings: []api.PublishedNoteIdentity{b.Source.Identity}})
	code, _ = daemonconn.ProblemCode(err)
	require.Equal(t, "publication_scan_sealed", code)
}
func TestPublicationLostBeginRetainsOperation(t *testing.T) {
	t.Parallel()
	var lost atomic.Bool
	var mu sync.Mutex
	var operations []string
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/scans") {
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var req api.NotePublicationBeginRequest
			require.NoError(t, json.Unmarshal(b, &req))
			mu.Lock()
			operations = append(operations, req.OperationID)
			mu.Unlock()
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			if !lost.Swap(true) {
				discardCommittedPublicationResponse(w, r, next)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	_, err := Run(t.Context(), c, publicationOptions(root))
	require.NoError(t, err)
	mu.Lock()
	require.Len(t, operations, 2)
	require.Equal(t, operations[0], operations[1])
	mu.Unlock()
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.False(t, status.Incomplete)
}

func TestPublicationReplacementNeverRetires(t *testing.T) {
	for _, kind := range []string{"root", "file", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			c, s := newPushTestDaemon(t, nil)
			parent := t.TempDir()
			root := filepath.Join(parent, "source")
			writePublicationFile(t, root, "retained.md")
			writePublicationFile(t, root, "current.md")
			opts := publicationOptions(root)
			_, err := Run(t.Context(), c, opts)
			require.NoError(t, err)
			require.NoError(t, os.Remove(filepath.Join(root, "retained.md")))
			require.NoError(t, os.WriteFile(filepath.Join(root, "current.md"), []byte("second stable bytes"), 0600))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts.Progress = func(ref, outcome string) error {
				if outcome != "updated" {
					return nil
				}
				switch kind {
				case "root":
					require.NoError(t, os.Rename(root, filepath.Join(parent, "moved")))
					require.NoError(t, os.Mkdir(root, 0700))
				case "file":
					require.NoError(t, os.Rename(filepath.Join(root, "current.md"), filepath.Join(parent, "old.md")))
					writePublicationFile(t, root, "current.md")
				case "cancel":
					cancel()
				}
				return nil
			}
			_, err = Run(ctx, c, opts)
			require.Error(t, err)
			status, err := s.NotePublicationStatus(t.Context(), "research")
			require.NoError(t, err)
			require.EqualValues(t, 2, status.Present)
			require.Zero(t, status.Absent)
		})
	}
}
