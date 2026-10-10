package processing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func writeCitationBlob(t *testing.T, f publicationFixture, text string) (document.TextCitation, int64) {
	t.Helper()
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader(text))
	require.NoError(t, err)
	require.NoError(t, f.catalog.RecordRenditionBlob(t.Context(), receipt.Hash, receipt.Size,
		processingBlobPhysical(t, receipt)))
	return document.TextCitation{RenditionSHA256: receipt.Hash, End: 1}, receipt.Size
}

func TestResolveTextCitationExactText(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	for _, test := range []struct {
		text       string
		start, end int
		want       string
	}{
		{"aé界🙂z", 1, 4, "é界🙂"}, {"aé界🙂z", 4, 5, "z"},
		{"a\r\nb", 1, 3, "\r\n"}, {"ae\u0301b", 1, 3, "e\u0301"},
		{"2024\\-05\\-06", 4, 6, "\\-"}, {"a\ufffdb", 1, 2, "\ufffd"},
	} {
		t.Run(test.want, func(t *testing.T) {
			citation, size := writeCitationBlob(t, f, test.text)
			citation.Start, citation.End = test.start, test.end
			got, err := readTextCitation(t.Context(), f.blobs, citation, size)
			require.NoError(t, err)
			require.Equal(t, test.want, got.Text)
			require.Equal(t, len(test.want), got.TextBytes)
			require.Equal(t, citation, got.Citation)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(test.want))), got.TextSHA256)
			got, err = readTextCitation(t.Context(), citationReadBoundary{source: f.blobs,
				wrap: func(r io.Reader) io.Reader { return iotest.OneByteReader(r) }}, citation, size)
			require.NoError(t, err)
			require.Equal(t, test.want, got.Text)
		})
	}
	citation, size := writeCitationBlob(t, f, "short")
	citation.End = 6
	got, err := readTextCitation(t.Context(), f.blobs, citation, size)
	require.ErrorIs(t, err, document.ErrInvalidCitationRange)
	require.Empty(t, got.Text)
}

func TestResolveTextCitationVerification(t *testing.T) {
	t.Parallel()
	t.Run("corrupt suffix", func(t *testing.T) {
		f := newPublicationFixture(t)
		citation, size := writeCitationBlob(t, f, "quote unchanged; original suffix")
		layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(f.databasePath), "blobs"),
			packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"})
		require.NoError(t, err)
		hash, err := packstore.ParseHash(citation.RenditionSHA256)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(layout.LoosePath(hash),
			[]byte("quote unchanged; tampered suffix"), 0o600))
		got, err := readTextCitation(t.Context(), f.blobs, citation, size)
		require.ErrorIs(t, err, document.ErrCitationIntegrity)
		require.Empty(t, got.Text)
	})
	t.Run("invalid suffix encoding", func(t *testing.T) {
		f := newPublicationFixture(t)
		citation, size := writeCitationBlob(t, f, "quote\xff")
		got, err := readTextCitation(t.Context(), f.blobs, citation, size)
		require.ErrorIs(t, err, document.ErrCitationIntegrity)
		require.Empty(t, got.Text)
	})
	t.Run("size mismatch", func(t *testing.T) {
		f := newPublicationFixture(t)
		citation, size := writeCitationBlob(t, f, "quote")
		_, err := readTextCitation(t.Context(), f.blobs, citation, size+1)
		require.ErrorIs(t, err, document.ErrCitationIntegrity)
	})
	t.Run("limits", func(t *testing.T) {
		f := newPublicationFixture(t)
		citation, size := writeCitationBlob(t, f, strings.Repeat("x", int(MaxRenditionBytes)))
		got, err := readTextCitation(t.Context(), f.blobs, citation, size)
		require.NoError(t, err)
		require.Equal(t, "x", got.Text)
		_, err = readTextCitation(t.Context(), nil, citation, size+1)
		require.ErrorIs(t, err, document.ErrCitationLimit, "oversize is refused before opening")
		// A lying stream length cannot turn a bounded read into an unbounded one.
		stream := &instrumentedVerifiedStream{
			reader: strings.NewReader(strings.Repeat("x", int(size+8))),
		}
		_, err = readTextCitation(t.Context(),
			instrumentedVerifiedSource{stream: stream, size: size}, citation, size)
		require.ErrorIs(t, err, document.ErrCitationLimit)
		require.LessOrEqual(t, stream.bytesRead, int(size+1))
	})
}

func TestResolveTextCitationReadFailures(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	citation, size := writeCitationBlob(t, f, "quote")
	for _, cause := range []error{io.ErrUnexpectedEOF, io.ErrClosedPipe} {
		got, err := readTextCitation(t.Context(), citationReadBoundary{source: f.blobs,
			wrap: func(io.Reader) io.Reader { return iotest.ErrReader(cause) }}, citation, size)
		require.ErrorIs(t, err, cause)
		require.NotErrorIs(t, err, document.ErrCitationIntegrity)
		require.Empty(t, got.Text)
	}
	got, err := readTextCitation(t.Context(), citationReadBoundary{
		source: f.blobs, closeErr: io.ErrClosedPipe,
	}, citation, size)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	require.Empty(t, got.Text)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = readTextCitation(ctx, f.blobs, citation, size)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, f.blobs.Remove(citation.RenditionSHA256))
	_, err = readTextCitation(t.Context(), f.blobs, citation, size)
	require.ErrorIs(t, err, document.ErrCitationUnavailable)
}

