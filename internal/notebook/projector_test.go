package notebook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/sqlite"
	"go.kenn.io/kit/packstore"
)

type projectorFixture struct {
	catalog *store.Store
	blobs   *blob.Store
	db      *sql.DB
}

type faultOriginalReader struct {
	originalReader

	kind           string
	cancel         context.CancelFunc
	opened, closed int
	readBytes      int64
}

func (r *faultOriginalReader) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	stream, size, err := r.originalReader.OpenStreamContext(ctx, hash)
	if err != nil {
		return stream, size, err
	}
	r.opened++
	wrapper := &faultOriginalStream{VerifiedReadCloser: stream, owner: r}
	switch r.kind {
	case "size", "canceled_size":
		size++
		if r.kind == "canceled_size" {
			r.cancel()
		}
	case "sha":
		wrapper.payload = strings.NewReader("evil")
	case "short":
		wrapper.payload = strings.NewReader("bod")
	case "long":
		wrapper.payload = strings.NewReader("bodyplus")
	case "bounded":
		wrapper.payload = io.LimitReader(projectorZeros{}, 8<<20)
	}
	return wrapper, size, nil
}

type projectorZeros struct{}

func (projectorZeros) Read(data []byte) (int, error) {
	clear(data)
	return len(data), nil
}

type faultOriginalStream struct {
	packstore.VerifiedReadCloser

	owner   *faultOriginalReader
	payload io.Reader
}

var errPrivateFixture = errors.New("synthetic-private-diagnostic")

func (r *faultOriginalStream) Read(data []byte) (int, error) {
	if r.owner.kind == "read" {
		return 0, errPrivateFixture
	}
	if r.owner.kind == "cancel" {
		r.owner.cancel()
		return 0, context.Canceled
	}
	var n int
	var err error
	if r.payload != nil {
		n, err = r.payload.Read(data)
	} else {
		n, err = r.VerifiedReadCloser.Read(data)
	}
	r.owner.readBytes += int64(n)
	return n, err
}

func (r *faultOriginalStream) Verify() error {
	if r.owner.kind == "verify" {
		return errPrivateFixture
	}
	if r.owner.kind == "bounded" {
		// Kit's Verify drains an unfinished stream. This fault therefore catches
		// validation that drains past LimitReader after already proving oversize.
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
	}
	if err := r.VerifiedReadCloser.Verify(); err != nil {
		return fmt.Errorf("verify synthetic stream: %w", err)
	}
	return nil
}

func (r *faultOriginalStream) Close() error {
	r.owner.closed++
	err := r.VerifiedReadCloser.Close()
	if r.owner.kind == "close" {
		return errors.Join(err, errPrivateFixture)
	}
	return err
}

