package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

func TestRootPackageConstructor(t *testing.T) {
	vault, err := docbank.New(context.Background(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
}

func TestRootPackageRecoversInterruptedProcessingUploads(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "default"
		if custom {
			name = "custom"
		}
		t.Run(name, func(t *testing.T) {
			config := docbank.Config{Root: t.TempDir()}
			spool := filepath.Join(config.Root, "blobs", "tmp")
			if custom {
				spool = t.TempDir()
				config.Processing.SpoolDirectory = spool
			}
			vault, err := docbank.New(t.Context(), config)
			require.NoError(t, err)
			require.NoError(t, vault.Close())

			stale := filepath.Join(spool, ".docbank-upload-synthetic")
			require.NoError(t, os.Mkdir(stale, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(stale, "source"), []byte("partial upload"), 0o600))
			loose := filepath.Join(config.Root, "blobs", "tmp", "partial-blob")
			require.NoError(t, os.WriteFile(loose, []byte("partial blob"), 0o600))
			unrelated := filepath.Join(spool, "keep.txt")
			if custom {
				require.NoError(t, os.WriteFile(unrelated, []byte("keep"), 0o600))
			}

			vault, err = docbank.New(t.Context(), config)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			_, err = os.Stat(stale)
			require.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(loose)
			require.ErrorIs(t, err, os.ErrNotExist)
			if custom {
				content, err := os.ReadFile(unrelated)
				require.NoError(t, err)
				require.Equal(t, "keep", string(content))
			}
		})
	}
}

func TestRootPackageOwnsProcessingSpoolUntilClose(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "default"
		if custom {
			name = "custom"
		}
		t.Run(name, func(t *testing.T) {
			config := docbank.Config{Root: t.TempDir()}
			spool := filepath.Join(config.Root, "blobs", "tmp")
			if custom {
				spool = t.TempDir()
				config.Processing.SpoolDirectory = spool
			}
			first, err := docbank.New(t.Context(), config)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, first.Close()) })
			active := filepath.Join(spool, ".docbank-upload-active")
			require.NoError(t, os.Mkdir(active, 0o700))
			source := filepath.Join(active, "source")
			require.NoError(t, os.WriteFile(source, []byte("active upload"), 0o600))

			otherConfig := docbank.Config{Root: t.TempDir(),
				Processing: docbank.ProcessingOptions{SpoolDirectory: spool}}
			other, err := docbank.New(t.Context(), otherConfig)
			if other != nil {
				t.Cleanup(func() { require.NoError(t, other.Close()) })
			}
			require.ErrorIs(t, err, docbank.ErrProcessingSpoolLocked)
			content, err := os.ReadFile(source)
			require.NoError(t, err)
			require.Equal(t, "active upload", string(content))

			require.NoError(t, first.Close())
			other, err = docbank.New(t.Context(), otherConfig)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			_, err = os.Stat(active)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRootPackageReleasesProcessingSpoolAfterOpenFailure(t *testing.T) {
	config := docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
		SpoolDirectory: t.TempDir(), Profiles: map[string]docbank.ProcessingProfileConfig{"invalid": {}},
	}}
	_, err := docbank.New(t.Context(), config)
	require.Error(t, err)
	config.Processing.Profiles = nil
	vault, err := docbank.New(t.Context(), config)
	require.NoError(t, err)
	require.NoError(t, vault.Close())
}

func TestEmbeddedProcessingPlanRunReadAndSearch(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	profile := embeddedProcessingProfile(t, provider.Descriptor())
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: profile, RenditionProvider: provider},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	_, err = vault.Put(t.Context(), "/needle-outside-fence.txt", strings.NewReader("outside"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	receipt, err := vault.Put(t.Context(), "/private.txt",
		strings.NewReader("# Private note\n\nA source-fenced needle.\n"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "private"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	require.NotEmpty(t, plan.Fingerprint)
	require.Equal(t, "local_process", plan.Flow[0].TrustBoundary)
	require.Contains(t, plan.RetainedClasses, "sanitized_markdown")
	require.True(t, plan.ConsentRequired)

	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest:     docbank.ProcessingPlanRequest{Selector: selector},
		PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equalf(t, "completed", status.State, "status: %+v", status)
	repeated, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest:     docbank.ProcessingPlanRequest{Selector: selector},
		PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	require.Equal(t, job.RenditionJobID, repeated.RenditionJobID)

	rendition, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
	require.NoError(t, err)
	body, err := io.ReadAll(rendition.Reader)
	require.NoError(t, err)
	require.NoError(t, rendition.Reader.Close())
	require.True(t, bytes.HasPrefix(body, []byte("---\ndocbank:\n  contract: \"docbank-sanitized-markdown/v1\"\n")))
	require.Contains(t, string(body), "    format: \"txt\"")
	require.Contains(t, string(body), "needle")

	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchLexical, Profile: "private", Fence: fence,
	})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
	require.NotContains(t, report.Results[0].Excerpt, "docbank-sanitized-markdown")

	metadataOnly, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "sanitized-markdown", Mode: docbank.DocumentSearchLexical, Profile: "private", Fence: fence,
	})
	require.NoError(t, err)
	require.Empty(t, metadataOnly.Results)
}

func TestEmbeddedProcessingRejectsForeignFenceAndClosedVault(t *testing.T) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider},
		}}})
	require.NoError(t, err)
	_, err = vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "value",
		Mode: docbank.DocumentSearchLexical, Profile: "private", Fence: docbank.DocumentSourceFence{
			VaultUID:          "00000000-0000-4000-8000-000000000000",
			ContentVersionIDs: []string{"00000000-0000-4000-8000-000000000001"},
		}})
	require.ErrorIs(t, err, docbank.ErrForeignVault)
	require.NoError(t, vault.Close())
	_, err = vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{})
	require.ErrorIs(t, err, docbank.ErrClosed)
}

