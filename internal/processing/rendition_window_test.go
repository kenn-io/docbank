package processing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestReadUnicodeRenditionWindowUsesCodePointOffsets(t *testing.T) {
	text, actualEnd, eof, err := readUnicodeRenditionWindow(t.Context(), strings.NewReader("aé界🙂z"), 1, 3)
	require.NoError(t, err)
	assert.Equal(t, "é界🙂", text)
	assert.Equal(t, 4, actualEnd)
	assert.False(t, eof)

	text, actualEnd, eof, err = readUnicodeRenditionWindow(t.Context(), strings.NewReader("aé界🙂z"), 4, 3)
	require.NoError(t, err)
	assert.Equal(t, "z", text)
	assert.Equal(t, 5, actualEnd)
	assert.True(t, eof)

	text, actualEnd, eof, err = readUnicodeRenditionWindow(t.Context(), strings.NewReader("aé界🙂z"), 5, 3)
	require.NoError(t, err)
	assert.Empty(t, text)
	assert.Equal(t, 5, actualEnd)
	assert.True(t, eof)
}

func TestReadUnicodeRenditionWindowFailsClosed(t *testing.T) {
	text, actualEnd, eof, err := readUnicodeRenditionWindow(t.Context(), strings.NewReader("short"), 6, 1)
	require.Empty(t, text)
	require.Zero(t, actualEnd)
	require.False(t, eof)
	require.ErrorIs(t, err, ErrInvalidRenditionWindow)

	text, actualEnd, eof, err = readUnicodeRenditionWindow(t.Context(), strings.NewReader(string([]byte{'a', 0xff, 'b'})), 0, 3)
	require.Empty(t, text)
	require.Zero(t, actualEnd)
	require.False(t, eof)
	require.ErrorIs(t, err, ErrInvalidRenditionEncoding)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	text, actualEnd, eof, err = readUnicodeRenditionWindow(canceled, strings.NewReader("text"), 0, 1)
	require.Empty(t, text)
	require.Zero(t, actualEnd)
	require.False(t, eof)
	require.ErrorIs(t, err, context.Canceled)
}

func TestReadUnicodeRenditionWindowNeverRequestsAnUnboundedBuffer(t *testing.T) {
	reader := &maximumReadReader{reader: strings.NewReader(strings.Repeat("x", 100_000))}
	text, _, eof, err := readUnicodeRenditionWindow(t.Context(), reader, 80_000, 16_000)
	require.NoError(t, err)
	assert.Len(t, text, 16_000)
	assert.False(t, eof)
	assert.LessOrEqual(t, reader.maximum, 4096)
}

func TestReadRenditionBlobWindowStreamsOnlyTheRequestedPrefix(t *testing.T) {
	stream := &instrumentedVerifiedStream{reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	source := instrumentedVerifiedSource{stream: stream, size: 1 << 20}

	text, actualEnd, eof, err := readRenditionBlobWindow(
		t.Context(), source, strings.Repeat("a", 64), 1<<20, 0, 1,
	)
	require.NoError(t, err)
	assert.Equal(t, "x", text)
	assert.Equal(t, 1, actualEnd)
	assert.False(t, eof)
	assert.LessOrEqual(t, stream.bytesRead, 4096,
		"a tiny window must not materialize or verify the complete artifact")
	assert.Zero(t, stream.verifyCalls)
	assert.True(t, stream.closed)
}

func TestReadRenditionBlobWindowPreservesCancellationAndCleanupErrors(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	stream := &instrumentedVerifiedStream{reader: strings.NewReader("synthetic")}
	text, actualEnd, eof, err := readRenditionBlobWindow(canceled,
		instrumentedVerifiedSource{stream: stream, size: 9}, strings.Repeat("a", 64), 9, 0, 1)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, text)
	assert.Zero(t, actualEnd)
	assert.False(t, eof)
	assert.True(t, stream.closed)

	cleanupErr := errors.New("synthetic stream cleanup failed")
	stream = &instrumentedVerifiedStream{reader: strings.NewReader("synthetic"), closeErr: cleanupErr}
	text, actualEnd, eof, err = readRenditionBlobWindow(t.Context(),
		instrumentedVerifiedSource{stream: stream, size: 9}, strings.Repeat("a", 64), 9, 0, 1)
	require.ErrorIs(t, err, cleanupErr)
	assert.Empty(t, text)
	assert.Zero(t, actualEnd)
	assert.False(t, eof)
	assert.NotErrorIs(t, err, pack.ErrVerificationIncomplete)
}