// Catches trusting an advertised SHA, ignoring Verify/Close failures, saving
// operational diagnostics as content failures, and draining beyond admission.
func TestNoteProjectorOriginalFaults(t *testing.T) {
	for _, kind := range []string{"size", "sha", "short", "long", "bounded", "read", "verify", "close", "cancel", "canceled_size"} {
		t.Run(kind, func(t *testing.T) {
			f := newProjectorFixture(t)
			scan := f.begin(t)
			id := f.accept(t, scan, "note.md", "body", 0)
			f.complete(t, scan)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &faultOriginalReader{originalReader: f.blobs, kind: kind, cancel: cancel}
			p := NewProjector(f.catalog, f.blobs)
			p.blobs = reader
			err := p.Process(ctx, f.target(t))
			require.Error(t, err)
			if kind == "cancel" || kind == "canceled_size" {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.NotContains(t, err.Error(), errPrivateFixture.Error())
			_, found, err := f.catalog.NoteParseForVersion(t.Context(), id.ContentVersionID, notemarkdown.Recipe)
			require.NoError(t, err)
			require.False(t, found, "an operational failure remains retryable")
			require.Empty(t, f.currentHead(t))
			require.Equal(t, reader.opened, reader.closed, "every acquired stream is closed")
			require.LessOrEqual(t, reader.readBytes, int64((4<<20)+1), "verification cannot drain a known oversized stream")
			// Restored availability retries the same immutable accepted version.
			require.NoError(t, NewProjector(f.catalog, f.blobs).Process(t.Context(), f.target(t)))
			require.NotEmpty(t, f.currentHead(t))
		})
	}
}

func newProjectorFixture(t testing.TB) projectorFixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "catalog.db")
	catalog, err := store.Open(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	// This independent connection only inspects the synthetic derived records.
	// ReadOnlyImmutable would miss live WAL changes.
	db, err := store.DefaultSQLiteDriver().Open(dbPath, sqlite.OpenOptions{
		Access: sqlite.ReadWriteExisting, TransactionMode: sqlite.Deferred,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return projectorFixture{catalog, blobs, db}
}

func (f projectorFixture) begin(t testing.TB) store.NotePublicationScan {
	t.Helper()
	scan, err := f.catalog.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{
		Name: "notebook", Destination: "/notes", PolicyFingerprint: strings.Repeat("ab", 32), OperationID: uuid.NewString(),
	})
	require.NoError(t, err)
	return scan
}

func (f projectorFixture) accept(t testing.TB, scan store.NotePublicationScan, ref, body string, revision int64) store.PublishedNoteIdentity {
	t.Helper()
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewBufferString(body))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	accepted, err := f.catalog.AcceptPublishedNote(t.Context(), store.NotePublicationUploadRequest{
		Name: "notebook", ScanID: scan.ScanID, SourceRef: ref, FileName: path.Base(ref),
		ParentPath: path.Join("/notes", path.Dir(ref)), OperationID: uuid.NewString(),
		Hash: receipt.Hash, Size: receipt.Size, ExpectedSourceRevision: revision,
	}, store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize, MD5: receipt.MD5,
		Created: receipt.Created, PackEligible: receipt.PackEligible})
	require.NoError(t, err)
	return accepted.Source.Identity
}

func (f projectorFixture) complete(t testing.TB, scan store.NotePublicationScan) {
	t.Helper()
	_, err := f.catalog.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{
		Name: "notebook", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true,
	})
	require.NoError(t, err)
}

func (f projectorFixture) target(t testing.TB) store.NoteIndexTarget {
	t.Helper()
	status, err := f.catalog.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	return store.NoteIndexTarget{Name: status.Name, DatabaseEpoch: status.DatabaseEpoch, NamespaceRevision: status.NamespaceRevision}
}

func (f projectorFixture) edges(t testing.TB, ref string) []store.NoteIndexEdge {
	t.Helper()
	rows, err := f.db.QueryContext(t.Context(), `SELECT e.edge_json FROM note_link_edges e
 JOIN note_index_heads h ON h.generation_id=e.generation_id
 WHERE h.publication_name='notebook' AND e.source_ref=$1 ORDER BY e.ordinal`, ref)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var edges []store.NoteIndexEdge
	for rows.Next() {
		var data []byte
		require.NoError(t, rows.Scan(&data))
		var edge store.NoteIndexEdge
		require.NoError(t, json.Unmarshal(data, &edge))
		edges = append(edges, edge)
	}
	require.NoError(t, rows.Err())
	return edges
}

func (f projectorFixture) currentHead(t testing.TB) string {
	t.Helper()
	var id string
	err := f.db.QueryRowContext(t.Context(), `SELECT h.generation_id FROM note_index_heads h
 JOIN note_index_generations g ON g.generation_id=h.generation_id
 JOIN note_publications p ON p.name=h.publication_name AND p.namespace_revision=g.namespace_revision
 JOIN note_publication_state s ON s.singleton=1 AND s.database_epoch=g.database_epoch
 WHERE h.publication_name='notebook' AND p.active=1 AND g.completed=1`).Scan(&id)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return id
}