func TestEmbeddedProcessingWaitsForSharedRenditionAndHonorsCancellation(t *testing.T) {
	plaintextProvider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	provider := &waitingRenditionProvider{RenditionProvider: plaintextProvider,
		started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(provider.release) })
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"shared": {Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	requests := make([]docbank.StartProcessingRequest, 2)
	for index := range requests {
		receipt, err := vault.Put(t.Context(), fmt.Sprintf("/shared-%d.txt", index),
			strings.NewReader("shared rendition source"), docbank.PutOptions{MediaType: "text/plain"})
		require.NoError(t, err)
		request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{
			NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "shared"}}
		plan, err := vault.PlanProcessing(t.Context(), request)
		require.NoError(t, err)
		requests[index] = docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true}
	}
	var first docbank.ProcessingJob
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		first, firstErr = vault.StartProcessing(t.Context(), requests[0])
	}()
	t.Cleanup(func() { release(); <-done })
	select {
	case <-provider.started:
	case <-done:
		t.Fatalf("first processing request returned before rendering: %v", firstErr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = vault.StartProcessing(ctx, requests[1])
	require.ErrorIs(t, err, context.DeadlineExceeded, "joining a live rendition must wait without failing its lease")
	release()
	<-done
	require.NoError(t, firstErr)
	joined, err := vault.StartProcessing(t.Context(), requests[1])
	require.NoError(t, err)
	require.Equal(t, first.RenditionJobID, joined.RenditionJobID)
	require.Equal(t, int32(1), provider.calls.Load(), "both versions must share one provider execution")
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: joined.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
}

func TestEmbeddedProcessingRequiresOwnRenditionPublication(t *testing.T) {
	for _, chunks := range []bool{false, true} {
		for _, trash := range []bool{false, true} {
			t.Run(fmt.Sprintf("chunks=%t/trash=%t", chunks, trash), func(t *testing.T) {
				base, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
				require.NoError(t, err)
				provider := &waitingRenditionProvider{RenditionProvider: base, started: make(chan struct{}), release: make(chan struct{})}
				release := sync.OnceFunc(func() { close(provider.release) })
				embedding := newSyntheticEmbeddingProvider(t)
				config := docbank.ProcessingProfileConfig{Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider}
				if chunks {
					config.Profile.Embeddings = []document.EmbeddingBindingV1{syntheticChunkEmbeddingBinding(embedding.descriptor)}
					config.EmbeddingProviders = map[string]document.EmbeddingProvider{"chunks": embedding}
					config.Tokenizers = map[string]document.Tokenizer{"chunks": syntheticRuneTokenizer{}}
				}
				root := t.TempDir()
				vault, err := docbank.New(t.Context(), docbank.Config{Root: root, Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{"test": config}}})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, vault.Close()) })
				requests := make([]docbank.StartProcessingRequest, 2)
				for i := range requests {
					receipt, err := vault.Put(t.Context(), fmt.Sprintf("/source-%d.txt", i), strings.NewReader("synthetic shared source"), docbank.PutOptions{MediaType: "text/plain"})
					require.NoError(t, err)
					request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "test"}}
					plan, err := vault.PlanProcessing(t.Context(), request)
					require.NoError(t, err)
					requests[i] = docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true}
				}
				db, err := store.DefaultSQLiteDriver().Open(filepath.Join(root, "docbank.db"), docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				jobs := make([]docbank.ProcessingJob, 2)
				errs := make([]error, 2)
				dones := []chan struct{}{make(chan struct{}), make(chan struct{})}
				go func() {
					defer close(dones[0])
					jobs[0], errs[0] = vault.StartProcessing(ctx, requests[0])
				}()
				t.Cleanup(func() { release(); cancel(); <-dones[0] })
				select {
				case <-provider.started:
				case <-dones[0]:
					t.Fatalf("first returned: %v", errs[0])
				}
				go func() {
					defer close(dones[1])
					jobs[1], errs[1] = vault.StartProcessing(ctx, requests[1])
				}()
				t.Cleanup(func() { release(); cancel(); <-dones[1] })
				require.Eventually(t, func() bool {
					var n int
					return db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rendition_job_waiters WHERE state='waiting'").Scan(&n) == nil && n == 2
				}, 10*time.Second, 10*time.Millisecond)
				if trash {
					_, err = vault.TrashPath(ctx, "/source-1.txt", docbank.RevisionOptions{})
					require.NoError(t, err)
				}
				release()
				<-dones[0]
				<-dones[1]
				require.NoError(t, errs[0])
				var shared, waiterID, waiterState, code string
				require.NoError(t, db.QueryRowContext(ctx, "SELECT j.state,w.waiter_id,w.state,COALESCE(w.failure_code,'') FROM rendition_job_waiters w JOIN rendition_jobs j ON w.job_id=j.job_id WHERE w.content_version_id=?", requests[1].PlanRequest.Selector.ContentVersionID).Scan(&shared, &waiterID, &waiterState, &code))
				status, err := vault.ProcessingStatus(ctx, docbank.ProcessingStatusRequest{JobID: waiterID})
				require.NoError(t, err)
				var embeddingJobs int
				require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM embedding_jobs WHERE content_version_id=?", requests[1].PlanRequest.Selector.ContentVersionID).Scan(&embeddingJobs))
				require.Equal(t, "completed", shared)
				require.Equal(t, int32(1), provider.calls.Load(), "both requests share one provider execution")
				if trash {
					require.Equal(t, "rejected", waiterState)
					require.Equal(t, "stale_authority", code)
					require.ErrorIs(t, errs[1], docbank.ErrProcessingPlanChanged)
					require.Empty(t, jobs[1].ID)
					require.Zero(t, embeddingJobs, "rejected requests must not enqueue embeddings")
				} else {
					require.NoError(t, errs[1])
					require.Equal(t, "published", waiterState)
					require.Equal(t, "completed", status.State)
					// Repeated starts also exercise the already-completed path.
					repeated, err := vault.StartProcessing(ctx, requests[1])
					require.NoError(t, err)
					require.Equal(t, jobs[1], repeated)
					require.Equal(t, int32(1), provider.calls.Load())
				}
			})
		}
	}
}

type waitingRenditionProvider struct {
	document.RenditionProvider

	started, release chan struct{}
	calls            atomic.Int32
	filenames        []string
}

func TestEmbeddedProcessingWaitsForRenditionOutcome(t *testing.T) {
	for _, chunks := range []bool{false, true} {
		for _, test := range []struct {
			name string
			code document.RenditionErrorCode
			want error
		}{
			{"retry", document.RenditionErrorRateLimited, nil},
			{"failed", document.RenditionErrorUnsupportedInput, docbank.ErrRenditionFailed},
			{"operator required", document.RenditionErrorAmbiguousSubmission, docbank.ErrRenditionOperatorRequired},
		} {
			t.Run(fmt.Sprintf("%s/chunks=%t", test.name, chunks), func(t *testing.T) {
				plain, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
				require.NoError(t, err)
				delay := time.Duration(0)
				if test.want == nil {
					delay = 20 * time.Millisecond
				}
				failure, err := document.NewRenditionProviderError(test.code, delay, nil)
				require.NoError(t, err)
				provider := &outcomeRenditionProvider{RenditionProvider: plain, failure: failure}
				embedding := newSyntheticEmbeddingProvider(t)
				config := docbank.ProcessingProfileConfig{Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider}
				if chunks {
					config.Profile.Embeddings = []document.EmbeddingBindingV1{syntheticChunkEmbeddingBinding(embedding.descriptor)}
					config.EmbeddingProviders = map[string]document.EmbeddingProvider{"chunks": embedding}
					config.Tokenizers = map[string]document.Tokenizer{"chunks": syntheticRuneTokenizer{}}
				}
				vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
					Profiles: map[string]docbank.ProcessingProfileConfig{"test": config}}})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, vault.Close()) })
				receipt, err := vault.Put(t.Context(), "/source.txt", strings.NewReader("synthetic chunk semantic needle"), docbank.PutOptions{MediaType: "text/plain"})
				require.NoError(t, err)
				request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "test"}}
				plan, err := vault.PlanProcessing(t.Context(), request)
				require.NoError(t, err)
				start := docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true}
				job, err := vault.StartProcessing(t.Context(), start)
				if test.want != nil {
					require.ErrorIs(t, err, test.want)
					require.Zero(t, embedding.calls.Load())
					_, err = vault.StartProcessing(t.Context(), start)
					require.ErrorIs(t, err, test.want)
					require.Equal(t, int32(1), provider.calls.Load(), "terminal work must not be retried")
					return
				}
				require.NoError(t, err)
				status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
				require.NoError(t, err)
				require.Equal(t, "completed", status.State)
				require.Equal(t, int32(2), provider.calls.Load())
				if chunks {
					require.Len(t, job.EmbeddingJobIDs, 1)
					require.Equal(t, 1, status.CompletedBindings)
					require.Equal(t, int32(1), embedding.calls.Load())
				}
			})
		}
	}
}

