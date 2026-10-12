package processing

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestTextNavigationService(t *testing.T) {
	t.Parallel()
	f := NewTextCitationTestFixture(t)
	outline, err := f.Service.GetTextOutline(t.Context(),
		document.TextOutlineRequest{Citation: f.Citation})
	require.NoError(t, err)
	request := document.TextSectionRequest{Citation: f.Citation,
		Section: outline.EnclosingSection, MaxChars: 16000}
	before, err := f.Service.ReadTextSection(t.Context(), request)
	require.NoError(t, err)
	require.True(t, before.Complete)
	require.Contains(t, before.Text, "aé界🙂z")
	derived := f.Citation
	derived.Start = before.ActualStart
	derived.End = before.ActualEnd
	quote, err := f.Service.ResolveTextCitation(t.Context(), derived)
	require.NoError(t, err)
	require.Equal(t, before.Text, quote.Text)
	require.Equal(t, before.TextSHA256, quote.TextSHA256)
	publisher, err := NewArtifactPublisher(f.Catalog, f.Blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), f.fixture.stage(t,
		publicationIDs{"navigation-next-build", "navigation-next-attachment", "navigation-next-index"},
		"replacement rendition", "Other quote"))
	require.NoError(t, err)
	f.ReplaceSource(t)
	after, err := f.Service.ReadTextSection(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestTextNavigationVerification(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	citation, size := writeCitationBlob(t, f, "quote; original suffix")
	for _, tc := range []struct {
		name   string
		source verifiedBlobReader
		cause  error
	}{
		{"unverified", citationReadBoundary{source: f.blobs, unverified: true},
			pack.ErrVerificationIncomplete},
		{"dropped", citationReadBoundary{source: f.blobs, wrap: func(io.Reader) io.Reader {
			return iotest.ErrReader(io.ErrUnexpectedEOF)
		}}, io.ErrUnexpectedEOF},
		{"close", citationReadBoundary{source: f.blobs, closeErr: io.ErrClosedPipe}, io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readTextNavigationBlob(t.Context(), tc.source, citation, size)
			require.ErrorIs(t, err, tc.cause)
			require.Empty(t, got)
			if tc.name == "unverified" {
				require.ErrorIs(t, err, document.ErrCitationIntegrity)
			} else {
				require.NotErrorIs(t, err, document.ErrCitationIntegrity)
			}
		})
	}
	_, err := readTextNavigationBlob(t.Context(), f.blobs, citation, size+1)
	require.ErrorIs(t, err, document.ErrCitationIntegrity)
	bad, badSize := writeCitationBlob(t, f, "quote\xff")
	_, err = readTextNavigationBlob(t.Context(), f.blobs, bad, badSize)
	require.ErrorIs(t, err, document.ErrCitationIntegrity)
	layout, err := packstore.NewLayout(filepath.Join(filepath.Dir(f.databasePath), "blobs"),
		packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(t, err)
	hash, err := packstore.ParseHash(citation.RenditionSHA256)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(layout.LoosePath(hash), []byte("quote; tampered suffix"), 0o600))
	_, err = readTextNavigationBlob(t.Context(), f.blobs, citation, size)
	require.ErrorIs(t, err, document.ErrCitationIntegrity)
	require.NoError(t, f.blobs.Remove(citation.RenditionSHA256))
	_, err = readTextNavigationBlob(t.Context(), f.blobs, citation, size)
	require.ErrorIs(t, err, document.ErrCitationUnavailable)
}

func TestTextNavigationReadLimits(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	citation, size := writeCitationBlob(t, f, strings.Repeat("x", document.MaxTextNavigationBytes))
	got, err := readTextNavigationBlob(t.Context(), f.blobs, citation, size)
	require.NoError(t, err)
	require.Len(t, got, document.MaxTextNavigationBytes)
	_, err = readTextNavigationBlob(t.Context(), nil, citation, size+1)
	require.ErrorIs(t, err, document.ErrTextNavigationLimit)
	stream := &instrumentedVerifiedStream{reader: strings.NewReader(strings.Repeat("x", int(size)+8))}
	_, err = readTextNavigationBlob(t.Context(),
		instrumentedVerifiedSource{stream: stream, size: size}, citation, size)
	require.ErrorIs(t, err, document.ErrCitationIntegrity)
	require.NotErrorIs(t, err, document.ErrTextNavigationLimit)
	require.LessOrEqual(t, stream.bytesRead, int(size)+1)
}

type navigationPausedCatalog struct {
	*store.PackCatalog

	entered chan struct{}
	resume  chan struct{}
	calls   atomic.Int32
}

func (c *navigationPausedCatalog) ResolveLocations(
	ctx context.Context, hash packstore.Hash,
) (packstore.Resolution, error) {
	locations, err := c.PackCatalog.ResolveLocations(ctx, hash)
	if err != nil {
		return locations, err
	}
	if c.calls.Add(1) <= 2 {
		c.entered <- struct{}{}
		select {
		case <-ctx.Done():
			return packstore.Resolution{}, ctx.Err()
		case <-c.resume:
		}
	}
	return locations, nil
}