// Catches rendition parsing, normalized offsets, code-block links, stale target
// versions, and replacing a heading span with its larger section span.
func TestNoteProjectorOriginalBytes(t *testing.T) {
	t.Parallel()
	f := newProjectorFixture(t)
	scan := f.begin(t)
	const source = "\ufeff---\r\naliases: [Start]\r\ntags: [work]\r\n---\r\n# Café\r\n[[dir/target#section]]\r\n[again](dir/target.md#absent)\r\n`[[ignored]]`\r\n```md\r\n[[ignored-too]]\r\n```\r\n"
	sourceID := f.accept(t, scan, "source.md", source, 0)
	targetID := f.accept(t, scan, "dir/target.md", "# Section\r\nBody α\r\n## Child\r\nMore\r\n", 0)
	f.complete(t, scan)
	p := NewProjector(f.catalog, f.blobs)
	require.NoError(t, p.Process(t.Context(), f.target(t)))
	record, found, err := f.catalog.NoteParseForVersion(t.Context(), sourceID.ContentVersionID, notemarkdown.Recipe)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "ready", record.State)
	sum := sha256.Sum256([]byte(source))
	require.Equal(t, hex.EncodeToString(sum[:]), record.Hash)
	require.EqualValues(t, 153, record.Size)
	var note notemarkdown.Note
	require.NoError(t, json.Unmarshal(record.Artifact, &note))
	require.Equal(t, []string{"Start"}, note.Metadata.Aliases)
	require.Equal(t, []string{"work"}, note.Metadata.Tags)
	require.Len(t, note.Headings, 1)
	require.Equal(t, notemarkdown.Span{Start: 45, End: 54}, note.Headings[0].Span)
	status, err := f.catalog.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	targetID.NamespaceRevision = status.NamespaceRevision
	want := []store.NoteIndexEdge{
		{Ordinal: 0, Link: notemarkdown.Link{Span: notemarkdown.Span{Start: 54, End: 76}, DestinationSpan: notemarkdown.Span{Start: 56, End: 74},
			Form: "wiki", RawTarget: "dir/target#section", Target: "dir/target#section"}, State: "resolved", Target: &targetID, TargetHeading: &notemarkdown.Span{Start: 0, End: 11}},
		{Ordinal: 1, Link: notemarkdown.Link{Span: notemarkdown.Span{Start: 78, End: 107}, DestinationSpan: notemarkdown.Span{Start: 86, End: 106},
			Form: "markdown", RawTarget: "dir/target.md#absent", Target: "dir/target.md#absent"}, State: "missing", Target: &targetID},
	}
	require.Equal(t, want, f.edges(t, "source.md"))
	head := f.currentHead(t)
	require.NotEmpty(t, head)
	require.NoError(t, p.Process(t.Context(), f.target(t)))
	require.Equal(t, head, f.currentHead(t), "a quiet head is a no-op")
}

// Catches sealing only a page, inventing an empty basename for .md, or choosing
// one alias match when the bounded evidence represents three distinct notes.
func TestNoteProjectorEmptyAliasDoesNotBlockNamespace(t *testing.T) {
	t.Parallel()
	f := newProjectorFixture(t)
	scan := f.begin(t)
	f.accept(t, scan, "empty.md", "---\naliases: ['']\n---\n# Heading\n", 0)
	f.accept(t, scan, "link.md", "[[empty]]", 0)
	f.complete(t, scan)
	require.NoError(t, NewProjector(f.catalog, f.blobs).Process(t.Context(), f.target(t)))
	require.NotEmpty(t, f.currentHead(t))
	edges := f.edges(t, "link.md")
	require.Len(t, edges, 1)
	require.Equal(t, "resolved", edges[0].State)
}

func TestNoteProjectorOversizedLookupDoesNotBlockNamespace(t *testing.T) {
	t.Parallel()
	f := newProjectorFixture(t)
	scan := f.begin(t)
	f.accept(t, scan, "source.md", "[["+strings.Repeat("x", 4096)+"]] [[target]]", 0)
	f.accept(t, scan, "target.md", "# Target", 0)
	f.complete(t, scan)
	require.NoError(t, NewProjector(f.catalog, f.blobs).Process(t.Context(), f.target(t)))
	require.NotEmpty(t, f.currentHead(t))
	edges := f.edges(t, "source.md")
	require.Len(t, edges, 2)
	require.Equal(t, "unsupported", edges[0].State)
	require.Equal(t, "resolved", edges[1].State)
}