func TestEmbeddedProcessingCancelsRenditionBackoff(t *testing.T) {
	plain, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	failure, err := document.NewRenditionProviderError(document.RenditionErrorRateLimited, 10*time.Minute, nil)
	require.NoError(t, err)
	provider := &outcomeRenditionProvider{RenditionProvider: plain, failure: failure}
	embedding := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, provider.Descriptor())
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticChunkEmbeddingBinding(embedding.descriptor)}
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root, Processing: docbank.ProcessingOptions{
		Profiles: map[string]docbank.ProcessingProfileConfig{"test": {Profile: profile, RenditionProvider: provider,
			EmbeddingProviders: map[string]document.EmbeddingProvider{"chunks": embedding},
			Tokenizers:         map[string]document.Tokenizer{"chunks": syntheticRuneTokenizer{}}}}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/source.txt", strings.NewReader("synthetic chunk semantic needle"), docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "test"}}
	plan, err := vault.PlanProcessing(t.Context(), request)
	require.NoError(t, err)
	// Observe the committed retry state before canceling, without timing the provider.
	db, err := store.DefaultSQLiteDriver().Open(filepath.Join(root, "docbank.db"), docsqlite.OpenOptions{
		Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Deferred})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = vault.StartProcessing(ctx, docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
	}()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool {
		var state string
		return db.QueryRowContext(t.Context(), "SELECT state FROM rendition_jobs").Scan(&state) == nil && state == "retry_wait"
	}, 10*time.Second, 10*time.Millisecond)
	cancel()
	<-done
	require.ErrorIs(t, runErr, context.Canceled)
	require.Equal(t, int32(1), provider.calls.Load(), "cancellation must not bypass the retry delay")
	require.Zero(t, embedding.calls.Load())
}

type outcomeRenditionProvider struct {
	document.RenditionProvider

	failure error
	calls   atomic.Int32
}

func (provider *outcomeRenditionProvider) Render(ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	if provider.calls.Add(1) == 1 {
		return document.RenditionResult{}, provider.failure
	}
	return provider.RenditionProvider.Render(ctx, upload, authorization)
}

func (provider *waitingRenditionProvider) Render(ctx context.Context, upload document.AuthorizedUpload,
	authorization document.RenditionAuthorization,
) (document.RenditionResult, error) {
	provider.filenames = append(provider.filenames, upload.Metadata().Filename)
	if provider.calls.Add(1) == 1 {
		close(provider.started)
	}
	select {
	case <-ctx.Done():
		return document.RenditionResult{}, ctx.Err()
	case <-provider.release:
		return provider.RenditionProvider.Render(ctx, upload, authorization)
	}
}

func TestEmbeddedProcessingRunsDirectEmbeddingsAndSemanticSearch(t *testing.T) {
	renditionProvider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	embeddingProvider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, renditionProvider.Descriptor())
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(embeddingProvider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: profile, RenditionProvider: renditionProvider,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": embeddingProvider}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/private.txt", strings.NewReader("semantic needle"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "private"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Len(t, job.EmbeddingJobIDs, 1)
	require.Equal(t, []string{""}, embeddingProvider.filenames)
	aggregate, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", aggregate.State)
	require.Equal(t, 1, aggregate.CompletedBindings)
	require.Equal(t, job.EmbeddingJobIDs, aggregate.EmbeddingJobIDs)
	embeddingStatus, err := vault.ProcessingStatus(t.Context(),
		docbank.ProcessingStatusRequest{JobID: job.EmbeddingJobIDs[0]})
	require.NoError(t, err)
	require.Equal(t, "completed", embeddingStatus.State)

	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	coverage, err := vault.DocumentCoverage(t.Context(), docbank.CoverageRequest{Profile: "private", Fence: fence})
	require.NoError(t, err)
	require.Len(t, coverage.Embeddings, 1)
	require.Equal(t, "complete", coverage.Embeddings[0].State)
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchSemantic, Profile: "private", BindingID: "direct", Fence: fence})
	require.NoError(t, err)
	require.Equal(t, docbank.DocumentSearchSemantic, report.ActualMode)
	require.Len(t, report.Results, 1)
	require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
}

func TestEmbeddedProcessingReturnsStatusWhenOptionalEmbeddingFails(t *testing.T) {
	provider := newSyntheticEmbeddingProvider(t)
	provider.failure = errors.New("synthetic provider failure")
	profile := embeddedProcessingProfile(t, plaintextDescriptorForProfile(t))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	binding := syntheticEmbeddingBinding(provider.descriptor)
	binding.Activation = document.EmbeddingOptional
	profile.Embeddings = []document.EmbeddingBindingV1{binding}
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root,
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"optional": {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": provider},
				EmbeddingClassifiers: map[string]docbank.EmbeddingErrorClassifier{
					"direct": func(error) (docbank.EmbeddingFailureClass, time.Duration) {
						return docbank.EmbeddingFailurePermanent, 0
					},
				}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/document.txt", strings.NewReader("synthetic document"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{
		NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "optional"}}
	plan, err := vault.PlanProcessing(t.Context(), request)
	require.NoError(t, err)
	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, "partial", status.State)
	require.NotEmpty(t, status.FailureCode)
	require.NoError(t, vault.Close())
	vault, err = docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	reopened, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, status, reopened, "status must use the stored profile when providers are no longer configured")
}

