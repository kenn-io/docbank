package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
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
	maps.Copy(w.Header(), recorder.Header())
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
					// Windows can refuse to rename an open root. That failed
					// replacement must also prevent inventory completion.
					if err := os.Rename(root, filepath.Join(parent, "moved")); err != nil {
						return err
					}
					return os.Mkdir(root, 0700)
				case "file":
					if err := os.Rename(filepath.Join(root, "current.md"), filepath.Join(parent, "old.md")); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(root, "current.md"), []byte("# Synthetic note\n"), 0600)
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

func replacePublicationHead(t *testing.T, c *daemonconn.Connection, s *store.Store, nodeID int64, content string) api.ContentReplacementReceipt {
	t.Helper()
	node, err := s.NodeByID(t.Context(), nodeID)
	require.NoError(t, err)
	hash := sha256.Sum256([]byte(content))
	receipt, err := c.ReplaceContent(t.Context(), node.ID, node.Revision, "text/markdown", hex.EncodeToString(hash[:]), int64(len(content)), strings.NewReader(content))
	require.NoError(t, err)
	return receipt
}
func grantPublicationRecovery(t *testing.T, c *daemonconn.Connection, s *store.Store, source store.NotePublicationSource, head string) {
	t.Helper()
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	_, err = c.AcceptLocalPublishedNote(t.Context(), api.NotePublicationAcceptLocalRequest{Name: "research", SourceRef: source.Identity.SourceRef, DatabaseEpoch: status.DatabaseEpoch, ExpectedHeadVersionID: head, ExpectedSourceRevision: source.Identity.SourceRevision, OperationID: uuid.NewV4().String()})
	require.NoError(t, err)
}
func TestPublicationAcceptLocalEqualDigestForcesVerifiedUpload(t *testing.T) {
	t.Parallel()
	var uploads atomic.Int64
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if strings.HasSuffix(r.URL.Path, "/uploads") {
			uploads.Add(1)
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	original := publicationSources(t, s)["a.md"]
	manual := replacePublicationHead(t, c, s, original.Identity.NodeID, "# Manual archive revision\n")
	grantPublicationRecovery(t, c, s, original, manual.Version.ID)
	before := uploads.Load()
	report, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Equal(t, before+1, uploads.Load())
	require.Equal(t, 1, report.Updated)
	current := publicationSources(t, s)["a.md"]
	require.Equal(t, original.Identity.Hash, current.Identity.Hash)
	require.NotEqual(t, original.Identity.ContentVersionID, current.Identity.ContentVersionID)
	require.NotEqual(t, manual.Version.ID, current.Identity.ContentVersionID)
	require.Nil(t, current.Recovery)
	require.Empty(t, current.Conflict)
	versions, total, err := s.ContentVersions(t.Context(), original.Identity.NodeID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	ids := []string{}
	for _, v := range versions {
		ids = append(ids, v.ID)
	}
	require.Contains(t, ids, original.Identity.ContentVersionID)
	require.Contains(t, ids, manual.Version.ID)
	require.Contains(t, ids, current.Identity.ContentVersionID)
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.Zero(t, status.RecoveryPending)
}
func TestPublicationAcceptLocalHeadChangedRefuses(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	source := publicationSources(t, s)["a.md"]
	manual := replacePublicationHead(t, c, s, source.Identity.NodeID, "manual B")
	grantPublicationRecovery(t, c, s, source, manual.Version.ID)
	latest := replacePublicationHead(t, c, s, source.Identity.NodeID, "manual C")
	_, err = Run(t.Context(), c, opts)
	require.Error(t, err)
	node, err := s.NodeByID(t.Context(), source.Identity.NodeID)
	require.NoError(t, err)
	require.Equal(t, latest.Version.ID, node.CurrentVersionID)
	_, total, err := s.ContentVersions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 3, total)
	status, err := s.NotePublicationStatus(t.Context(), "research")
	require.NoError(t, err)
	require.EqualValues(t, 1, status.Conflicts)
	require.Zero(t, status.RecoveryPending)
}
func TestPublicationUnchangedDivergenceNeverAcknowledgesAsSuccess(t *testing.T) {
	t.Parallel()
	var armed atomic.Bool
	var seen, complete atomic.Int64
	c, s := newPublicationTestDaemon(t, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if armed.Load() {
			if strings.HasSuffix(r.URL.Path, "/seen") {
				seen.Add(1)
			}
			if strings.HasSuffix(r.URL.Path, "/complete") {
				complete.Add(1)
			}
		}
		next.ServeHTTP(w, r)
	})
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	source := publicationSources(t, s)["a.md"]
	manual := replacePublicationHead(t, c, s, source.Identity.NodeID, "manual edit")
	armed.Store(true)
	_, err = Run(t.Context(), c, opts)
	require.Error(t, err)
	require.Zero(t, seen.Load())
	require.Zero(t, complete.Load())
	node, err := s.NodeByID(t.Context(), source.Identity.NodeID)
	require.NoError(t, err)
	require.Equal(t, manual.Version.ID, node.CurrentVersionID)
}
func TestPublicationPrunedSourceNeverRemintsKey(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	source := publicationSources(t, s)["a.md"]
	manual := replacePublicationHead(t, c, s, source.Identity.NodeID, "manual edit")
	node, err := s.NodeByID(t.Context(), source.Identity.NodeID)
	require.NoError(t, err)
	result, err := s.PruneContentVersions(t.Context(), node.ID, node.Revision, store.VersionPruneSelector{VersionIDs: []string{source.Identity.ContentVersionID}}, true)
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedVersions)
	_, err = Run(t.Context(), c, opts)
	require.Error(t, err)
	current := publicationSources(t, s)["a.md"]
	require.Equal(t, source.Identity.NodeID, current.Identity.NodeID)
	require.Equal(t, "unavailable", current.Availability)
	node, err = s.NodeByID(t.Context(), node.ID)
	require.NoError(t, err)
	require.Equal(t, manual.Version.ID, node.CurrentVersionID)
	_, total, err := s.ContentVersions(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total)
}
func TestPublicationArchiveMoveKeepsSourceIdentity(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	source := publicationSources(t, s)["a.md"]
	node, err := s.NodeByID(t.Context(), source.Identity.NodeID)
	require.NoError(t, err)
	_, _, err = s.MoveToPath(t.Context(), node.ID, node.Revision, "/moved.md")
	require.NoError(t, err)
	_, err = Run(t.Context(), c, opts)
	require.NoError(t, err)
	current := publicationSources(t, s)["a.md"]
	require.Equal(t, source.Identity.NodeID, current.Identity.NodeID)
	require.Equal(t, source.Identity.ContentVersionID, current.Identity.ContentVersionID)
	moved, err := s.NodeByPath(t.Context(), "/moved.md")
	require.NoError(t, err)
	require.Equal(t, node.ID, moved.ID)
}
func TestPublicationConflictContinuesHealthySiblings(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	source := publicationSources(t, s)["a.md"]
	replacePublicationHead(t, c, s, source.Identity.NodeID, "manual edit")
	writePublicationFile(t, root, "z.md")
	report, err := Run(t.Context(), c, opts)
	require.ErrorIs(t, err, ErrFilesFailed)
	require.Equal(t, 1, report.Added)
	require.Equal(t, 1, report.Failed)
	current := publicationSources(t, s)
	require.Contains(t, current, "z.md")
	require.Equal(t, "diverged", current["a.md"].Conflict)
}
func TestPublicationArchivalManualHeadStillReplaces(t *testing.T) {
	t.Parallel()
	c, s := newPushTestDaemon(t, nil)
	root := t.TempDir()
	writePublicationFile(t, root, "a.md")
	opts := publicationOptions(root)
	opts.Publication = false
	_, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	cursors, err := c.PushCursors(t.Context(), "research")
	require.NoError(t, err)
	source := cursors["a.md"]
	manual := replacePublicationHead(t, c, s, source.NodeID, "manual edit")
	unchanged, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Equal(t, 1, unchanged.Skipped)
	node, err := s.NodeByID(t.Context(), source.NodeID)
	require.NoError(t, err)
	require.Equal(t, manual.Version.ID, node.CurrentVersionID)
	changed := []byte("changed local note")
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), changed, 0600))
	report, err := Run(t.Context(), c, opts)
	require.NoError(t, err)
	require.Equal(t, 1, report.Updated)
	after, err := c.PushCursors(t.Context(), "research")
	require.NoError(t, err)
	hash := sha256.Sum256(changed)
	require.Equal(t, hex.EncodeToString(hash[:]), after["a.md"].Hash)
	require.Equal(t, source.NodeID, after["a.md"].NodeID)
}