// This wraps only external I/O; the catalog, hashing stream and resolver remain real.
type citationReadBoundary struct {
	source   verifiedBlobReader
	wrap     func(io.Reader) io.Reader
	closeErr error
}

func (b citationReadBoundary) OpenStreamContext(
	ctx context.Context, hash string,
) (packstore.VerifiedReadCloser, int64, error) {
	r, size, err := b.source.OpenStreamContext(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	var reader io.Reader = r
	if b.wrap != nil {
		reader = b.wrap(r)
	}
	return &citationBoundaryStream{VerifiedReadCloser: r, reader: reader, closeErr: b.closeErr}, size, nil
}

type citationBoundaryStream struct {
	packstore.VerifiedReadCloser
	reader   io.Reader
	closeErr error
}

func (r *citationBoundaryStream) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r *citationBoundaryStream) Close() error {
	return errors.Join(r.VerifiedReadCloser.Close(), r.closeErr)
}

// TextCitationTestFixture crosses the external backup test's import boundary only.
type TextCitationTestFixture struct {
	Catalog  *store.Store
	Blobs    *blob.Store
	Service  *Service
	Citation document.TextCitation
	fixture  publicationFixture
}

func NewTextCitationTestFixture(t *testing.T) TextCitationTestFixture {
	t.Helper()
	f := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(f.catalog, f.blobs)
	require.NoError(t, err)
	staged := f.stage(t, publicationIDs{"citation-build", "citation-attachment", "citation-generation"},
		"aé界🙂z", "Synthetic quote")
	_, err = publisher.PublishRendition(t.Context(), staged)
	require.NoError(t, err)
	r, _, err := f.blobs.OpenStreamContext(t.Context(), staged.Build.MarkdownChecksum)
	require.NoError(t, err)
	text, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	prefix, _, found := strings.Cut(string(text), "é界🙂")
	require.True(t, found)
	vaultID, err := uuid.Parse(f.catalog.VaultID())
	require.NoError(t, err)
	version, err := f.catalog.ContentVersionByID(t.Context(), f.versionID)
	require.NoError(t, err)
	versionID, err := uuid.Parse(f.versionID)
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs,
		Gate: newTestOperationGate(), SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
	require.NoError(t, err)
	start := utf8.RuneCountInString(prefix)
	return TextCitationTestFixture{Catalog: f.catalog, Blobs: f.blobs, Service: service, fixture: f,
		Citation: document.TextCitation{Version: 1, VaultUID: vaultID, NodeID: version.NodeID,
			ContentVersionID: versionID, ContentSHA256: version.BlobHash,
			RenditionAttachmentID: staged.Attachment.ID, BuildID: staged.Build.ID,
			RenditionSHA256: staged.Build.MarkdownChecksum, Start: start, End: start + 3}}
}

func (f TextCitationTestFixture) ReplaceSource(t *testing.T) {
	t.Helper()
	receipt, err := f.Blobs.WriteDetailedContext(t.Context(), strings.NewReader("new original bytes"))
	require.NoError(t, err)
	_, _, err = f.Catalog.ReplaceContent(t.Context(), f.Citation.NodeID, store.UnconditionalRev,
		receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
}

func TestResolveTextCitationLifecycle(t *testing.T) {
	t.Parallel()
	for _, removal := range []string{"trash", "prune", "purge"} {
		t.Run(removal, func(t *testing.T) {
			f := NewTextCitationTestFixture(t)
			old, err := f.Service.ResolveTextCitation(t.Context(), f.Citation)
			require.NoError(t, err)
			require.Equal(t, "é界🙂", old.Text)
			publisher, err := NewArtifactPublisher(f.Catalog, f.Blobs)
			require.NoError(t, err)
			_, err = publisher.PublishRendition(t.Context(), f.fixture.stage(t,
				publicationIDs{"next-citation-build", "next-citation-attachment", "next-citation-generation"},
				"replacement rendition", "Other quote"))
			require.NoError(t, err)
			f.ReplaceSource(t)
			got, err := f.Service.ResolveTextCitation(t.Context(), f.Citation)
			require.NoError(t, err)
			require.Equal(t, old, got)
			switch removal {
			case "trash":
				_, _, err = f.Catalog.Trash(t.Context(), f.Citation.NodeID, store.UnconditionalRev)
			case "prune":
				_, err = f.Catalog.PruneContentVersions(t.Context(), f.Citation.NodeID, store.UnconditionalRev,
					store.VersionPruneSelector{VersionIDs: []string{f.Citation.ContentVersionID.String()}}, true)
			case "purge":
				_, err = f.Catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{
					AttachmentIDs: []string{f.Citation.RenditionAttachmentID}})
			}
			require.NoError(t, err)
			_, err = f.Service.ResolveTextCitation(t.Context(), f.Citation)
			require.ErrorIs(t, err, document.ErrCitationUnavailable)
			if removal == "trash" {
				_, _, err = f.Catalog.Restore(t.Context(), f.Citation.NodeID, store.UnconditionalRev)
				require.NoError(t, err)
				got, err = f.Service.ResolveTextCitation(t.Context(), f.Citation)
				require.NoError(t, err)
				require.Equal(t, old, got)
			}
		})
	}
}