func TestEmbeddedProcessingJoinsRunningEmbedding(t *testing.T) {
	provider := newSyntheticEmbeddingProvider(t)
	provider.started, provider.release = make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(provider.release) })
	profile := embeddedProcessingProfile(t, plaintextDescriptorForProfile(t))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(provider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"shared": {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": provider}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/shared.txt", strings.NewReader("shared embedding needle"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	planRequest := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{
		NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "shared"}}
	plan, err := vault.PlanProcessing(t.Context(), planRequest)
	require.NoError(t, err)
	request := docbank.StartProcessingRequest{PlanRequest: planRequest, PlanFingerprint: plan.Fingerprint, Consent: true}
	type result struct {
		job docbank.ProcessingJob
		err error
	}
	first, joined := make(chan result, 1), make(chan result, 1)
	var callers sync.WaitGroup
	callers.Go(func() { job, err := vault.StartProcessing(t.Context(), request); first <- result{job, err} })
	t.Cleanup(func() { release(); callers.Wait() })
	select {
	case <-provider.started:
	case result := <-first:
		t.Fatalf("processing returned before embedding: %v", result.err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = vault.StartProcessing(ctx, request)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	callers.Go(func() { job, err := vault.StartProcessing(t.Context(), request); joined <- result{job, err} })
	select {
	case result := <-joined:
		t.Fatalf("joining caller returned before shared embedding finished: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	owner, waiter := <-first, <-joined
	require.NoError(t, owner.err)
	require.NoError(t, waiter.err)
	require.Equal(t, owner.job.ID, waiter.job.ID)
	require.Equal(t, int32(1), provider.calls.Load())
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: waiter.job.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
}

func TestEmbeddedProcessingUsesEachBindingClassifier(t *testing.T) {
	provider := newSyntheticEmbeddingProvider(t)
	provider.failure = errors.New("synthetic provider failure")
	profiles := make(map[string]docbank.ProcessingProfileConfig)
	classifications := []docbank.EmbeddingFailureClass{docbank.EmbeddingFailurePermanent, docbank.EmbeddingFailureTransient}
	for index, classification := range classifications {
		profile := embeddedProcessingProfile(t, plaintextDescriptorForProfile(t))
		profile.Rendition = nil
		profile.RetentionDisclosure.RetainSanitizedMarkdown = false
		profile.Retrieval.LexicalLimit = index + 1
		profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(provider.descriptor)}
		profiles[fmt.Sprintf("profile-%d", index)] = docbank.ProcessingProfileConfig{Profile: profile,
			EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": provider},
			EmbeddingClassifiers: map[string]docbank.EmbeddingErrorClassifier{
				"direct": func(error) (docbank.EmbeddingFailureClass, time.Duration) { return classification, 0 },
			}}
	}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: profiles}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/classifiers.txt", strings.NewReader("classifier source"), docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	for index, classification := range classifications {
		request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID,
			ContentVersionID: receipt.Version.ID, Profile: fmt.Sprintf("profile-%d", index)}}
		plan, err := vault.PlanProcessing(t.Context(), request)
		require.NoError(t, err)
		job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
		status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
		require.NoError(t, err)
		want := "input_rejected"
		if classification == docbank.EmbeddingFailureTransient {
			want = "provider_unavailable"
		}
		require.Equal(t, want, status.FailureCode)
	}
}

func TestEmbeddedProcessingIndexesConcurrentDocuments(t *testing.T) {
	provider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, plaintextDescriptorForProfile(t))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(provider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"concurrent": {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": provider}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	var versions []string
	var requests []docbank.StartProcessingRequest
	for index := range 4 {
		receipt, err := vault.Put(t.Context(), fmt.Sprintf("/concurrent-%d.txt", index), strings.NewReader("concurrent needle"),
			docbank.PutOptions{MediaType: "text/plain"})
		require.NoError(t, err)
		request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{
			NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "concurrent"}}
		plan, err := vault.PlanProcessing(t.Context(), request)
		require.NoError(t, err)
		requests = append(requests, docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
		versions = append(versions, receipt.Version.ID)
	}
	start := make(chan struct{})
	results := make(chan error, len(requests))
	var callers sync.WaitGroup
	for _, request := range requests {
		callers.Go(func() {
			<-start
			_, err := vault.StartProcessing(t.Context(), request)
			results <- err
		})
	}
	close(start)
	callers.Wait()
	for range requests {
		require.NoError(t, <-results)
	}
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "needle", Profile: "concurrent",
		Mode: docbank.DocumentSearchSemantic, Fence: docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: versions}})
	require.NoError(t, err)
	require.Len(t, report.Results, len(requests))
}

func TestEmbeddedProcessingEnforcesProfileSearchLimits(t *testing.T) {
	provider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, plaintextDescriptorForProfile(t))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(provider.descriptor)}
	profile.Retrieval = document.RetrievalPolicyV1{LexicalLimit: 1, VectorLimit: 2}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"bounded": {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": provider}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	var versions []string
	for index := range 3 {
		receipt, err := vault.Put(t.Context(), fmt.Sprintf("/needle-%d.txt", index), strings.NewReader("semantic needle"),
			docbank.PutOptions{MediaType: "text/plain"})
		require.NoError(t, err)
		request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID,
			ContentVersionID: receipt.Version.ID, Profile: "bounded"}}
		plan, err := vault.PlanProcessing(t.Context(), request)
		require.NoError(t, err)
		_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
		require.NoError(t, err)
		versions = append(versions, receipt.Version.ID)
	}
	for _, test := range []struct {
		mode        docbank.DocumentSearchMode
		limit, want int
	}{
		{docbank.DocumentSearchLexical, 10, 1},
		{docbank.DocumentSearchSemantic, 10, 2},
		{docbank.DocumentSearchHybrid, 1, 1},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "needle", Profile: "bounded",
				Mode: test.mode, Limit: test.limit, Fence: docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: versions}})
			require.NoError(t, err)
			require.Len(t, report.Results, test.want)
			require.True(t, report.Truncated)
		})
	}
}

func TestEmbeddedProcessingBuildsChunkEmbeddingsFromNormalizedEvidence(t *testing.T) {
	renditionProvider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	embeddingProvider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, renditionProvider.Descriptor())
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticChunkEmbeddingBinding(embeddingProvider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"private": {Profile: profile, RenditionProvider: renditionProvider,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"chunks": embeddingProvider},
				Tokenizers:         map[string]document.Tokenizer{"chunks": syntheticRuneTokenizer{}}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/private.txt", strings.NewReader("chunk semantic needle"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "private"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchSemantic, Profile: "private", BindingID: "chunks", Fence: fence})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	require.Equal(t, receipt.Version.ID, report.Results[0].ContentVersionID)
	require.Equal(t, "rendition_chunk", report.Results[0].Evidence[0].InputKind)
}

func TestEmbeddedProcessingSupportsDirectEmbeddingWithoutRenditionProvider(t *testing.T) {
	embeddingProvider := newSyntheticEmbeddingProvider(t)
	profile := embeddedProcessingProfile(t, plaintextDescriptorForProfile(t))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(embeddingProvider.descriptor)}
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(),
		Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
			"direct": {Profile: profile,
				EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": embeddingProvider}},
		}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/private.txt", strings.NewReader("direct-only needle"),
		docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID,
		ContentVersionID: receipt.Version.ID, Profile: "direct"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	require.Len(t, plan.Flow, 2)
	require.Equal(t, "embedding", plan.Flow[0].Capability)
	require.Contains(t, plan.DisclosedClasses, "query_text")
	require.NotContains(t, plan.RetainedClasses, "normalized_evidence")
	job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	require.Empty(t, job.RenditionJobID)
	require.Equal(t, job.EmbeddingJobIDs[0], job.ID)
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
	require.Equal(t, 1, status.CompletedBindings)
	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}
	report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "needle", Mode: docbank.DocumentSearchSemantic, Profile: "direct", BindingID: "direct", Fence: fence})
	require.NoError(t, err)
	require.Len(t, report.Results, 1)
}

func TestEmbeddedProcessingRejectsConflictingProvidersForOneDescriptor(t *testing.T) {
	renditionProvider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	first := newSyntheticEmbeddingProvider(t)
	second := newSyntheticEmbeddingProvider(t)
	require.Equal(t, first.descriptor.Fingerprint, second.descriptor.Fingerprint)
	profile := embeddedProcessingProfile(t, renditionProvider.Descriptor())
	profile.Embeddings = []document.EmbeddingBindingV1{syntheticEmbeddingBinding(first.descriptor)}
	config := func(provider document.EmbeddingProvider) docbank.Config {
		return docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
			Profiles: map[string]docbank.ProcessingProfileConfig{
				"one": {Profile: profile, RenditionProvider: renditionProvider,
					EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": first}},
				"two": {Profile: profile, RenditionProvider: renditionProvider,
					EmbeddingProviders: map[string]document.EmbeddingProvider{"direct": provider}},
			}}}
	}
	_, err = docbank.New(t.Context(), config(second))
	require.ErrorContains(t, err, "conflicts with another profile's provider")

	vault, err := docbank.New(t.Context(), config(first))
	require.NoError(t, err)
	require.NoError(t, vault.Close())
}

