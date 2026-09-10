package processing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

const emailPipelineSource = "Subject: headeronlymarker\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n" +
	"--m\r\nContent-Type: multipart/alternative; boundary=a\r\n\r\n" +
	"--a\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nalternateplainmarker\r\n" +
	"--a\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>outerhtmlmarker &amp; chosen</p><script>scriptonlymarker</script>\r\n--a--\r\n" +
	"--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=synthetic.txt\r\n\r\nattachmentonlymarker\r\n" +
	"--m\r\nContent-Type: message/rfc822\r\n\r\nSubject: nested\r\nContent-Type: text/plain\r\n\r\nnestedonlymarker\r\n--m--\r\n"

type emailPipelineFixture struct {
	catalog *store.Store
	blobs   *blob.Store
	spool   string
}

func newEmailPipelineFixture(t *testing.T) emailPipelineFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	spool := filepath.Join(root, "spools")
	require.NoError(t, os.Mkdir(spool, 0700))
	return emailPipelineFixture{catalog, blobs, spool}
}
func (f emailPipelineFixture) add(t *testing.T, name, source, mime string) store.EmailTarget {
	t.Helper()
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader(source))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), name, receipt.Hash, receipt.Size, mime, processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	view, err := f.catalog.EmailMetadata(t.Context(), node.CurrentVersionID)
	require.True(t, errors.Is(err, store.ErrEmailPending) || errors.Is(err, store.ErrEmailNotSupported))
	return store.EmailTarget{Version: view.Version}
}
func (f emailPipelineFixture) emptySpool(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.spool)
	require.NoError(t, err)
	require.Empty(t, entries)
}
func TestEmailPipelineChosenBodySearchAndQMD(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "message.eml", emailPipelineSource, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	require.Equal(t, "available", view.BodySearch.State)
	require.Equal(t, "1.1.2", *view.Evidence.Inventory.Messages[0].SelectedBodyPath)
	hits, _, err := f.catalog.SearchPage(t.Context(), "outerhtmlmarker", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, target.Version.ID, hits[0].Node.CurrentVersionID)
	for _, term := range []string{"headeronlymarker", "alternateplainmarker", "attachmentonlymarker", "nestedonlymarker", "scriptonlymarker"} {
		hits, _, err = f.catalog.SearchPage(t.Context(), term, 10)
		require.NoError(t, err)
		require.Empty(t, hits, term)
	}
	sources, err := f.catalog.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	require.Equal(t, *view.BodySearch.RenditionBuildID, sources[0].BuildID)
	active, err := f.catalog.ActiveRendition(t.Context(), target.Version.ID, sources[0].ProcessingProfileFingerprint)
	require.NoError(t, err)
	receipt, _, err := document.DecodeEmailBodyReceiptV1(active.Build.ProviderReceipt)
	require.NoError(t, err)
	require.Equal(t, processingHash(emailPipelineSource), receipt.SourceSHA256)
	require.Equal(t, int64(len(emailPipelineSource)), receipt.SourceSize)
	require.Equal(t, view.Generation.ID, receipt.EmailGenerationID)
	require.Equal(t, view.Generation.Checksum, receipt.EmailChecksum)
	require.Equal(t, "1.1.2", receipt.PartPath)
	chosenHTML := "<p>outerhtmlmarker &amp; chosen</p><script>scriptonlymarker</script>"
	require.Equal(t, processingHash(chosenHTML), receipt.BodySHA256)
	require.Equal(t, int64(len(chosenHTML)), receipt.BodySize)
	require.Equal(t, sources[0].MarkdownChecksum, receipt.MarkdownChecksum)
	require.Equal(t, store.RenditionAttachmentID(active.Build.ID, target.Version.ID, sources[0].ProcessingProfileFingerprint), active.Attachment.ID)
	original, _, err := f.blobs.OpenStreamContext(t.Context(), target.Version.BlobHash)
	require.NoError(t, err)
	originalBytes, err := io.ReadAll(original)
	require.NoError(t, err)
	require.NoError(t, original.Close())
	require.Equal(t, emailPipelineSource, string(originalBytes))

	stream, _, err := f.blobs.OpenStreamContext(t.Context(), sources[0].BlobSHA256)
	require.NoError(t, err)
	markdown, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	require.Contains(t, string(markdown), "outerhtmlmarker & chosen")
	for _, term := range []string{"headeronlymarker", "alternateplainmarker", "attachmentonlymarker", "nestedonlymarker", "scriptonlymarker"} {
		require.NotContains(t, string(markdown), term)
	}
	require.NoError(t, f.catalog.ValidateMetadata(t.Context()))
	f.emptySpool(t)
}