func TestNoteProjectorCompleteNamespace(t *testing.T) {
	t.Parallel()
	f := newProjectorFixture(t)
	scan := f.begin(t)
	f.accept(t, scan, "source.md", "[[shared]] [[.md]] [[absent]]", 0)
	for _, ref := range []string{"a/note.md", "b/note.md", "c/note.md"} {
		f.accept(t, scan, ref, "---\naliases: [shared]\n---\nbody", 0)
	}
	dot := f.accept(t, scan, ".md", "plain", 0)
	for i := range 65 {
		f.accept(t, scan, fmt.Sprintf("page/%03d.md", i), "plain", 0)
	}
	f.complete(t, scan)
	require.NoError(t, NewProjector(f.catalog, f.blobs).Process(t.Context(), f.target(t)))
	edges := f.edges(t, "source.md")
	require.Len(t, edges, 3)
	require.Equal(t, "ambiguous", edges[0].State)
	require.Nil(t, edges[0].Target)
	status, err := f.catalog.NotePublicationStatus(t.Context(), "notebook")
	require.NoError(t, err)
	dot.NamespaceRevision = status.NamespaceRevision
	require.Equal(t, "resolved", edges[1].State)
	require.Equal(t, &dot, edges[1].Target)
	require.Equal(t, "missing", edges[2].State)
	var count int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_lookup_keys k
 JOIN note_index_heads h ON h.generation_id=k.generation_id WHERE k.source_ref='.md'`).Scan(&count))
	require.Equal(t, 1, count, "extension-only source contributes its path key")
}

// Catches treating content limits as transient errors or saving operational
// failures as fixed content diagnoses; missing originals must remain pending.
func TestNoteProjectorLimitAndMissingBytes(t *testing.T) {
	for _, tc := range []struct {
		name, body, state, reason string
	}{
		{"input_limit", strings.Repeat("x", (4<<20)+1), "limit", "input_limit"},
		{"parser_limit", strings.Repeat("\n", 65537), "limit", "parser_limit"},
		{"invalid_markdown", "bad\x00input", "failed", "invalid_markdown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProjectorFixture(t)
			scan := f.begin(t)
			id := f.accept(t, scan, "note.md", tc.body, 0)
			f.complete(t, scan)
			require.NoError(t, NewProjector(f.catalog, f.blobs).Process(t.Context(), f.target(t)))
			record, found, err := f.catalog.NoteParseForVersion(t.Context(), id.ContentVersionID, notemarkdown.Recipe)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, tc.state, record.State)
			require.Equal(t, tc.reason, record.Reason)
			require.Empty(t, record.Artifact)
			require.Empty(t, f.edges(t, "note.md"))
			require.NotEmpty(t, f.currentHead(t))
		})
	}
	t.Run("missing_then_available", func(t *testing.T) {
		f := newProjectorFixture(t)
		scan := f.begin(t)
		id := f.accept(t, scan, "note.md", "body", 0)
		f.complete(t, scan)
		unavailable, err := blob.New(store.NewPackCatalog(f.catalog), filepath.Join(t.TempDir(), "missing"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, unavailable.Close()) })
		require.Error(t, NewProjector(f.catalog, unavailable).Process(t.Context(), f.target(t)))
		_, found, err := f.catalog.NoteParseForVersion(t.Context(), id.ContentVersionID, notemarkdown.Recipe)
		require.NoError(t, err)
		require.False(t, found, "unavailable bytes have no fixed content failure")
		require.Empty(t, f.currentHead(t))
		require.NoError(t, NewProjector(f.catalog, f.blobs).Process(t.Context(), f.target(t)))
		require.NotEmpty(t, f.currentHead(t))
	})
	t.Run("canceled", func(t *testing.T) {
		f := newProjectorFixture(t)
		scan := f.begin(t)
		id := f.accept(t, scan, "note.md", "body", 0)
		f.complete(t, scan)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, NewProjector(f.catalog, f.blobs).Process(ctx, f.target(t)), context.Canceled)
		_, found, err := f.catalog.NoteParseForVersion(t.Context(), id.ContentVersionID, notemarkdown.Recipe)
		require.NoError(t, err)
		require.False(t, found)
		require.Empty(t, f.currentHead(t))
	})
}

func (f projectorFixture) seedParse(t testing.TB, id store.PublishedNoteIdentity, body string) store.NoteParseRecord {
	t.Helper()
	// Parser output is only the durable precondition of an interrupted attempt.
	// Expected graph edges below are authored independently.
	note, err := notemarkdown.Parse(t.Context(), []byte(body))
	require.NoError(t, err)
	data, err := notemarkdown.CanonicalArtifact(t.Context(), note)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	record := store.NoteParseRecord{ContentVersionID: id.ContentVersionID, Hash: id.Hash, Size: id.Size,
		Recipe: notemarkdown.Recipe, State: "ready", Checksum: hex.EncodeToString(sum[:]), Artifact: data}
	require.NoError(t, f.catalog.RecordNoteParse(t.Context(), record))
	return record
}

// Catches trusting an abandoned stage, skipping cached sources on retry, or
// publishing an incomplete edge pass after interruption.
func TestNoteProjectorCrashRecovery(t *testing.T) {
	for _, point := range []string{"after_parse", "half_keys", "half_edges"} {
		t.Run(point, func(t *testing.T) {
			f := newProjectorFixture(t)
			scan := f.begin(t)
			a := f.accept(t, scan, "a.md", "[[b]]", 0)
			b := f.accept(t, scan, "b.md", "body", 0)
			f.complete(t, scan)
			ra := f.seedParse(t, a, "[[b]]")
			rb := f.seedParse(t, b, "body")
			target := f.target(t)
			a.NamespaceRevision, b.NamespaceRevision = target.NamespaceRevision, target.NamespaceRevision
			if point != "after_parse" {
				g, err := f.catalog.BeginNoteIndex(t.Context(), target, notemarkdown.Recipe, notemarkdown.ResolverRecipe)
				require.NoError(t, err)
				require.NoError(t, f.catalog.StageNoteIndexSource(t.Context(), g,
					store.NotePublicationSource{Identity: a}, ra, []store.NoteLookupKey{{Kind: "path", Value: "a.md"}, {Kind: "basename", Value: "a"}}))
				if point == "half_edges" {
					require.NoError(t, f.catalog.StageNoteIndexSource(t.Context(), g,
						store.NotePublicationSource{Identity: b}, rb, []store.NoteLookupKey{{Kind: "path", Value: "b.md"}, {Kind: "basename", Value: "b"}}))
					require.NoError(t, f.catalog.SealNoteIndexKeys(t.Context(), g))
					require.NoError(t, f.catalog.StageNoteIndexEdges(t.Context(), g, "b.md", nil))
				}
			}
			require.Empty(t, f.currentHead(t))
			require.NoError(t, NewProjector(f.catalog, f.blobs).Process(t.Context(), target))
			require.Equal(t, []store.NoteIndexEdge{{Ordinal: 0,
				Link:  notemarkdown.Link{Span: notemarkdown.Span{Start: 0, End: 5}, DestinationSpan: notemarkdown.Span{Start: 2, End: 3}, Form: "wiki", RawTarget: "b", Target: "b"},
				State: "resolved", Target: &b}}, f.edges(t, "a.md"))
			require.NotEmpty(t, f.currentHead(t))
			var generations int
			require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM note_index_generations`).Scan(&generations))
			require.Equal(t, 1, generations, "abandoned work is replaced, not published")
		})
	}
}