func plaintextDescriptorForProfile(t *testing.T) document.RenditionDescriptor {
	t.Helper()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	return provider.Descriptor()
}

type syntheticEmbeddingProvider struct {
	descriptor       document.EmbeddingDescriptor
	filenames        []string
	filenamesMu      sync.Mutex
	failure          error
	started, release chan struct{}
	calls            atomic.Int32
}

func newSyntheticEmbeddingProvider(t *testing.T) *syntheticEmbeddingProvider {
	t.Helper()
	contract, err := document.NewModelInputContract(document.ModelInputContractConfig{
		Profile: document.ModelInputProfileCustom, CompatibilityID: "synthetic-direct/v1",
		Document: document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "document: {{content}}"},
		Query:    document.ModelInputEncoder{Mode: document.ModelInputModeText, Template: "query: {{content}}"},
	})
	require.NoError(t, err)
	descriptor, err := document.NewEmbeddingDescriptor(document.EmbeddingDescriptor{
		ID: "synthetic-direct", ContractVersion: document.EmbeddingProviderContractVersion,
		PolicyFingerprint: embeddedHash("synthetic-embedding-policy"),
		TrustBoundary:     document.EmbeddingTrustLocalProcess, Model: "synthetic", ModelRevision: "v1",
		Dimension: 2, Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationUnitLength, ScalarEncoding: "float32",
		DocumentFormatter: "document/v1", QueryFormatter: "query/v1",
		InputKinds: []document.EmbeddingInputKind{document.EmbeddingInputOriginalFile,
			document.EmbeddingInputRenditionChunk},
		CompatibilityID: contract.CompatibilityID, SupportsTextQuery: true, ModelInput: contract,
		SupportedRequestModes: []document.ModelInputMode{document.ModelInputModeText},
	})
	require.NoError(t, err)
	return &syntheticEmbeddingProvider{descriptor: descriptor}
}

func (provider *syntheticEmbeddingProvider) Descriptor() document.EmbeddingDescriptor {
	return provider.descriptor
}

func (provider *syntheticEmbeddingProvider) Embed(ctx context.Context, inputs []document.EmbeddingInput,
	_ document.EmbeddingAuthorization,
) (document.EmbeddingResult, error) {
	if provider.calls.Add(1) == 1 && provider.started != nil {
		close(provider.started)
	}
	if provider.release != nil {
		select {
		case <-ctx.Done():
			return document.EmbeddingResult{}, ctx.Err()
		case <-provider.release:
		}
	}
	if provider.failure != nil {
		return document.EmbeddingResult{}, provider.failure
	}
	vectors := make([]document.EmbeddingVector, len(inputs))
	for index, input := range inputs {
		text := input.Text
		if input.Source != nil {
			provider.filenamesMu.Lock()
			provider.filenames = append(provider.filenames, input.Source.Metadata().Filename)
			provider.filenamesMu.Unlock()
			body, err := io.ReadAll(input.Source)
			if err != nil {
				return document.EmbeddingResult{}, err
			}
			text = string(body)
		}
		vector := []float32{0, 1}
		if strings.Contains(text, "needle") {
			vector = []float32{1, 0}
		}
		vectors[index] = document.EmbeddingVector{Key: input.Key, Values: vector}
	}
	return document.EmbeddingResult{Vectors: vectors}, nil
}

func syntheticEmbeddingBinding(descriptor document.EmbeddingDescriptor) document.EmbeddingBindingV1 {
	return document.EmbeddingBindingV1{Activation: document.EmbeddingRequired,
		AuthorizationFingerprint: embeddedHash("embedding-authorization"),
		CompatibilityID:          descriptor.CompatibilityID, CredentialBinding: "credential:none",
		Descriptor: document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
		Dimensions: descriptor.Dimension, DisclosureFingerprint: embeddedHash("embedding-disclosure"),
		DocumentFormatter: descriptor.DocumentFormatter, InputKind: document.EmbeddingInputOriginalFile,
		MaxBatchItems: 8, MaxInputBytes: 1 << 20, MaxResponseBytes: 1 << 20,
		Metric: descriptor.Metric, ModelInput: descriptor.ModelInput, Model: descriptor.Model, Name: "direct",
		Normalization: descriptor.Normalization, QueryFormatter: descriptor.QueryFormatter,
		ScalarEncoding: descriptor.ScalarEncoding, TrustBoundary: string(descriptor.TrustBoundary)}
}

func syntheticChunkEmbeddingBinding(descriptor document.EmbeddingDescriptor) document.EmbeddingBindingV1 {
	binding := syntheticEmbeddingBinding(descriptor)
	binding.Name = "chunks"
	binding.InputKind = document.EmbeddingInputRenditionChunk
	binding.MaxInputTokens = 1_000_000
	binding.Chunk = &document.EmbeddingChunkPolicyV1{ContextFingerprint: embeddedHash("chunk-context"),
		Formatter: "rendition-chunk/v1", MaxTokens: 64, OverlapTokens: 8,
		Tokenizer: "synthetic-runes", TokenizerRevision: "v1", TruncationPolicy: document.TruncationPolicyReject}
	return binding
}

type syntheticRuneTokenizer struct{}

func (syntheticRuneTokenizer) Identity() document.TokenizerIdentity {
	return document.TokenizerIdentity{Name: "synthetic-runes", Revision: "v1"}
}

func (syntheticRuneTokenizer) PrefixTokenCountsMonotonic() bool { return true }

func (syntheticRuneTokenizer) Tokenize(text string, limit int) ([]document.TokenBoundary, error) {
	runes := []rune(text)
	if len(runes) > limit {
		return nil, document.ErrTokenizerLimit
	}
	result := make([]document.TokenBoundary, len(runes))
	for index := range runes {
		result[index] = document.TokenBoundary{Start: index, End: index + 1}
	}
	return result, nil
}

func embeddedHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func embeddedProcessingProfile(t *testing.T, descriptor document.RenditionDescriptor) document.ProcessingProfileV1 {
	t.Helper()
	hash := func(value string) string {
		digest := sha256.Sum256([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	return document.ProcessingProfileV1{
		ContractVersion: document.ProcessingProfileContractV1,
		Rendition: &document.RenditionBindingV1{
			AdapterContract: "plaintext.in-process/v1", AuthorizationFingerprint: hash("authorization"),
			CredentialBinding: "credential:none", DeploymentFingerprint: hash("deployment"),
			Descriptor:            document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint},
			DisclosureFingerprint: hash("rendition-disclosure"), MaxDocumentBytes: 1 << 20,
			MaxResponseBytes: 1 << 20, MaxUnits: 1000, Name: "plaintext",
			RequestedArtifacts: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
			TrustBoundary:      string(descriptor.TrustBoundary), UploadOptionsFingerprint: hash("upload"),
		},
		EvidenceLexical: document.EvidenceLexicalPolicyV1{
			CompletenessFingerprint: hash("completeness"), LexicalSegmenterFingerprint: hash("segments"),
			MaxDocumentChars: 1_000_000, MaxSegmentRunes: 1000, MaxUnitRunes: 100_000,
			NormalizedEvidenceContract: document.NormalizedEvidenceContractV1,
			NormalizerFingerprint:      hash("normalizer"), RenditionContract: document.RenditionContractV1,
			SanitizerFingerprint: hash("sanitizer"), SourceEvidenceContract: document.SourceEvidenceContractV1,
		},
		RetentionDisclosure: document.RetentionDisclosurePolicyV1{
			AttachmentPolicyFingerprint: hash("attachment"), ConsentFingerprint: hash("consent"),
			RetainSanitizedMarkdown: true, TrustBoundary: string(descriptor.TrustBoundary),
		},
		Retrieval: document.RetrievalPolicyV1{LexicalLimit: 50, VectorLimit: 50},
	}
}

func createRangeFixture(
	t *testing.T, vault *docbank.Vault, virtualPath string, content []byte,
) docbank.PutReceipt {
	t.Helper()
	sum := sha256.Sum256(content)
	receipt, err := vault.Create(
		t.Context(), virtualPath, bytes.NewReader(content),
		docbank.CreateOptions{
			MediaType: "application/octet-stream",
			Expected: docbank.ContentIdentity{
				SHA256: hex.EncodeToString(sum[:]),
				Size:   int64(len(content)),
			},
		},
	)
	require.NoError(t, err)
	return receipt
}

func TestOpenVersionContentRangeRejectsInvalidSlices(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(
		t, vault, "/ranges/value.bin", []byte("0123456789"),
	)

	cases := []docbank.ContentRangeOptions{
		{Offset: -1, Length: 1},
		{Offset: 0, Length: 0},
		{Offset: 0, Length: -1},
		{Offset: 10, Length: 1},
		{Offset: 9, Length: 2},
		{Offset: math.MaxInt64, Length: math.MaxInt64},
	}
	for _, opts := range cases {
		_, err := vault.OpenVersionContentRange(t.Context(), receipt.Version.ID, opts)
		require.ErrorIs(t, err, docbank.ErrInvalidContentRange)
	}
}

func TestOpenVersionContentRangeMissingVersion(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	_, err = vault.OpenVersionContentRange(
		t.Context(), "00000000-0000-4000-8000-000000000000",
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.ErrorIs(t, err, docbank.ErrNotFound)
}

func TestOpenVersionContentRangeRawLoose(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(
		t, vault, "/ranges/raw.bin", []byte("0123456789"),
	)

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 2, Length: 4},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, []byte("2345"), body)
	require.Equal(t, receipt.Version, got.Version)
	require.Equal(t, int64(2), got.Offset)
	require.Equal(t, int64(4), got.Length)
}

func TestOpenVersionContentRangeHistoricalVersion(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	path := "/ranges/history.bin"
	first := createRangeFixture(t, vault, path, []byte("abcdefghij"))
	_, err = vault.Put(
		t.Context(), path, bytes.NewReader([]byte("ABCDEFGHIJ")),
		docbank.PutOptions{MediaType: "application/octet-stream"},
	)
	require.NoError(t, err)

	got, err := vault.OpenVersionContentRange(
		t.Context(), first.Version.ID,
		docbank.ContentRangeOptions{Offset: 1, Length: 3},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, []byte("bcd"), body)
	require.Equal(t, first.Version.ID, got.Version.ID)
}

func TestOpenVersionContentRangeCompressedLoose(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{
		Root: t.TempDir(),
		LooseCompression: docbank.LooseCompressionOptions{
			Enabled: true, MinBytes: 1, MinSavingsPercent: 0,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte(strings.Repeat("compressed logical range\n", 128))
	receipt := createRangeFixture(t, vault, "/ranges/compressed.bin", content)
	require.Equal(t, "loose", receipt.Physical.Kind)
	require.Equal(t, "zstd", receipt.Physical.Encoding)

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 11, Length: 17},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, content[11:28], body)
}

func TestOpenVersionContentRangePacked(t *testing.T) {
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	content := []byte("packed logical range content")
	receipt := createRangeFixture(t, vault, "/ranges/packed.bin", content)
	report, err := vault.Pack(t.Context(), docbank.PackOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.BlobsPacked)
	require.NoFileExists(t, looseBlobPath(root, receipt.Computed.SHA256))

	got, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 7, Length: 7},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, got.Reader.Close()) })
	body, err := io.ReadAll(got.Reader)
	require.NoError(t, err)
	require.Equal(t, content[7:14], body)
}

func TestOpenVersionContentRangeUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing authority",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			content := []byte("physical authority bytes")
			receipt := createRangeFixture(t, vault, "/ranges/unavailable.bin", content)
			test.corrupt(t, looseBlobPath(root, receipt.Computed.SHA256))

			_, err = vault.OpenVersionContentRange(
				t.Context(), receipt.Version.ID,
				docbank.ContentRangeOptions{Offset: 0, Length: 1},
			)
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestOpenVersionContentRangeHoldsVaultLease(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt := createRangeFixture(t, vault, "/ranges/lease.bin", []byte("lease bytes"))
	opened, err := vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, opened.Reader.Close()) })

	closeDone := make(chan error, 1)
	go func() { closeDone <- vault.Close() }()
	select {
	case err := <-closeDone:
		require.FailNow(t, "vault closed while a range held its lifecycle lease", "error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	require.NoError(t, opened.Reader.Close())
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "vault did not close after the range released its lease")
	}
}

func TestOpenVersionContentRangeClosedVault(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	receipt := createRangeFixture(t, vault, "/ranges/closed.bin", []byte("closed bytes"))
	require.NoError(t, vault.Close())

	_, err = vault.OpenVersionContentRange(
		t.Context(), receipt.Version.ID,
		docbank.ContentRangeOptions{Offset: 0, Length: 1},
	)
	require.ErrorIs(t, err, docbank.ErrClosed)
}

func TestEmbeddedImmutableCreate(t *testing.T) {
	content := []byte("immutable external content\n")
	sum := sha256.Sum256(content)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	receipt, err := vault.Create(t.Context(), "/external.txt", bytes.NewReader(content), docbank.CreateOptions{
		MediaType: "text/plain",
		Expected:  docbank.ContentIdentity{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))},
	})
	require.NoError(t, err)
	require.True(t, receipt.Created)
}

