//go:build !darwin || arm64

package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"hegel.dev/go/hegel"
)

const (
	publicationPropertyExamples = 2000
	publicationPropertyShards   = 8
)

// Both properties share this cap. Each shard owns at most one live fixture,
// including shrinking, and returns its slot only after all cleanup has run.
var publicationPropertySlots = make(chan struct{}, 4)

func testPublicationProperty(t *testing.T, baseSeed int64, body func(*hegel.T)) {
	t.Helper()
	for shard := range publicationPropertyShards {
		seed := baseSeed + 100*int64(shard)
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			t.Parallel()
			select {
			case publicationPropertySlots <- struct{}{}:
				defer func() { <-publicationPropertySlots }()
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			hegel.Test(t, body,
				hegel.WithTestCases(publicationPropertyExamples/publicationPropertyShards),
				hegel.WithSeed(seed), hegel.WithDatabase(t.TempDir()), hegel.WithStatistics(true))
		})
	}
}

// Each example owns a real HTTP daemon, store and private synthetic source tree.
// Explicit per-example cleanup also runs during shrinking, bounding open files,
// retained objects and disk rather than retaining 2,000 fixtures until test end.
type publicationCase struct {
	connection *daemonconn.Connection
	store      *store.Store
	root, url  string
	close      func()
}

func newPublicationCase(t testing.TB, hook func(http.ResponseWriter, *http.Request, http.Handler)) (f publicationCase) {
	t.Helper()
	base, err := os.MkdirTemp("", "docbank-publication-case-")
	require.NoError(t, err)
	var s *store.Store
	var blobs *blob.Store
	var server *api.Server
	var ts *httptest.Server
	ready := false
	f.close = func() {
		if ts != nil {
			ts.Close()
		}
		if server != nil {
			server.Close()
		}
		if blobs != nil {
			require.NoError(t, blobs.Close())
		}
		if s != nil {
			require.NoError(t, s.Close())
		}
		require.NoError(t, os.RemoveAll(base))
	}
	defer func() {
		if !ready {
			f.close()
		}
	}()
	s, err = store.Open(t.Context(), filepath.Join(base, "vault.db"))
	require.NoError(t, err)
	dir := filepath.Join(base, "blobs")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "tmp"), 0700))
	blobs, err = blob.New(store.NewPackCatalog(s), dir)
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-key"
	server = api.NewServer(api.Deps{Store: s, Blobs: blobs, VaultRoot: base, Cfg: cfg, Logger: slog.New(slog.DiscardHandler)})
	handler := server.Handler()
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil {
			hook(w, r, handler)
		} else {
			handler.ServeHTTP(w, r)
		}
	}))
	f.connection, err = daemonconn.NewPushConnection(ts.URL, "synthetic-key")
	require.NoError(t, err)
	f.store, f.url, f.root = s, ts.URL, filepath.Join(base, "local")
	require.NoError(t, os.Mkdir(f.root, 0700))
	ready = true
	return f
}
func publicationModelHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func drawPublicationBytes(ht *hegel.T) []byte {
	raw := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 255)))
	if len(raw) > 64 {
		raw = raw[:64]
	} // Bound materialized bytes, including arbitrary binary.
	b := make([]byte, len(raw))
	for i, v := range raw {
		b[i] = byte(v)
	}
	return b
}

type publicationModelSource struct {
	local                                         []byte
	localPresent, excluded, known, present, grant bool
	hash, acceptedVersion, headVersion            string
	acceptedSize                                  int64
	nodeID, revision                              int64
	versions                                      []string // Expected hashes in immutable history, including repeats.
	versionIDs                                    map[string]string
	newVersion                                    bool
}