type emailFaultCatalog struct {
	*store.Store

	inventory func(context.Context, store.EmailPublication) (store.EmailMetadataView, error)
	body      func(context.Context, store.EmailBodyPublication) error
	stage     func(context.Context, store.RenditionBuildRecord) error
	buildRead func(context.Context, string) (store.RenditionBuildRecord, error)
}

func (f emailFaultCatalog) PublishEmailGeneration(ctx context.Context, p store.EmailPublication) (store.EmailMetadataView, error) {
	if f.inventory != nil {
		return f.inventory(ctx, p)
	}
	return f.Store.PublishEmailGeneration(ctx, p)
}
func (f emailFaultCatalog) PublishEmailBody(ctx context.Context, p store.EmailBodyPublication) error {
	if f.body != nil {
		return f.body(ctx, p)
	}
	return f.Store.PublishEmailBody(ctx, p)
}
func (f emailFaultCatalog) StageRenditionBuild(ctx context.Context, b store.RenditionBuildRecord) error {
	if f.stage != nil {
		return f.stage(ctx, b)
	}
	return f.Store.StageRenditionBuild(ctx, b)
}

type emailFaultBlobs struct {
	*blob.Store

	write func(context.Context, io.Reader) (blob.WriteReceipt, error)
	open  func(context.Context, string) (packstore.VerifiedReadCloser, int64, error)
}

func (f emailFaultBlobs) WriteDetailedContext(ctx context.Context, r io.Reader) (blob.WriteReceipt, error) {
	if f.write != nil {
		return f.write(ctx, r)
	}
	return f.Store.WriteDetailedContext(ctx, r)
}
func (f emailFaultBlobs) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	if f.open != nil {
		return f.open(ctx, hash)
	}
	return f.Store.OpenStreamContext(ctx, hash)
}

type emailCorruptStream struct {
	packstore.VerifiedReadCloser

	changed bool
}