func TestEmbeddedPutRequiresCurrentRevision(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/external.txt", strings.NewReader("first\n"),
		docbank.PutOptions{MediaType: "text/plain"},
	)
	require.NoError(t, err)

	unchanged, err := vault.Put(
		t.Context(), "/external.txt", strings.NewReader("first\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: created.Node.Revision},
	)
	require.NoError(t, err)
	require.Equal(t, created.Version.ID, unchanged.Version.ID)
	require.Equal(t, created.Node.Revision, unchanged.Node.Revision)

	replaced, err := vault.Put(
		t.Context(), "/external.txt", strings.NewReader("second\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: created.Node.Revision},
	)
	require.NoError(t, err)
	require.NotEqual(t, created.Version.ID, replaced.Version.ID)

	_, err = vault.Put(
		t.Context(), "/external.txt", strings.NewReader("third\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: created.Node.Revision},
	)
	require.ErrorIs(t, err, docbank.ErrStaleRevision)

	current, err := vault.Stat(t.Context(), "/external.txt")
	require.NoError(t, err)
	require.Equal(t, replaced.Version.ID, current.CurrentVersionID)

	_, err = vault.Put(
		t.Context(), "/external.txt", strings.NewReader("invalid\n"),
		docbank.PutOptions{MediaType: "text/plain", IfRevision: -1},
	)
	require.Error(t, err)
}

func TestVaultMoveTrashRestoreExternalAPI(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/inbox/report.txt", strings.NewReader("report\n"), docbank.PutOptions{},
	)
	require.NoError(t, err)

	moved, err := vault.MovePath(t.Context(), "/inbox/report.txt", "/archive.txt", docbank.RevisionOptions{
		IfRevision: created.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, created.Node.ID, moved.Node.ID)
	require.Equal(t, created.Node.Revision+1, moved.Node.Revision)
	require.Equal(t, "/archive.txt", moved.Path)

	trashed, err := vault.TrashPath(t.Context(), moved.Path, docbank.RevisionOptions{
		IfRevision: moved.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, moved.Path, trashed.Path)
	restored, err := vault.Restore(t.Context(), trashed.Node.ID, docbank.RevisionOptions{
		IfRevision: trashed.Node.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, moved.Path, restored.Path)

	_, err = vault.TrashPath(t.Context(), restored.Path, docbank.RevisionOptions{})
	require.NoError(t, err)
	report, err := vault.EmptyTrash(t.Context(), docbank.TrashEmptyOptions{MaxRoots: 1, DryRun: true})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Candidates)
	require.True(t, report.DryRun)
}

func TestVaultMoveBatchExternalAPI(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	first, err := vault.Put(t.Context(), "/left/first.txt", strings.NewReader("first\n"), docbank.PutOptions{})
	require.NoError(t, err)
	second, err := vault.Put(t.Context(), "/right/second.txt", strings.NewReader("second\n"), docbank.PutOptions{})
	require.NoError(t, err)

	receipts, err := vault.BatchMove(t.Context(), []docbank.BatchMoveItem{
		{SourcePath: "/left/first.txt", DestinationPath: "/right/second.txt"},
		{NodeID: second.Node.ID, IfRevision: second.Node.Revision, DestinationPath: "/left/first.txt"},
	})
	require.NoError(t, err)
	require.Len(t, receipts, 2)
	require.Equal(t, first.Node.ID, receipts[0].Node.ID)
	require.Equal(t, "/left/first.txt", receipts[0].FromPath)
	require.Equal(t, "/right/second.txt", receipts[0].Path)
	require.Equal(t, second.Node.ID, receipts[1].Node.ID)
	require.Equal(t, "/left/first.txt", receipts[1].Path)
}

func TestTreeMutationErrorsAreClassifiableOutsidePackage(t *testing.T) {
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	created, err := vault.Put(
		t.Context(), "/parent/child/document.txt", strings.NewReader("document\n"),
		docbank.PutOptions{},
	)
	require.NoError(t, err)

	_, err = vault.Restore(t.Context(), created.Node.ID, docbank.RevisionOptions{})
	require.ErrorIs(t, err, docbank.ErrNotTrashed)
	_, err = vault.TrashPath(t.Context(), "/", docbank.RevisionOptions{})
	require.ErrorIs(t, err, docbank.ErrIsRoot)
	_, err = vault.MovePath(
		t.Context(), "/parent/child/document.txt", "/parent/../document.txt",
		docbank.RevisionOptions{},
	)
	require.ErrorIs(t, err, docbank.ErrInvalidName)
	_, err = vault.MovePath(
		t.Context(), "/parent", "/parent/child/parent", docbank.RevisionOptions{},
	)
	require.ErrorIs(t, err, docbank.ErrCycle)

	// Existing audited vaults can surface this sentinel through the same public
	// methods even though first enrollment is currently daemon-owned.
	require.ErrorIs(t, fmt.Errorf("embedded audited mutation: %w", docbank.ErrAuditMutationUnsupported), docbank.ErrAuditMutationUnsupported)
}

func TestOpenContentClassifiesPhysicalContentFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			receipt, err := vault.Put(
				t.Context(), "/notes/current.md", strings.NewReader("current bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			test.corrupt(t, looseBlobPath(root, receipt.Computed.SHA256))

			_, err = vault.OpenContent(t.Context(), "/notes/current.md")
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestOpenVersionContentClassifiesPhysicalContentFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "physical size mismatch",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("short"), 0o600))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			first, err := vault.Put(
				t.Context(), "/notes/history.md", strings.NewReader("historical bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			_, err = vault.Put(
				t.Context(), "/notes/history.md", strings.NewReader("current bytes\n"), docbank.PutOptions{},
			)
			require.NoError(t, err)
			test.corrupt(t, looseBlobPath(root, first.Computed.SHA256))

			_, err = vault.OpenVersionContent(t.Context(), first.Version.ID)
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestEnsureSourceMetadataClassifiesPhysicalContentFailures(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, string)
	}{
		{
			name: "missing blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Remove(path))
			},
		},
		{
			name: "corrupt blob",
			corrupt: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0o600))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })

			receipt := createRangeFixture(t, vault, "/source.bin", []byte("source metadata bytes"))
			test.corrupt(t, looseBlobPath(root, receipt.Computed.SHA256))

			_, err = vault.EnsureSourceMetadata(t.Context(), receipt.Version.ID)
			require.ErrorIs(t, err, docbank.ErrContentUnavailable)
		})
	}
}

func TestContentMetadataErrorsRemainDistinctFromPhysicalUnavailability(t *testing.T) {
	root := t.TempDir()
	vault, err := docbank.New(t.Context(), docbank.Config{Root: root})
	require.NoError(t, err)

	receipt, err := vault.Put(
		t.Context(), "/notes/entry.md", strings.NewReader("entry\n"), docbank.PutOptions{},
	)
	require.NoError(t, err)

	_, err = vault.OpenContent(t.Context(), "/missing.md")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenContent(t.Context(), "/notes")
	require.ErrorIs(t, err, docbank.ErrNotFile)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenVersionContent(t.Context(), "00000000-0000-4000-8000-000000000000")
	require.ErrorIs(t, err, docbank.ErrNotFound)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	require.NoError(t, vault.Close())

	_, err = vault.OpenContent(t.Context(), "/notes/entry.md")
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)

	_, err = vault.OpenVersionContent(t.Context(), receipt.Version.ID)
	require.ErrorIs(t, err, docbank.ErrClosed)
	require.NotErrorIs(t, err, docbank.ErrContentUnavailable)
}

func looseBlobPath(root, hash string) string {
	return filepath.Join(root, "blobs", hash[:2], hash)
}