type racingNoteCatalog struct {
	noteCatalog

	point string
	fired bool
	hook  func()
}

func (s *racingNoteCatalog) change(point string) {
	if !s.fired && s.point == point {
		s.fired = true
		s.hook()
	}
}

func (s *racingNoteCatalog) RecordNoteParse(ctx context.Context, record store.NoteParseRecord) error {
	if err := s.noteCatalog.RecordNoteParse(ctx, record); err != nil {
		return err
	}
	s.change("parse")
	return nil
}

func (s *racingNoteCatalog) StageNoteIndexSource(ctx context.Context, g store.NoteIndexGeneration, source store.NotePublicationSource, record store.NoteParseRecord, keys []store.NoteLookupKey) error {
	if err := s.noteCatalog.StageNoteIndexSource(ctx, g, source, record, keys); err != nil {
		return err
	}
	s.change("keys")
	return nil
}

func (s *racingNoteCatalog) SealNoteIndexKeys(ctx context.Context, g store.NoteIndexGeneration) error {
	if err := s.noteCatalog.SealNoteIndexKeys(ctx, g); err != nil {
		return err
	}
	s.change("sealed")
	return nil
}

func (s *racingNoteCatalog) NoteIndexCandidates(ctx context.Context, g store.NoteIndexGeneration, keys []store.NoteLookupKey) ([]store.NoteLookupCandidate, error) {
	rows, err := s.noteCatalog.NoteIndexCandidates(ctx, g, keys)
	if err == nil {
		s.change("lookup")
	}
	return rows, err
}