func (r *emailCorruptStream) Read(p []byte) (int, error) {
	n, err := r.VerifiedReadCloser.Read(p)
	if n > 0 && !r.changed {
		p[0] ^= 1
		r.changed = true
	}
	return n, err
}
func TestEmailPipelineFailureBoundariesAndRetry(t *testing.T) {
	for _, failure := range []string{"source corruption", "CAS write", "CAS content", "cancellation", "inventory publication", "inventory references", "body publication", "body bytes"} {
		t.Run(failure, func(t *testing.T) {
			f := newEmailPipelineFixture(t)
			target := f.add(t, "message.eml", emailPipelineSource, "message/rfc822")
			injected := errors.New("synthetic transient failure")
			catalog := emailFaultCatalog{Store: f.catalog}
			blobs := emailFaultBlobs{Store: f.blobs}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case "source corruption":
				blobs.open = func(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
					r, size, err := f.blobs.OpenStreamContext(ctx, hash)
					if err != nil {
						return nil, 0, err
					}
					return &emailCorruptStream{VerifiedReadCloser: r}, size, nil
				}
			case "CAS content":
				blobs.write = func(ctx context.Context, r io.Reader) (blob.WriteReceipt, error) {
					return f.blobs.WriteDetailedContext(ctx, io.MultiReader(r, strings.NewReader("synthetic extra bytes")))
				}
			case "inventory references":
				catalog.inventory = func(ctx context.Context, p store.EmailPublication) (store.EmailMetadataView, error) {
					p.Artifacts = p.Artifacts[:len(p.Artifacts)-1]
					return f.catalog.PublishEmailGeneration(ctx, p)
				}
			case "CAS write":
				blobs.write = func(context.Context, io.Reader) (blob.WriteReceipt, error) { return blob.WriteReceipt{}, injected }
			case "cancellation":
				blobs.write = func(ctx context.Context, r io.Reader) (blob.WriteReceipt, error) {
					cancel()
					return f.blobs.WriteDetailedContext(ctx, r)
				}
			case "inventory publication":
				catalog.inventory = func(context.Context, store.EmailPublication) (store.EmailMetadataView, error) {
					return store.EmailMetadataView{}, injected
				}
			case "body publication":
				catalog.body = func(context.Context, store.EmailBodyPublication) error { return injected }
			case "body bytes":
				blobs.open = func(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
					r, size, err := f.blobs.OpenStreamContext(ctx, hash)
					if err != nil {
						return nil, 0, err
					}
					if hash != target.Version.BlobHash {
						return &emailCorruptStream{VerifiedReadCloser: r}, size, nil
					}
					return r, size, nil
				}
			}
			_, err := ensureEmailTarget(ctx, catalog, blobs, f.spool, target)
			require.Error(t, err)
			if failure == "cancellation" {
				require.ErrorIs(t, err, context.Canceled)
			}
			if failure == "CAS write" || failure == "inventory publication" || failure == "body publication" {
				require.ErrorIs(t, err, injected)
			}
			view, err := f.catalog.EmailMetadata(t.Context(), target.Version.ID)
			inventoryCommitted := failure == "body publication" || failure == "body bytes"
			if inventoryCommitted {
				require.NoError(t, err)
				require.Equal(t, "pending", view.BodySearch.State)
			} else {
				require.ErrorIs(t, err, store.ErrEmailPending)
				require.Empty(t, view.Generation.ID)
			}
			hits, _, err := f.catalog.SearchPage(t.Context(), "outerhtmlmarker", 10)
			require.NoError(t, err)
			require.Empty(t, hits)
			sources, err := f.catalog.QMDExportSources(t.Context(), 10)
			require.NoError(t, err)
			require.Empty(t, sources)
			fingerprint, err := EmailDecoderFingerprint()
			require.NoError(t, err)
			bodyProfileFingerprint, err := EmailBodyProfileFingerprint()
			require.NoError(t, err)
			missing, err := f.catalog.MissingEmailTargetsAfter(
				t.Context(), fingerprint, bodyProfileFingerprint, "", 100,
			)
			require.NoError(t, err)
			require.Len(t, missing, 1)
			f.emptySpool(t)
			retried, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
			require.NoError(t, err)
			require.Equal(t, "available", retried.BodySearch.State)
			if inventoryCommitted {
				require.Equal(t, view.Generation.ID, retried.Generation.ID)
				require.Equal(t, view.Attachment.ID, retried.Attachment.ID)
			}
			require.NoError(t, f.catalog.VerifyRenditionBlobBytes(t.Context(), f.blobs))
			f.emptySpool(t)
		})
	}
}
func TestEmailPipelineUnavailableDoesNotRequeue(t *testing.T) {
	for _, tc := range []struct{ name, source, reason string }{
		{"blank", "Content-Type: text/plain\r\n\r\n \t\r\n", "empty_body"},
		{"empty HTML", "Content-Type: text/html\r\n\r\n<script>hidden</script><template>hidden</template>", "empty_body"},
		{"unsupported charset", "Content-Type: text/plain; charset=not-a-charset\r\n\r\ntext", "no_supported_body"},
		{"unsupported media", "Content-Type: application/octet-stream\r\n\r\ntext", "no_supported_body"},
		{"encrypted", "Content-Type: multipart/encrypted; boundary=e; protocol=application/pgp-encrypted\r\n\r\n--e\r\nContent-Type: application/pgp-encrypted\r\n\r\nVersion: 1\r\n--e\r\nContent-Type: application/octet-stream\r\n\r\nopaque\r\n--e--\r\n", "no_supported_body"},
		{"text limit", "Content-Type: text/html; charset=utf-8\r\n\r\n" + strings.Repeat("&nGt;", (16<<20)/5), "body_text_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEmailPipelineFixture(t)
			target := f.add(t, "message.eml", tc.source, "message/rfc822")
			view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
			require.NoError(t, err)
			require.Equal(t, "unavailable", view.BodySearch.State)
			require.Equal(t, tc.reason, *view.BodySearch.Reason)
			fingerprint, err := EmailDecoderFingerprint()
			require.NoError(t, err)
			bodyProfileFingerprint, err := EmailBodyProfileFingerprint()
			require.NoError(t, err)
			missing, err := f.catalog.MissingEmailTargetsAfter(
				t.Context(), fingerprint, bodyProfileFingerprint, "", 100,
			)
			require.NoError(t, err)
			require.Empty(t, missing)
			var metadata bytes.Buffer
			require.NoError(t, f.catalog.ExportMetadata(t.Context(), &metadata))
			require.Contains(t, metadata.String(), `"type":"email_body_result"`)
			again, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
			require.NoError(t, err)
			require.Equal(t, view, again)
			f.emptySpool(t)
		})
	}
}
func TestEmailPipelineOversizedCatalogRefusalDoesNotReadSource(t *testing.T) {
	f := newEmailPipelineFixture(t)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "oversize.eml", processingHash("synthetic catalog-only original"), (128<<20)+1, "message/rfc822")
	require.NoError(t, err)
	version, err := f.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	blobs := emailFaultBlobs{Store: f.blobs, open: func(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
		return nil, 0, errors.New("over-limit source must not open")
	}}
	view, err := ensureEmailTarget(t.Context(), f.catalog, blobs, f.spool, store.EmailTarget{Version: version})
	require.NoError(t, err)
	require.Equal(t, document.EmailOutcome("unavailable"), view.Evidence.Outcome)
	require.Equal(t, document.EmailVerification("catalog_only"), view.Evidence.Source.Verification)
	require.Equal(t, "inventory_unavailable", *view.BodySearch.Reason)
	require.Empty(t, view.Generation.Artifacts)
	var metadata bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(t.Context(), &metadata))
	require.Contains(t, metadata.String(), `"type":"email_body_result"`)
	f.emptySpool(t)
}
func TestEmailPipelineEqualBytesReuseAndDistinctAttachments(t *testing.T) {
	f := newEmailPipelineFixture(t)
	first := f.add(t, "first.eml", emailPipelineSource, "message/rfc822")
	second := f.add(t, "second.eml", emailPipelineSource, "application/octet-stream")
	one, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, first)
	require.NoError(t, err)
	// A missing source read or spool parent would fail decoding: exact generation reuse must avoid both.
	blobs := emailFaultBlobs{Store: f.blobs, open: func(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
		if hash == first.Version.BlobHash {
			return nil, 0, errors.New("exact source already decoded")
		}
		return f.blobs.OpenStreamContext(ctx, hash)
	}}
	two, err := ensureEmailTarget(t.Context(), f.catalog, blobs, filepath.Join(f.spool, "missing"), second)
	require.NoError(t, err)
	require.Equal(t, one.Generation.ID, two.Generation.ID)
	require.NotEqual(t, one.Attachment.ID, two.Attachment.ID)
	require.Equal(t, one.BodySearch.RenditionBuildID, two.BodySearch.RenditionBuildID)
	require.NotEqual(t, *one.BodySearch.RenditionAttachmentID, *two.BodySearch.RenditionAttachmentID)
	require.Equal(t, "application/octet-stream", two.Version.MimeType)
	_, err = f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{first.Version.ID}})
	require.NoError(t, err)
	_, err = EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, first)
	require.ErrorIs(t, err, store.ErrEmailDerivativeSuppressed)
	remaining, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, second)
	require.NoError(t, err)
	require.Equal(t, "available", remaining.BodySearch.State)
	require.NoError(t, f.catalog.VerifyRenditionBlobBytes(t.Context(), f.blobs))
	f.emptySpool(t)
}
func TestEmailPipelineBackfillJoinsFailuresAndProgresses(t *testing.T) {
	f := newEmailPipelineFixture(t)
	one := f.add(t, "one.eml", emailPipelineSource, "message/rfc822")
	two := f.add(t, "two.eml", emailPipelineSource, "message/rfc822")
	three := f.add(t, "three.eml", emailPipelineSource, "message/rfc822")
	firstErr, secondErr := errors.New("synthetic first"), errors.New("synthetic second")
	catalog := emailFaultCatalog{Store: f.catalog, body: func(ctx context.Context, p store.EmailBodyPublication) error {
		switch p.RenditionAttachment.ContentVersionID {
		case one.Version.ID:
			return firstErr
		case two.Version.ID:
			return secondErr
		}
		return f.catalog.PublishEmailBody(ctx, p)
	}}
	count, err := backfillEmailTargets(t.Context(), catalog, f.blobs, f.spool, []store.EmailTarget{one, two, three})
	require.Equal(t, 1, count)
	require.ErrorIs(t, err, firstErr)
	require.ErrorIs(t, err, secondErr)
	for _, target := range []store.EmailTarget{one, two} {
		view, err := f.catalog.EmailMetadata(t.Context(), target.Version.ID)
		require.NoError(t, err)
		require.Equal(t, "pending", view.BodySearch.State)
	}
	view, err := f.catalog.EmailMetadata(t.Context(), three.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "available", view.BodySearch.State)
	count, err = BackfillEmailTargets(t.Context(), f.catalog, f.blobs, f.spool, []store.EmailTarget{one, two, three})
	require.NoError(t, err)
	require.Equal(t, 3, count)
	f.emptySpool(t)
}