// The oracle knows local edits, accepted bytes, archive edits and explicit grants.
// It never derives expected membership or history from publisher/store helpers.
func TestPublicationConvergenceProperty(t *testing.T) {
	testPublicationProperty(t, 2026110703, func(ht *hegel.T) {
		commands := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 111)).MinSize(1))
		if len(commands) > 32 {
			commands = commands[:32]
		}
		initial := hegel.Draw(ht, hegel.Integers(0, 8))
		offline := atomic.Bool{}
		var dropSuffix atomic.Value
		dropSuffix.Store("")
		f := newPublicationCase(ht, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
			if offline.Load() && strings.Contains(r.URL.Path, "/note-publications/") {
				publicationProblem(w, 503, "internal")
				return
			}
			suffix := dropSuffix.Load().(string)
			if suffix != "" && strings.HasSuffix(r.URL.Path, suffix) {
				dropSuffix.Store("")
				discardCommittedPublicationResponse(w, r, next)
				return
			}
			next.ServeHTTP(w, r)
		})
		defer f.close()
		model := map[string]*publicationModelSource{}
		for i := range 8 {
			ref := fmt.Sprintf("folder-%d/note-%d.md", i%2, i)
			m := &publicationModelSource{local: drawPublicationBytes(ht), localPresent: i < initial, versionIDs: map[string]string{}}
			model[ref] = m
			if m.localPresent {
				pathname := filepath.Join(f.root, filepath.FromSlash(ref))
				require.NoError(ht, os.MkdirAll(filepath.Dir(pathname), 0700))
				require.NoError(ht, os.WriteFile(pathname, m.local, 0600))
			}
		}
		var lastScan api.NotePublicationScan
		var namespace int64
		reconcile := func() {
			opts := publicationOptions(f.root)
			for ref, m := range model {
				if m.excluded {
					opts.Folder.Exclude = append(opts.Folder.Exclude, ref)
				}
			}
			opts.Status = func(_ context.Context, s PublicationStatus) error {
				if s.Event == "begin" {
					lastScan = s.Scan
				}
				return nil
			}
			complete := !offline.Load()
			for _, m := range model {
				if m.localPresent && !m.excluded && m.known && m.headVersion != m.acceptedVersion && !m.grant {
					complete = false
				}
			}
			// Predict positive accepts first; a different source's conflict must not
			// prevent these. Negative membership requires the whole inventory.
			if !offline.Load() {
				for _, m := range model {
					m.newVersion = false
					if !m.localPresent || m.excluded || (m.known && m.headVersion != m.acceptedVersion && !m.grant) {
						continue
					}
					hash := publicationModelHash(m.local)
					changed := !m.known || hash != m.hash || m.grant
					if changed {
						m.versions = append(m.versions, hash)
						m.newVersion = true
					}
					if !m.known {
						m.revision = 1
					} else if changed || !m.present {
						m.revision++
					}
					m.hash, m.known, m.present, m.grant = hash, true, true, false
					m.acceptedSize = int64(len(m.local))
				}
				if complete {
					for _, m := range model {
						if m.known && (!m.localPresent || m.excluded) && m.present {
							m.present = false
							m.revision++
						}
					}
				}
			}
			report, err := Run(ht.Context(), f.connection, opts)
			if complete {
				require.NoError(ht, err)
				require.NotNil(ht, report.Publication)
			} else {
				require.Error(ht, err)
				require.Nil(ht, report.Publication)
			}
			status, err := f.store.NotePublicationStatus(ht.Context(), "research")
			require.NoError(ht, err)
			require.GreaterOrEqual(ht, status.NamespaceRevision, namespace)
			namespace = status.NamespaceRevision
			if !offline.Load() {
				require.Equal(ht, !complete, status.Incomplete)
			}
			page, err := f.store.NotePublicationSources(ht.Context(), store.NotePublicationSourcesRequest{Name: "research", ScanID: lastScan.ScanID, Limit: 1000})
			require.NoError(ht, err)
			sources := map[string]store.NotePublicationSource{}
			for _, source := range page.Items {
				sources[source.Identity.SourceRef] = source
			}
			nodes := map[int64]string{}
			expectedSources := 0
			expectedPresent := int64(0)
			for ref, m := range model {
				if !m.known {
					require.NotContains(ht, sources, ref)
					continue
				}
				expectedSources++
				source, exists := sources[ref]
				require.True(ht, exists, "missing %s", ref)
				if m.nodeID == 0 {
					m.nodeID = source.Identity.NodeID
				} else {
					require.Equal(ht, m.nodeID, source.Identity.NodeID)
				}
				require.NotContains(ht, nodes, m.nodeID)
				nodes[m.nodeID] = ref
				require.Equal(ht, m.hash, source.Identity.Hash)
				require.Equal(ht, m.acceptedSize, source.Identity.Size, ref)
				require.Equal(ht, m.revision, source.Identity.SourceRevision, ref)
				wantPresence := "absent"
				if m.present {
					wantPresence = "present"
					expectedPresent++
				}
				require.Equal(ht, wantPresence, source.Presence, ref)
				versions, total, err := f.store.ContentVersions(ht.Context(), m.nodeID, 100, 0)
				require.NoError(ht, err)
				require.Equal(ht, len(m.versions), total, ref)
				actualHashes := map[string]int{}
				wantHashes := map[string]int{}
				actualIDs := map[string]string{}
				for _, v := range versions {
					actualHashes[v.BlobHash]++
					actualIDs[v.ID] = v.BlobHash
				}
				for _, hash := range m.versions {
					wantHashes[hash]++
				}
				require.Equal(ht, wantHashes, actualHashes, ref)
				for id, hash := range m.versionIDs {
					require.Equal(ht, hash, actualIDs[id], "retained version %s", id)
				}
				if m.newVersion {
					require.NotContains(ht, m.versionIDs, source.Identity.ContentVersionID)
					m.acceptedVersion, m.headVersion = source.Identity.ContentVersionID, source.Identity.ContentVersionID
					m.newVersion = false
				}
				require.Equal(ht, m.acceptedVersion, source.Identity.ContentVersionID)
				require.NotNil(ht, source.ArchiveHead)
				require.Equal(ht, m.headVersion, source.ArchiveHead.ContentVersionID)
				m.versionIDs = actualIDs
			}
			require.Len(ht, sources, expectedSources)
			require.Equal(ht, expectedPresent, status.Present)
		}
		reconcile()
		refs := make([]string, 0, 8)
		for ref := range model {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for step, token := range commands {
			kind, index := token/8, token%8
			ref := refs[index]
			m := model[ref]
			pathname := filepath.Join(f.root, filepath.FromSlash(ref))
			bytes := drawPublicationBytes(ht)
			switch kind {
			case 0, 1:
				m.local = bytes
				m.localPresent = true
			case 2: // Equal size and restored mtime, with changed bytes where possible.
				if m.localPresent {
					info, err := os.Stat(pathname)
					require.NoError(ht, err)
					m.local = append([]byte(nil), m.local...)
					if len(m.local) > 0 {
						m.local[0] ^= 1
					}
					require.NoError(ht, os.WriteFile(pathname, m.local, 0600))
					require.NoError(ht, os.Chtimes(pathname, info.ModTime(), info.ModTime()))
				}
			case 3:
				if m.localPresent {
					stamp := time.Unix(1800000000+int64(step), 0)
					require.NoError(ht, os.Chtimes(pathname, stamp, stamp))
				}
			case 4:
				target := model[refs[(index+1)%8]]
				if m.localPresent && !target.localPresent {
					to := filepath.Join(f.root, filepath.FromSlash(refs[(index+1)%8]))
					require.NoError(ht, os.MkdirAll(filepath.Dir(to), 0700))
					require.NoError(ht, os.Rename(pathname, to))
					target.local, target.localPresent = append([]byte(nil), m.local...), true
					m.localPresent = false
				}
			case 5:
				if m.localPresent {
					require.NoError(ht, os.Remove(pathname))
					m.localPresent = false
				}
			case 6:
				m.localPresent = true
			case 7:
				m.excluded = true
			case 8:
				m.excluded = false
			case 9:
				offline.Store(true)
				m.local = bytes
				m.localPresent = true
			case 10:
				dropSuffix.Store([]string{"/uploads", "/seen", "/complete"}[step%3])
				m.local = bytes
				m.localPresent = true
			case 11:
				if m.known {
					body := fmt.Sprintf("# manual-%d\n", step) + string(bytes)
					if len(body) > 64 {
						body = body[:64]
					}
					node, err := f.store.NodeByID(ht.Context(), m.nodeID)
					require.NoError(ht, err)
					hash := publicationModelHash([]byte(body))
					receipt, err := f.connection.ReplaceContent(ht.Context(), node.ID, node.Revision, "text/markdown", hash, int64(len(body)), strings.NewReader(body))
					require.NoError(ht, err)
					m.versions = append(m.versions, hash)
					m.headVersion = receipt.Version.ID
					m.grant = false
				}
			case 12:
				if m.known && m.headVersion != m.acceptedVersion {
					status, err := f.store.NotePublicationStatus(ht.Context(), "research")
					require.NoError(ht, err)
					_, err = f.connection.AcceptLocalPublishedNote(ht.Context(), api.NotePublicationAcceptLocalRequest{Name: "research", DatabaseEpoch: status.DatabaseEpoch, SourceRef: ref, ExpectedHeadVersionID: m.headVersion, ExpectedSourceRevision: m.revision, OperationID: publicationTestUUID()})
					require.NoError(ht, err)
					m.grant = true
				}
			case 13:
				var err error
				f.connection, err = daemonconn.NewPushConnection(f.url, "synthetic-key")
				require.NoError(ht, err)
			}
			if (kind == 0 || kind == 1 || kind == 6 || kind == 9 || kind == 10) && m.localPresent {
				require.NoError(ht, os.MkdirAll(filepath.Dir(pathname), 0700))
				require.NoError(ht, os.WriteFile(pathname, m.local, 0600))
			}
			reconcile()
			offline.Store(false)
			dropSuffix.Store("")
		}
		// A final healthy full stable pass may leave only explicitly ungranted
		// archive conflicts. No prefix silently shrinks retained history.
		reconcile()
	})
}