func TestTextNavigationAdmission(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := NewTextCitationTestFixture(t)
		paused := &navigationPausedCatalog{PackCatalog: store.NewPackCatalog(f.Catalog),
			entered: make(chan struct{}, 2), resume: make(chan struct{})}
		release := sync.OnceFunc(func() { close(paused.resume) })
		defer release()
		blobs, err := blob.New(paused, filepath.Join(filepath.Dir(f.fixture.databasePath), "blobs"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, blobs.Close()) })
		f.Service.blobs = blobs
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		request := document.TextOutlineRequest{Citation: f.Citation}
		done := make(chan error, 2)
		for range 2 {
			go func() { _, err := f.Service.GetTextOutline(ctx, request); done <- err }()
		}
		for range 2 {
			select {
			case <-paused.entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
		defer stop()
		_, err = f.Service.GetTextOutline(waiting, request)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, int32(2), paused.calls.Load(), "third read must not reach physical I/O")
		quote, err := f.Service.ResolveTextCitation(ctx, f.Citation)
		require.NoError(t, err)
		require.Equal(t, "é界🙂", quote.Text)
		release()
		for range 2 {
			require.NoError(t, <-done)
		}
		bad := request
		bad.Citation.BuildID = strings.Repeat("f", 64)
		_, err = f.Service.GetTextOutline(ctx, bad)
		require.ErrorIs(t, err, document.ErrCitationUnavailable)
		_, err = f.Service.GetTextOutline(ctx, request)
		require.NoError(t, err)
	})
}

type navigationParsePause struct {
	entered chan struct{}
	resume  <-chan struct{}
}

func (p navigationParsePause) Transform(_ *ast.Document, _ text.Reader, _ parser.Context) {
	p.entered <- struct{}{}
	<-p.resume
}

func TestTextNavigationParsingReleasesCapture(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := NewTextCitationTestFixture(t)
		request := document.TextOutlineRequest{Citation: f.Citation}
		entered, resume := make(chan struct{}, 2), make(chan struct{})
		release := sync.OnceFunc(func() { close(resume) })
		defer release()
		f.Service.textParser = func() parser.Parser {
			p := newTextSectionParser()
			p.AddOptions(parser.WithASTTransformers(
				util.Prioritized(navigationParsePause{entered, resume}, 1000)))
			return p
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		done := make(chan error, 2)
		for range 2 {
			go func() { _, err := f.Service.GetTextOutline(ctx, request); done <- err }()
		}
		for range 2 {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
		defer stop()
		_, err := f.Service.GetTextOutline(waiting, request)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		purge := DerivativePurgeRequest{AttachmentIDs: []string{f.Citation.RenditionAttachmentID}}
		plan, err := f.Service.PlanDerivativePurge(ctx, purge)
		require.NoError(t, err)
		receipt, err := f.Service.RunDerivativePurge(ctx, DerivativePurgeJobRequest{
			DerivativePurgeRequest: purge, PlanFingerprint: plan.Fingerprint})
		require.NoError(t, err, "purge must finish while the actual parser is paused")
		require.Equal(t, 1, receipt.RemovedAttachments)
		release()
		for range 2 {
			require.NoError(t, <-done)
		}
		_, err = f.Service.GetTextOutline(ctx, request)
		require.ErrorIs(t, err, document.ErrCitationUnavailable)
	})
}

func TestTextNavigationMutationAfterSnapshot(t *testing.T) {
	t.Parallel()
	f := NewTextCitationTestFixture(t)
	outline, err := f.Service.GetTextOutline(t.Context(),
		document.TextOutlineRequest{Citation: f.Citation})
	require.NoError(t, err)
	request := document.TextSectionRequest{Citation: f.Citation,
		Section: outline.EnclosingSection, MaxChars: 16000}
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	blobs, err := blob.New(&citationPausedCatalog{PackCatalog: store.NewPackCatalog(f.Catalog),
		entered: entered, resume: resume}, filepath.Join(filepath.Dir(f.fixture.databasePath), "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	f.Service.blobs = blobs
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var page document.TextSectionPage
	go func() { var err error; page, err = f.Service.ReadTextSection(ctx, request); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	err = f.Service.gate.MutateContext(ctx, func() error {
		_, _, err := f.Catalog.Trash(ctx, f.Citation.NodeID, store.UnconditionalRev)
		return err
	})
	require.NoError(t, err, "trash must finish during physical read")
	release()
	require.NoError(t, <-done)
	require.Contains(t, page.Text, "aé界🙂z")
	_, err = f.Service.ReadTextSection(ctx, request)
	require.ErrorIs(t, err, document.ErrCitationUnavailable)
}