func TestRenditionTextWindowBindsTheCurrentLiveTuple(t *testing.T) {
	fixture := newPublicationFixture(t)
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	published, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"window-build", "window-attachment", "window-generation"},
		"alpha écho 界🙂 omega", "Synthetic heading",
	))
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
	require.NoError(t, err)

	full, err := service.RenditionByAttachment(t.Context(), published.AttachmentID, 1<<20)
	require.NoError(t, err)
	var body bytes.Buffer
	_, err = io.Copy(&body, full.Reader)
	require.NoError(t, err)
	require.NoError(t, full.Reader.Verify())
	require.NoError(t, full.Reader.Close())
	runes := []rune(body.String())

	window, err := service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
		VaultUID: fixture.catalog.VaultID(), NodeID: full.NodeID,
		ContentVersionID: fixture.versionID, AttachmentID: published.AttachmentID,
		Offset: 3, MaxChars: 11,
	})
	require.NoError(t, err)
	assert.Equal(t, string(runes[3:14]), window.Text)
	assert.Equal(t, 3, window.RequestedOffset)
	assert.Equal(t, 3, window.ActualStart)
	assert.Equal(t, 14, window.ActualEnd)
	assert.Equal(t, 14, window.NextOffset)
	assert.False(t, window.EOF)
	assert.Equal(t, fixture.catalog.VaultID(), window.VaultUID)
	assert.Equal(t, published.AttachmentID, window.AttachmentID)
	assert.Equal(t, published.BuildID, window.BuildID)

	for name, mutate := range map[string]func(*RenditionWindowRequest){
		"foreign vault": func(request *RenditionWindowRequest) {
			request.VaultUID = "11111111-1111-4111-8111-111111111111"
		},
		"wrong node": func(request *RenditionWindowRequest) { request.NodeID++ },
		"wrong version": func(request *RenditionWindowRequest) {
			request.ContentVersionID = "22222222-2222-4222-8222-222222222222"
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := RenditionWindowRequest{VaultUID: fixture.catalog.VaultID(), NodeID: full.NodeID,
				ContentVersionID: fixture.versionID, AttachmentID: published.AttachmentID,
				MaxChars: 1}
			mutate(&request)
			_, err := service.RenditionTextWindow(t.Context(), request)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}

	node, err := fixture.catalog.NodeByID(t.Context(), full.NodeID)
	require.NoError(t, err)
	_, _, err = fixture.catalog.Trash(t.Context(), node.ID, node.Revision)
	require.NoError(t, err)
	_, err = service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
		VaultUID: fixture.catalog.VaultID(), NodeID: full.NodeID,
		ContentVersionID: fixture.versionID, AttachmentID: published.AttachmentID, MaxChars: 1,
	})
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestRenditionTextWindowRejectsInactiveAndSupersededAuthority(t *testing.T) {
	newPublishedWindow := func(t *testing.T) (publicationFixture, *Service, RenditionTextWindow, string) {
		t.Helper()
		fixture := newPublicationFixture(t)
		publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
		require.NoError(t, err)
		published, err := publisher.PublishRendition(t.Context(), fixture.stage(t,
			publicationIDs{"authority-build", "authority-attachment", "authority-generation"},
			"synthetic authority", "Synthetic authority",
		))
		require.NoError(t, err)
		service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
			Gate: newWorkerTestGate(), SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
		require.NoError(t, err)
		node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
		require.NoError(t, err)
		window, err := service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
			VaultUID: fixture.catalog.VaultID(), NodeID: node.ID,
			ContentVersionID: fixture.versionID, AttachmentID: published.AttachmentID, MaxChars: 1,
		})
		require.NoError(t, err)
		return fixture, service, window, published.AttachmentID
	}

	t.Run("inactive attachment", func(t *testing.T) {
		fixture, service, baseline, oldAttachmentID := newPublishedWindow(t)
		_, err := service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
			VaultUID: fixture.catalog.VaultID(), NodeID: baseline.NodeID,
			ContentVersionID: fixture.versionID, AttachmentID: processingHash("inactive-attachment"), MaxChars: 1,
		})
		require.ErrorIs(t, err, store.ErrNotFound)
		_, err = service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
			VaultUID: fixture.catalog.VaultID(), NodeID: baseline.NodeID,
			ContentVersionID: fixture.versionID, AttachmentID: oldAttachmentID, MaxChars: 1,
		})
		require.NoError(t, err)
	})

	t.Run("superseded source", func(t *testing.T) {
		fixture, service, baseline, attachmentID := newPublishedWindow(t)
		replacement := []byte("synthetic replacement source")
		receipt, err := fixture.blobs.WriteDetailedContext(t.Context(), strings.NewReader(string(replacement)))
		require.NoError(t, err)
		node, err := fixture.catalog.NodeByID(t.Context(), baseline.NodeID)
		require.NoError(t, err)
		_, _, err = fixture.catalog.ReplaceContent(t.Context(), node.ID, node.Revision,
			receipt.Hash, receipt.Size, "application/pdf")
		require.NoError(t, err)
		_, err = service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
			VaultUID: fixture.catalog.VaultID(), NodeID: node.ID,
			ContentVersionID: fixture.versionID, AttachmentID: attachmentID, MaxChars: 1,
		})
		require.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestRenditionTextWindowAcceptsAnEmptyActiveMarkdownArtifact(t *testing.T) {
	fixture := newPublicationFixture(t)
	staged := fixture.stage(t,
		publicationIDs{"empty-build", "empty-attachment", "empty-generation"},
		"synthetic searchable evidence", "Synthetic heading",
	)
	empty, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(nil))
	require.NoError(t, err)
	require.NoError(t, fixture.catalog.RecordRenditionBlob(t.Context(), empty.Hash, empty.Size,
		processingBlobPhysical(t, empty)))
	for index := range staged.Build.Artifacts {
		artifact := &staged.Build.Artifacts[index]
		if artifact.Role != "sanitized_markdown" {
			continue
		}
		artifact.BlobHash = empty.Hash
		artifact.Checksum = empty.Hash
		artifact.Size = 0
		staged.Build.MarkdownChecksum = empty.Hash
	}
	for index := range staged.Artifacts {
		if staged.Artifacts[index].ID == staged.Build.Artifacts[1].ID {
			staged.Artifacts[index].Payload = bytes.NewReader(nil)
		}
	}
	for index, artifact := range staged.Build.Artifacts {
		if artifact.Role == "sanitized_markdown" {
			continue
		}
		payload := staged.Artifacts[index].Payload
		receipt, writeErr := fixture.blobs.WriteDetailedContext(t.Context(), payload)
		require.NoError(t, writeErr)
		require.Equal(t, artifact.BlobHash, receipt.Hash)
		require.NoError(t, fixture.catalog.RecordRenditionBlob(t.Context(), receipt.Hash, receipt.Size,
			processingBlobPhysical(t, receipt)))
	}
	require.NoError(t, fixture.catalog.StageRenditionBuild(t.Context(), staged.Build))
	generation, err := fixture.catalog.StageLexicalGeneration(t.Context(), staged.LexicalGenerationID)
	require.NoError(t, err)
	require.NoError(t, fixture.catalog.PublishRenditionAndLexicalHeads(
		t.Context(), staged.Attachment, staged.Head, generation.ID,
	))
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
	require.NoError(t, err)
	node, err := fixture.catalog.NodeByPath(t.Context(), "/source.pdf")
	require.NoError(t, err)

	window, err := service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
		VaultUID: fixture.catalog.VaultID(), NodeID: node.ID, ContentVersionID: fixture.versionID,
		AttachmentID: staged.Attachment.ID, MaxChars: 1,
	})
	require.NoError(t, err)
	assert.Empty(t, window.Text)
	assert.Zero(t, window.ActualEnd)
	assert.Zero(t, window.NextOffset)
	assert.True(t, window.EOF)
	assert.Zero(t, window.ResponseBytes)

	_, err = service.RenditionTextWindow(t.Context(), RenditionWindowRequest{
		VaultUID: fixture.catalog.VaultID(), NodeID: node.ID, ContentVersionID: fixture.versionID,
		AttachmentID: staged.Attachment.ID, Offset: 1, MaxChars: 1,
	})
	require.ErrorIs(t, err, ErrInvalidRenditionWindow)
}