// This second oracle owns the accepted hash/version, liveness, source revision,
// scan generation and sealing bit. Reordered messages may return an old receipt,
// but must never change the current authority or append an extra version.
func TestPublicationFencingProperty(t *testing.T) {
	testPublicationProperty(t, 2026110704, func(ht *hegel.T) {
		commands := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, 10)).MinSize(1))
		if len(commands) > 32 {
			commands = commands[:32]
		}
		f := newPublicationCase(ht, nil)
		defer f.close()
		begin := func() api.NotePublicationScan {
			scan, err := f.connection.BeginNotePublicationScan(ht.Context(), api.NotePublicationBeginRequest{Name: "research", Destination: "/notes", PolicyFingerprint: publicationTestPolicy, OperationID: publicationTestUUID()})
			require.NoError(ht, err)
			return scan
		}
		scan := begin()
		var older []api.NotePublicationScan
		makeRequest := func(body []byte, scanID string, revision int64) api.NotePublicationUploadRequest {
			return api.NotePublicationUploadRequest{Name: "research", ScanID: scanID, SourceRef: "a.md", FileName: "a.md", ParentPath: "/notes", Hash: publicationModelHash(body), Size: int64(len(body)), ExpectedSourceRevision: revision, OperationID: publicationTestUUID()}
		}
		type priorUpload struct {
			request api.NotePublicationUploadRequest
			body    []byte
			receipt api.NotePublicationReceipt
		}
		initial := drawPublicationBytes(ht)
		firstReq := makeRequest(initial, scan.ScanID, 0)
		first, err := f.connection.UploadPublishedNote(ht.Context(), firstReq, strings.NewReader(string(initial)))
		require.NoError(ht, err)
		prior := []priorUpload{{request: firstReq, body: initial, receipt: first}}
		binding := first.Source.Identity
		nodeID, version, hash, revision := binding.NodeID, binding.ContentVersionID, publicationModelHash(initial), int64(1)
		require.EqualValues(ht, 1, binding.SourceRevision)
		present, seen, sealed := true, true, false
		hashes := []string{hash}
		ids := map[string]string{version: hash}
		var namespace int64
		expectCode := func(err error, want string) {
			require.Error(ht, err)
			code, ok := daemonconn.ProblemCode(err)
			require.True(ht, ok, "expected %s, got %v", want, err)
			require.Equal(ht, want, code)
		}
		completeRequest := func(v api.NotePublicationScan) api.NotePublicationCompleteRequest {
			return api.NotePublicationCompleteRequest{Name: "research", ScanID: v.ScanID, PolicyFingerprint: v.PolicyFingerprint, InventoryComplete: true}
		}
		for _, command := range commands {
			body := drawPublicationBytes(ht)
			switch command {
			case 0:
				older = append(older, scan)
				previous := scan.Epoch
				scan = begin()
				require.Equal(ht, previous+1, scan.Epoch)
				seen, sealed = false, false
			case 1:
				request := makeRequest(body, scan.ScanID, revision)
				receipt, err := f.connection.UploadPublishedNote(ht.Context(), request, strings.NewReader(string(body)))
				if sealed {
					expectCode(err, "publication_scan_sealed")
					break
				}
				require.NoError(ht, err)
				nextHash := publicationModelHash(body)
				changed := nextHash != hash
				if changed {
					hashes = append(hashes, nextHash)
					require.NotContains(ht, ids, receipt.Source.Identity.ContentVersionID)
					version = receipt.Source.Identity.ContentVersionID
					ids[version] = nextHash
				} else {
					require.Equal(ht, version, receipt.Source.Identity.ContentVersionID)
				}
				if changed || !present {
					revision++
				}
				hash = nextHash
				present, seen = true, true
				require.Equal(ht, revision, receipt.Source.Identity.SourceRevision)
				prior = append(prior, priorUpload{request: request, body: body, receipt: receipt})
			case 2: // Deliver any old successful operation again, after arbitrary steps.
				old := prior[hegel.Draw(ht, hegel.Integers(0, len(prior)-1))]
				receipt, err := f.connection.UploadPublishedNote(ht.Context(), old.request, strings.NewReader(string(old.body)))
				if old.request.ScanID != scan.ScanID {
					expectCode(err, "publication_scan_superseded")
				} else {
					require.NoError(ht, err)
					require.Equal(ht, old.receipt, receipt)
				}
			case 3:
				request := makeRequest(body, scan.ScanID, revision-1)
				_, err := f.connection.UploadPublishedNote(ht.Context(), request, strings.NewReader(string(body)))
				code := "publication_source_stale"
				if sealed {
					code = "publication_scan_sealed"
				}
				expectCode(err, code)
			case 4:
				if len(older) > 0 {
					old := older[hegel.Draw(ht, hegel.Integers(0, len(older)-1))]
					_, err := f.connection.AcknowledgePublishedNotes(ht.Context(), api.NotePublicationSeenRequest{Name: "research", ScanID: old.ScanID, Bindings: []api.PublishedNoteIdentity{binding}})
					expectCode(err, "publication_scan_superseded")
				}
			case 5:
				_, err := f.connection.AcknowledgePublishedNotes(ht.Context(), api.NotePublicationSeenRequest{Name: "research", ScanID: scan.ScanID, Bindings: []api.PublishedNoteIdentity{binding}})
				if sealed {
					expectCode(err, "publication_scan_sealed")
				} else {
					require.NoError(ht, err)
					if !present {
						revision++
					}
					present, seen = true, true
				}
			case 6:
				_, err := f.connection.CompleteNotePublicationScan(ht.Context(), completeRequest(scan))
				require.NoError(ht, err)
				if !sealed && !seen && present {
					present = false
					revision++
				}
				sealed = true
			case 7:
				stale := binding
				stale.Hash = strings.Repeat("0", 64)
				_, err := f.connection.AcknowledgePublishedNotes(ht.Context(), api.NotePublicationSeenRequest{Name: "research", ScanID: scan.ScanID, Bindings: []api.PublishedNoteIdentity{stale}})
				code := "publication_source_stale"
				if sealed {
					code = "publication_scan_sealed"
				}
				expectCode(err, code)
			case 8:
				if len(older) > 0 {
					old := older[hegel.Draw(ht, hegel.Integers(0, len(older)-1))]
					_, err := f.connection.CompleteNotePublicationScan(ht.Context(), completeRequest(old))
					expectCode(err, "publication_scan_superseded")
				}
			case 9:
				old := prior[hegel.Draw(ht, hegel.Integers(0, len(prior)-1))]
				changed := append(append([]byte(nil), old.body...), 1)
				altered := old.request
				altered.Hash = publicationModelHash(changed)
				altered.Size = int64(len(changed))
				_, err := f.connection.UploadPublishedNote(ht.Context(), altered, strings.NewReader(string(changed)))
				code := "publication_operation_mismatch"
				if old.request.ScanID != scan.ScanID {
					code = "publication_scan_superseded"
				}
				expectCode(err, code)
			case 10:
				ctx, cancel := context.WithCancel(ht.Context())
				cancel()
				_, err := f.connection.CompleteNotePublicationScan(ctx, completeRequest(scan))
				require.Error(ht, err)
			}
			page, err := f.connection.NotePublicationSources(ht.Context(), api.NotePublicationSourcesRequest{Name: "research", ScanID: scan.ScanID, Limit: 1000})
			require.NoError(ht, err)
			require.Len(ht, page.Items, 1)
			current := page.Items[0]
			require.Equal(ht, nodeID, current.Identity.NodeID)
			require.Equal(ht, hash, current.Identity.Hash)
			require.Equal(ht, version, current.Identity.ContentVersionID)
			require.Equal(ht, revision, current.Identity.SourceRevision)
			wantPresence := "absent"
			if present {
				wantPresence = "present"
			}
			require.Equal(ht, wantPresence, current.Presence)
			binding = current.Identity
			status, err := f.connection.NotePublicationStatus(ht.Context(), "research")
			require.NoError(ht, err)
			require.Equal(ht, !sealed, status.Incomplete)
			require.GreaterOrEqual(ht, status.NamespaceRevision, namespace)
			namespace = status.NamespaceRevision
			versions, total, err := f.store.ContentVersions(ht.Context(), nodeID, 100, 0)
			require.NoError(ht, err)
			require.Equal(ht, len(hashes), total)
			actualHashes, wantHashes := map[string]int{}, map[string]int{}
			for _, v := range versions {
				actualHashes[v.BlobHash]++
				require.Equal(ht, ids[v.ID], v.BlobHash)
			}
			for _, h := range hashes {
				wantHashes[h]++
			}
			require.Equal(ht, wantHashes, actualHashes)
		}
	})
}