func TestEmbeddedProcessingSharesOnlyMatchingRenditionExecution(t *testing.T) {
	for _, disclose := range []bool{false, true} {
		t.Run(strconv.FormatBool(disclose), func(t *testing.T) {
			provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
			require.NoError(t, err)
			profile := embeddedProcessingProfile(t, provider.Descriptor())
			profile.Rendition.DiscloseFilename = disclose
			vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{"test": {Profile: profile, RenditionProvider: provider}}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			var jobs []docbank.ProcessingJob
			for _, name := range []string{"/first.txt", "/second.txt"} {
				receipt, err := vault.Put(t.Context(), name, strings.NewReader("identical synthetic document"), docbank.PutOptions{MediaType: "text/plain"})
				require.NoError(t, err)
				request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "test"}}
				plan, err := vault.PlanProcessing(t.Context(), request)
				require.NoError(t, err)
				job, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
				require.NoError(t, err)
				jobs = append(jobs, job)
			}
			if disclose {
				require.NotEqual(t, jobs[0].RenditionJobID, jobs[1].RenditionJobID)
			} else {
				require.Equal(t, jobs[0].RenditionJobID, jobs[1].RenditionJobID)
			}
		})
	}
}

func TestEmbeddedProcessingRejectsMismatchedProviderBoundaries(t *testing.T) {
	for _, kind := range []string{"rendition", "embedding"} {
		for _, boundary := range []string{"local_process", "hosted_provider", "unknown"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
				require.NoError(t, err)
				profile := embeddedProcessingProfile(t, rendition.Descriptor())
				config := docbank.ProcessingProfileConfig{Profile: profile, RenditionProvider: rendition}
				if kind == "rendition" {
					config.Profile.Rendition.TrustBoundary = boundary
				} else {
					embedding := newSyntheticEmbeddingProvider(t)
					binding := syntheticEmbeddingBinding(embedding.Descriptor())
					binding.TrustBoundary = boundary
					config.Profile.Embeddings = []document.EmbeddingBindingV1{binding}
					config.EmbeddingProviders = map[string]document.EmbeddingProvider{"direct": embedding}
				}
				vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{"test": config}}})
				if vault != nil {
					t.Cleanup(func() { require.NoError(t, vault.Close()) })
				}
				if boundary == "local_process" {
					require.NoError(t, err)
				} else {
					require.Error(t, err, "configuration must reject a misleading processing boundary")
				}
			})
		}
	}
}

func TestEmbeddedProcessingStatusExcludesHistoricalChunkJobs(t *testing.T) {
	for _, initialFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(initialFailure), func(t *testing.T) {
			rendition, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
			require.NoError(t, err)
			embedding := newSyntheticEmbeddingProvider(t)
			if initialFailure {
				embedding.failure = errors.New("synthetic permanent provider failure")
			}
			profile := embeddedProcessingProfile(t, rendition.Descriptor())
			profile.Rendition.DiscloseFilename = true
			profile.Embeddings = []document.EmbeddingBindingV1{syntheticChunkEmbeddingBinding(embedding.descriptor)}
			vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
				"test": {Profile: profile, RenditionProvider: rendition, EmbeddingProviders: map[string]document.EmbeddingProvider{"chunks": embedding}, Tokenizers: map[string]document.Tokenizer{"chunks": syntheticRuneTokenizer{}}, EmbeddingClassifiers: map[string]docbank.EmbeddingErrorClassifier{"chunks": func(error) (docbank.EmbeddingFailureClass, time.Duration) {
					return docbank.EmbeddingFailurePermanent, 0
				}}},
			}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			receipt, err := vault.Put(t.Context(), "/before.txt", strings.NewReader("synthetic chunk semantic needle"), docbank.PutOptions{MediaType: "text/plain"})
			require.NoError(t, err)
			request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "test"}}
			plan, err := vault.PlanProcessing(t.Context(), request)
			require.NoError(t, err)
			first, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
			require.NoError(t, err)
			before, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: first.ID})
			require.NoError(t, err)
			if initialFailure {
				require.Equal(t, "failed", before.State)
			} else {
				require.Equal(t, "completed", before.State)
			}
			renamed, err := vault.MovePath(t.Context(), "/before.txt", "/after.txt", docbank.RevisionOptions{})
			require.NoError(t, err)
			require.Equal(t, receipt.Version.ID, renamed.Node.CurrentVersionID)
			embedding.failure = nil
			plan, err = vault.PlanProcessing(t.Context(), request)
			require.NoError(t, err)
			second, err := vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: plan.Fingerprint, Consent: true})
			require.NoError(t, err)
			after, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: second.ID})
			require.NoError(t, err)
			require.NotEqual(t, first.RenditionJobID, second.RenditionJobID)
			require.NotEqual(t, first.EmbeddingJobIDs, second.EmbeddingJobIDs)
			report, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{Query: "needle", Mode: docbank.DocumentSearchSemantic, Profile: "test", BindingID: "chunks", Fence: docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.Version.ID}}})
			require.NoError(t, err)
			require.Len(t, report.Results, 1, "the current embedding is published and searchable")
			require.Equal(t, second.EmbeddingJobIDs, after.EmbeddingJobIDs)
			require.Equal(t, "completed", after.State)
			require.Equal(t, 1, after.CompletedBindings)
			require.Empty(t, after.FailureCode)
		})
	}
}

func TestEmbeddedProcessingRejectsRenamedConsent(t *testing.T) {
	for _, disclose := range []bool{false, true} {
		t.Run(strconv.FormatBool(disclose), func(t *testing.T) {
			base, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
			require.NoError(t, err)
			provider := &waitingRenditionProvider{RenditionProvider: base, started: make(chan struct{}), release: make(chan struct{})}
			close(provider.release)
			profile := embeddedProcessingProfile(t, provider.Descriptor())
			profile.Rendition.DiscloseFilename = disclose
			vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{"test": {Profile: profile, RenditionProvider: provider}}}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			receipt, err := vault.Put(t.Context(), "/before.txt", strings.NewReader("synthetic consent source"), docbank.PutOptions{MediaType: "text/plain"})
			require.NoError(t, err)
			request := docbank.ProcessingPlanRequest{Selector: docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "test"}}
			before, err := vault.PlanProcessing(t.Context(), request)
			require.NoError(t, err)
			_, err = vault.MovePath(t.Context(), "/before.txt", "/after.txt", docbank.RevisionOptions{})
			require.NoError(t, err)
			after, err := vault.PlanProcessing(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, disclose, before.Flow[0].DiscloseFilename)
			if disclose {
				require.Equal(t, "before.txt", before.Flow[0].Filename)
				require.Equal(t, "after.txt", after.Flow[0].Filename)
				require.Contains(t, before.DisclosedClasses, "filename")
				require.NotEqual(t, before.Fingerprint, after.Fingerprint)
				_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: before.Fingerprint, Consent: true})
				require.ErrorIs(t, err, docbank.ErrProcessingPlanChanged)
				require.Zero(t, provider.calls.Load())
			} else {
				require.Empty(t, before.Flow[0].Filename)
				require.NotContains(t, before.DisclosedClasses, "filename")
				require.Equal(t, before.Fingerprint, after.Fingerprint)
			}
			_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: request, PlanFingerprint: after.Fingerprint, Consent: true})
			require.NoError(t, err)
			if disclose {
				require.Equal(t, []string{"after.txt"}, provider.filenames)
			} else {
				require.Equal(t, []string{""}, provider.filenames)
			}
		})
	}
}