type maximumReadReader struct {
	reader  io.Reader
	maximum int
}

func (reader *maximumReadReader) Read(p []byte) (int, error) {
	reader.maximum = max(reader.maximum, len(p))
	return reader.reader.Read(p)
}

func (reader *maximumReadReader) Close() error { return nil }

var _ io.ReadCloser = (*maximumReadReader)(nil)

type instrumentedVerifiedSource struct {
	stream *instrumentedVerifiedStream
	size   int64
}

func (source instrumentedVerifiedSource) OpenStreamContext(
	context.Context, string,
) (packstore.VerifiedReadCloser, int64, error) {
	return source.stream, source.size, nil
}

type instrumentedVerifiedStream struct {
	reader      *strings.Reader
	bytesRead   int
	verifyCalls int
	closed      bool
	verified    bool
	closeErr    error
}

func (stream *instrumentedVerifiedStream) Read(buffer []byte) (int, error) {
	read, err := stream.reader.Read(buffer)
	stream.bytesRead += read
	if errors.Is(err, io.EOF) {
		stream.verified = true
	}
	if err != nil {
		return read, fmt.Errorf("reading instrumented rendition stream: %w", err)
	}
	return read, nil
}

func (stream *instrumentedVerifiedStream) Verify() error {
	stream.verifyCalls++
	return nil
}

func (stream *instrumentedVerifiedStream) Verified() bool { return stream.verified }

func (stream *instrumentedVerifiedStream) Close() error {
	stream.closed = true
	if !stream.verified {
		return errors.Join(pack.ErrVerificationIncomplete, stream.closeErr)
	}
	return stream.closeErr
}

var _ packstore.VerifiedReadCloser = (*instrumentedVerifiedStream)(nil)