func TestEmailPipelinePreservesLegacyAndUnrelatedHeads(t *testing.T) {
	f := newEmailPipelineFixture(t)
	legacy := f.add(t, "old.txt", "legacypreservedmarker", "text/plain")
	require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{BlobHash: legacy.Version.BlobHash, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: "legacypreservedmarker"}))
	target := f.add(t, "message.eml", emailPipelineSource, "message/rfc822")
	_, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	hits, _, err := f.catalog.SearchPage(t.Context(), "legacypreservedmarker", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, legacy.Version.ID, hits[0].Node.CurrentVersionID)
	// A separate ordinary profile on this same version remains selected after a
	// different email generation refuses body processing.
	ep, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	rp, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 100_000, MaxUnitRunes: 1000, MaxSegmentRunes: 100})
	require.NoError(t, err)
	pf := publicationFixture{catalog: f.catalog, blobs: f.blobs, profile: processingProfile(t), evidencePolicy: ep, renditionPolicy: rp, versionID: target.Version.ID}
	p, err := NewArtifactPublisher(f.catalog, f.blobs)
	require.NoError(t, err)
	_, err = p.PublishRendition(t.Context(), pf.stageForSource(t, publicationIDs{build: processingHash("other build"), attachment: processingHash("other attachment"), generation: processingHash("other projection")}, "unrelatedpreservedmarker", "unrelatedpreservedmarker", target.Version.ID, target.Version.BlobHash))
	require.NoError(t, err)
	old, err := f.catalog.EmailMetadata(t.Context(), target.Version.ID)
	require.NoError(t, err)
	evidence := old.Evidence
	evidence.Recipe.MultipartSourceSHA256 = processingHash("synthetic next parser source")
	evidence.Outcome = "unavailable"
	evidence.Inventory = nil
	evidence.Failure = &document.EmailFailureV1{Code: "source_unsupported", Operation: "source", Detail: "Synthetic next decoder refusal."}
	canonical, _, err := document.MarshalEmailV1(evidence)
	require.NoError(t, err)
	next, err := f.catalog.PublishEmailGeneration(t.Context(), store.EmailPublication{ContentVersionID: target.Version.ID, CanonicalJSON: canonical, Artifacts: []store.EmailPartArtifactRecord{}})
	require.NoError(t, err)
	require.Equal(t, "unavailable", next.BodySearch.State)
	hits, _, err = f.catalog.SearchPage(t.Context(), "outerhtmlmarker", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
	for _, term := range []string{"legacypreservedmarker", "unrelatedpreservedmarker"} {
		hits, _, err = f.catalog.SearchPage(t.Context(), term, 10)
		require.NoError(t, err)
		require.Len(t, hits, 1)
	}
	sources, err := f.catalog.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	for _, source := range sources {
		require.NotEqual(t, *old.BodySearch.RenditionAttachmentID, source.AttachmentID)
	}
	require.NoError(t, f.catalog.ValidateMetadata(t.Context()))
}
func TestEmailPipelineSuppressionRaces(t *testing.T) {
	for _, phase := range []string{"inventory", "body inventory purge", "body final fence", "body staging fence"} {
		t.Run(phase, func(t *testing.T) {
			f := newEmailPipelineFixture(t)
			target := f.add(t, "message.eml", emailPipelineSource, "message/rfc822")
			entered, resume := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			finished := make(chan struct{})
			t.Cleanup(func() { release(); <-finished })
			catalog := emailFaultCatalog{Store: f.catalog}
			pause := func() { close(entered); <-resume }
			switch phase {
			case "inventory":
				catalog.inventory = func(ctx context.Context, p store.EmailPublication) (store.EmailMetadataView, error) {
					pause()
					return f.catalog.PublishEmailGeneration(ctx, p)
				}
			case "body staging fence":
				catalog.stage = func(ctx context.Context, b store.RenditionBuildRecord) error {
					pause()
					return f.catalog.StageRenditionBuild(ctx, b)
				}
			default:
				catalog.body = func(ctx context.Context, p store.EmailBodyPublication) error {
					pause()
					return f.catalog.PublishEmailBody(ctx, p)
				}
			}
			type outcome struct {
				view store.EmailMetadataView
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				defer close(finished)
				view, err := ensureEmailTarget(t.Context(), catalog, f.blobs, f.spool, target)
				done <- outcome{view, err}
			}()
			select {
			case <-entered:
			case premature := <-done:
				t.Fatalf("email pipeline returned before the selected fence: %v", premature.err)
			}
			if phase == "inventory" || phase == "body inventory purge" {
				_, err := f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{ContentVersionIDs: []string{target.Version.ID}})
				require.NoError(t, err)
			} else {
				// A competing writer completes, then the user purges that actual body
				// before the paused producer reaches either staging or the final fence.
				view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
				require.NoError(t, err)
				_, err = f.catalog.PurgeDerivatives(t.Context(), store.PurgeRequest{AttachmentIDs: []string{*view.BodySearch.RenditionAttachmentID}})
				require.NoError(t, err)
			}
			release()
			result := <-done
			if phase == "inventory" || phase == "body inventory purge" {
				require.ErrorIs(t, result.err, store.ErrEmailDerivativeSuppressed)
			} else {
				require.NoError(t, result.err)
				require.Equal(t, "unavailable", result.view.BodySearch.State)
				require.Equal(t, "derivative_purged", *result.view.BodySearch.Reason)
			}
			fingerprint, err := EmailDecoderFingerprint()
			require.NoError(t, err)
			bodyProfileFingerprint, err := EmailBodyProfileFingerprint()
			require.NoError(t, err)
			missing, err := f.catalog.MissingEmailTargetsAfter(
				t.Context(), fingerprint, bodyProfileFingerprint, "", 100,
			)
			require.NoError(t, err)
			require.Empty(t, missing)
			hits, _, err := f.catalog.SearchPage(t.Context(), "outerhtmlmarker", 10)
			require.NoError(t, err)
			require.Empty(t, hits)
			f.emptySpool(t)
		})
	}
}
func TestEmailPipelineStaleProjectionRebuilds(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "message.eml", emailPipelineSource, "message/rfc822")
	later := f.add(t, "later.eml", "Content-Type: text/plain\r\n\r\nlaterprojectionmarker", "message/rfc822")
	first := true
	catalog := emailFaultCatalog{Store: f.catalog, body: func(ctx context.Context, p store.EmailBodyPublication) error {
		if first {
			first = false
			_, err := EnsureEmailTarget(ctx, f.catalog, f.blobs, f.spool, later)
			if err != nil {
				return err
			}
		}
		return f.catalog.PublishEmailBody(ctx, p)
	}}
	view, err := ensureEmailTarget(t.Context(), catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	require.Equal(t, "available", view.BodySearch.State)
	for _, term := range []string{"outerhtmlmarker", "laterprojectionmarker"} {
		hits, _, err := f.catalog.SearchPage(t.Context(), term, 10)
		require.NoError(t, err)
		require.Len(t, hits, 1)
	}
	f.emptySpool(t)
}
func TestEmailPipelineFullBodyTailIsSearchable(t *testing.T) {
	f := newEmailPipelineFixture(t)
	source := "Content-Type: text/plain; charset=utf-8\r\n\r\n" + strings.Repeat("x ", (16<<20)/2-14) + " fullbodytailmarker"
	target := f.add(t, "large.eml", source, "message/rfc822")
	view, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	require.Equal(t, "available", view.BodySearch.State)
	hits, _, err := f.catalog.SearchPage(t.Context(), "fullbodytailmarker", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	sources, err := f.catalog.QMDExportSources(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	reader, _, err := f.blobs.OpenStreamContext(t.Context(), sources[0].BlobSHA256)
	require.NoError(t, err)
	markdown, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Contains(t, string(markdown), "fullbodytailmarker")
	f.emptySpool(t)
}

func (f emailFaultCatalog) RenditionBuild(ctx context.Context, id string) (store.RenditionBuildRecord, error) {
	if f.buildRead != nil {
		return f.buildRead(ctx, id)
	}
	return f.Store.RenditionBuild(ctx, id)
}
func TestEmailPipelineBodyObservationsAndStagedRetry(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "message.eml", emailPipelineSource, "message/rfc822")
	injected := errors.New("synthetic staged body failure")
	var inventoryObserved time.Time
	var staged store.RenditionBuildRecord
	catalog := emailFaultCatalog{Store: f.catalog,
		inventory: func(ctx context.Context, p store.EmailPublication) (store.EmailMetadataView, error) {
			view, err := f.catalog.PublishEmailGeneration(ctx, p)
			inventoryObserved = time.Now().UTC()
			return view, err
		},
		stage: func(ctx context.Context, b store.RenditionBuildRecord) error {
			staged = b
			return f.catalog.StageRenditionBuild(ctx, b)
		},
		body: func(context.Context, store.EmailBodyPublication) error { return injected },
	}
	view, err := ensureEmailTarget(t.Context(), catalog, f.blobs, f.spool, target)
	require.ErrorIs(t, err, injected)
	require.Equal(t, "pending", view.BodySearch.State)
	first, err := f.catalog.RenditionBuild(t.Context(), staged.ID)
	require.NoError(t, err)
	completed, err := time.Parse(time.RFC3339Nano, first.CompletedAt)
	require.NoError(t, err)
	require.False(t, completed.Before(inventoryObserved), "body completion must follow inventory publication")
	mismatch := emailFaultCatalog{Store: f.catalog, stage: func(ctx context.Context, b store.RenditionBuildRecord) error {
		require.Equal(t, first.CompletedAt, b.CompletedAt)
		b.AuthorizationChecksum = processingHash("synthetic mismatched declaration")
		return f.catalog.StageRenditionBuild(ctx, b)
	}}
	_, err = ensureEmailTarget(t.Context(), mismatch, f.blobs, f.spool, target)
	require.Error(t, err)
	pending, err := f.catalog.EmailMetadata(t.Context(), target.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", pending.BodySearch.State)
	retryStarted := time.Now().UTC()
	final, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	require.Equal(t, "available", final.BodySearch.State)
	profile, err := document.EmailBodyProfileV1(final.Evidence.Recipe)
	require.NoError(t, err)
	_, fp, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	active, err := f.catalog.ActiveRendition(t.Context(), target.Version.ID, fp.Profile)
	require.NoError(t, err)
	require.Equal(t, first.CompletedAt, active.Build.CompletedAt)
	require.Equal(t, first.ID, active.Build.ID)
	attached, err := time.Parse(time.RFC3339Nano, active.Attachment.AttachedAt)
	require.NoError(t, err)
	require.False(t, attached.Before(retryStarted))
	published, err := time.Parse(time.RFC3339Nano, active.Head.PublishedAt)
	require.NoError(t, err)
	require.False(t, published.Before(retryStarted))
	second := f.add(t, "equal.eml", emailPipelineSource, "message/rfc822")
	secondStarted := time.Now().UTC()
	equal, err := EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, second)
	require.NoError(t, err)
	other, err := f.catalog.ActiveRendition(t.Context(), second.Version.ID, fp.Profile)
	require.NoError(t, err)
	require.Equal(t, active.Build.CompletedAt, other.Build.CompletedAt)
	require.Equal(t, final.Generation.ID, equal.Generation.ID)
	otherAttached, err := time.Parse(time.RFC3339Nano, other.Attachment.AttachedAt)
	require.NoError(t, err)
	require.False(t, otherAttached.Before(secondStarted))
	require.NotEqual(t, active.Attachment.ID, other.Attachment.ID)
	f.emptySpool(t)
}
func TestEmailPipelineConcurrentFirstBuildInsertionRemainsRetryable(t *testing.T) {
	f := newEmailPipelineFixture(t)
	target := f.add(t, "message.eml", emailPipelineSource, "message/rfc822")
	injected := errors.New("synthetic competing staged-only producer")
	competing := emailFaultCatalog{Store: f.catalog, body: func(context.Context, store.EmailBodyPublication) error { return injected }}
	catalog := emailFaultCatalog{Store: f.catalog, buildRead: func(ctx context.Context, id string) (store.RenditionBuildRecord, error) {
		// Hold the actual absent read result across the competing first insertion.
		absent, err := f.catalog.RenditionBuild(ctx, id)
		require.ErrorIs(t, err, store.ErrNotFound)
		_, competingErr := ensureEmailTarget(ctx, competing, f.blobs, f.spool, target)
		require.ErrorIs(t, competingErr, injected)
		return absent, err
	}}
	_, err := ensureEmailTarget(t.Context(), catalog, f.blobs, f.spool, target)
	require.Error(t, err)
	view, err := f.catalog.EmailMetadata(t.Context(), target.Version.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", view.BodySearch.State)
	view, err = EnsureEmailTarget(t.Context(), f.catalog, f.blobs, f.spool, target)
	require.NoError(t, err)
	require.Equal(t, "available", view.BodySearch.State)
	f.emptySpool(t)
}