func (s *racingNoteCatalog) StageNoteIndexEdges(ctx context.Context, g store.NoteIndexGeneration, ref string, edges []store.NoteIndexEdge) error {
	if err := s.noteCatalog.StageNoteIndexEdges(ctx, g, ref, edges); err != nil {
		return err
	}
	s.change("edges")
	return nil
}

func (s *racingNoteCatalog) PublishNoteIndex(ctx context.Context, g store.NoteIndexGeneration) error {
	s.change("publish")
	return s.noteCatalog.PublishNoteIndex(ctx, g)
}

// Catches dropping the generation fence between keys, lookup, target evidence,
// and publication, including restoration to identical logical source versions.
func TestNoteProjectorStagingRace(t *testing.T) {
	for _, point := range []string{"keys", "sealed", "lookup", "edges", "publish"} {
		for _, change := range []string{"edit", "absence", "move", "restore"} {
			t.Run(point+"/"+change, func(t *testing.T) {
				f := newProjectorFixture(t)
				scan := f.begin(t)
				a := f.accept(t, scan, "a.md", "[[b]]", 0)
				b := f.accept(t, scan, "b.md", "# Section\n", 0)
				f.complete(t, scan)
				active := f
				racing := &racingNoteCatalog{noteCatalog: f.catalog, point: point}
				racing.hook = func() {
					switch change {
					case "edit":
						scan := f.begin(t)
						f.accept(t, scan, "a.md", "replacement", a.SourceRevision)
						f.accept(t, scan, "b.md", "# Section\n", b.SourceRevision)
						f.complete(t, scan)
					case "absence":
						f.complete(t, f.begin(t))
					case "move":
						node, err := f.catalog.NodeByID(t.Context(), b.NodeID)
						require.NoError(t, err)
						_, _, err = f.catalog.Move(t.Context(), node.ID, f.catalog.RootID(), "archive-renamed.md", node.Revision)
						require.NoError(t, err)
					case "restore":
						var snapshot bytes.Buffer
						require.NoError(t, f.catalog.ExportMetadata(t.Context(), &snapshot))
						active = newProjectorFixture(t)
						require.NoError(t, active.catalog.ImportMetadata(t.Context(), &snapshot))
						racing.noteCatalog = active.catalog
					}
				}
				p := NewProjector(f.catalog, f.blobs)
				p.catalog = racing
				require.ErrorIs(t, p.Process(t.Context(), f.target(t)), store.ErrNoteIndexStale)
				require.True(t, racing.fired, "the authored race point was exercised")
				require.Empty(t, active.currentHead(t))
				if change == "restore" {
					// Restores intentionally need a fresh full inventory before notes
					// become eligible. The original source revisions are retained.
					scan := active.begin(t)
					active.accept(t, scan, "a.md", "[[b]]", a.SourceRevision)
					active.accept(t, scan, "b.md", "# Section\n", b.SourceRevision)
					active.complete(t, scan)
					p = NewProjector(active.catalog, active.blobs)
				}
				require.NoError(t, p.Process(t.Context(), active.target(t)))
				require.NotEmpty(t, active.currentHead(t))
				if change == "edit" || change == "absence" {
					require.Empty(t, active.edges(t, "a.md"), "old authored edges cannot survive source replacement or absence")
				} else {
					edges := active.edges(t, "a.md")
					require.Len(t, edges, 1)
					require.Equal(t, "b.md", edges[0].Target.SourceRef)
					require.Equal(t, b.ContentVersionID, edges[0].Target.ContentVersionID)
				}
			})
		}
	}
}

func TestNoteProjectorRestoreDuringParse(t *testing.T) {
	f := newProjectorFixture(t)
	scan := f.begin(t)
	f.accept(t, scan, "note.md", "body", 0)
	f.complete(t, scan)
	var snapshot bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &snapshot))
	restored := newProjectorFixture(t)
	require.NoError(t, restored.catalog.ImportMetadata(t.Context(), &snapshot))
	racing := &racingNoteCatalog{noteCatalog: f.catalog, point: "parse"}
	racing.hook = func() { racing.noteCatalog = restored.catalog }
	p := NewProjector(f.catalog, f.blobs)
	p.catalog = racing
	require.ErrorIs(t, p.Process(t.Context(), f.target(t)), store.ErrNoteIndexStale)
	require.True(t, racing.fired)
	require.Empty(t, f.currentHead(t))
	require.Empty(t, restored.currentHead(t))
}

func TestNoteProjectorBackfillDrains(t *testing.T) {
	t.Parallel()
	f := newProjectorFixture(t)
	scan := f.begin(t)
	f.accept(t, scan, "a.md", "[[b]]", 0)
	b := f.accept(t, scan, "b.md", "body", 0)
	f.complete(t, scan)
	mutations := 0
	backfill := NewBackfill(NewProjector(f.catalog, f.blobs), func(ctx context.Context, fn func() error) error {
		require.NoError(t, ctx.Err())
		mutations++
		return fn()
	}, slog.New(slog.DiscardHandler))
	require.NotNil(t, backfill)
	backfill.DrainOnce = true
	require.NoError(t, backfill.Run(t.Context()))
	require.Positive(t, mutations, "processing must pass through the supplied mutation gate")
	head := f.currentHead(t)
	require.NotEmpty(t, head)
	edges := f.edges(t, "a.md")
	require.Len(t, edges, 1)
	require.Equal(t, "resolved", edges[0].State)
	require.Equal(t, b.ContentVersionID, edges[0].Target.ContentVersionID)
	require.Equal(t, "b.md", edges[0].Target.SourceRef)
	before := mutations
	require.NoError(t, backfill.Run(t.Context()))
	require.Equal(t, before, mutations, "a current namespace has no recovery work")
	require.Equal(t, head, f.currentHead(t))
}

func TestNoteProjectorBackfillSupervisorShutdown(t *testing.T) {
	t.Parallel()
	f := newProjectorFixture(t)
	scan := f.begin(t)
	f.accept(t, scan, "note.md", "body", 0)
	f.complete(t, scan)
	backfill := NewBackfill(NewProjector(f.catalog, f.blobs), nil, slog.New(slog.DiscardHandler))
	require.NotNil(t, backfill)
	list := backfill.List
	waiting := make(chan struct{})
	backfill.List = func(ctx context.Context, after string, limit int) ([]store.NoteIndexTarget, error) {
		targets, err := list(ctx, after, limit)
		if err == nil && len(targets) == 0 {
			// Drive shutdown directly; no real-clock sleep or polling.
			close(waiting)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return targets, err
	}
	supervisor := jobs.New(t.Context(), slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		// This cleanup must drain the worker after the test context ends and
		// before the earlier fixture cleanups close its catalog and blobs.
		require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context())))
	})
	require.NoError(t, supervisor.StartWorker("derive:notes", backfill.Run))
	select {
	case <-waiting:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	require.NotEmpty(t, f.currentHead(t))
	require.NoError(t, supervisor.Shutdown(t.Context()))
	snapshots := supervisor.Snapshot()
	require.Len(t, snapshots, 1)
	require.Equal(t, jobs.StatusCancelled, snapshots[0].Status)
}
