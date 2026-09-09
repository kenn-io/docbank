package docbank_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	docbank "go.kenn.io/docbank"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/suppliedtranscript"
)

func TestEmbeddedSuppliedAudioTranscriptRoute(t *testing.T) {
	source := newGatedTranscriptSource()
	provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
		Source: source, ProcessingProfile: suppliedAudioProfileTemplate(t),
	})
	require.NoError(t, err)
	profile := suppliedAudioProfileForProvider(t, provider)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
		Profiles: map[string]docbank.ProcessingProfileConfig{
			"audio": {Profile: profile, RenditionProvider: provider},
		},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	audio := mediatest.WAV()
	first, err := vault.Put(t.Context(), "/voice/first.wav", bytes.NewReader(audio), docbank.PutOptions{MediaType: "audio/wav"})
	require.NoError(t, err)
	firstText := "The shipment arrives at dock seven."
	source.Set(first.Version.BlobHash, document.SuppliedTranscript{Provider: "beeper", Text: firstText})
	selector := docbank.ProcessingSelector{NodeID: first.Node.ID, ContentVersionID: first.Version.ID, Profile: "audio"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)

	submitCtx, cancelSubmit := context.WithCancel(t.Context())
	job, err := vault.SubmitProcessing(submitCtx, docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	cancelSubmit()
	select {
	case <-source.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("transcript resolver was not reached after durable acknowledgement")
	}
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: job.ID})
	require.NoError(t, err)
	assert.Equal(t, job.ID, status.JobID)
	assert.Contains(t, []string{"queued", "running"}, status.State)
	concurrent := make(chan struct {
		job docbank.ProcessingJob
		err error
	}, 1)
	go func() {
		accepted, submitErr := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
			PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
		})
		concurrent <- struct {
			job docbank.ProcessingJob
			err error
		}{job: accepted, err: submitErr}
	}()
	select {
	case result := <-concurrent:
		require.NoError(t, result.err)
		assert.Equal(t, job.RenditionJobID, result.job.RenditionJobID)
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent submission did not receive durable acknowledgement")
	}
	source.Release()
	status = waitForProcessingStatus(t, vault, job.ID, "completed")
	assert.Equal(t, job.ID, status.JobID)

	rendition, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
	require.NoError(t, err)
	body, err := io.ReadAll(rendition.Reader)
	require.NoError(t, err)
	require.NoError(t, rendition.Reader.Close())
	assert.Contains(t, string(body), `The shipment arrives at dock seven\.`)

	fence := docbank.DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{first.Version.ID}}
	search, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "arrives at dock seven", Mode: docbank.DocumentSearchLexical, Profile: "audio", Fence: fence,
	})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	assert.Equal(t, first.Version.ID, search.Results[0].ContentVersionID)
	assert.Contains(t, search.Results[0].Excerpt, "arrives")
	_, err = vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "arrives at dock seven", Mode: docbank.DocumentSearchLexical, Profile: "audio",
		Fence: docbank.DocumentSourceFence{VaultUID: vault.ID()},
	})
	require.ErrorContains(t, err, "source fence must contain between 1")
	_, err = vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "arrives at dock seven", Mode: docbank.DocumentSearchLexical, Profile: "audio",
		Fence: docbank.DocumentSourceFence{VaultUID: "foreign-vault", ContentVersionIDs: []string{first.Version.ID}},
	})
	require.ErrorIs(t, err, docbank.ErrForeignVault)

	repeated, err := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	assert.Equal(t, job.RenditionJobID, repeated.RenditionJobID)

	second, err := vault.Put(t.Context(), "/voice/second.wav", bytes.NewReader(audio), docbank.PutOptions{MediaType: "audio/wav"})
	require.NoError(t, err)
	secondSelector := docbank.ProcessingSelector{NodeID: second.Node.ID, ContentVersionID: second.Version.ID, Profile: "audio"}
	secondPlan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: secondSelector})
	require.NoError(t, err)
	secondJob, err := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: secondSelector}, PlanFingerprint: secondPlan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	waitForProcessingStatus(t, vault, secondJob.ID, "completed")
	assert.NotEqual(t, job.ID, secondJob.ID)
	assert.Equal(t, job.RenditionJobID, secondJob.RenditionJobID)
	assert.Equal(t, 1, source.Calls())

	fenced, err := vault.SearchDocuments(t.Context(), docbank.DocumentSearchRequest{
		Query: "arrives at dock seven", Mode: docbank.DocumentSearchLexical, Profile: "audio",
		Fence: fence,
	})
	require.NoError(t, err)
	require.Len(t, fenced.Results, 1)
	assert.Equal(t, first.Version.ID, fenced.Results[0].ContentVersionID)
	assert.NotEqual(t, second.Version.ID, fenced.Results[0].ContentVersionID)

	replacement := append([]byte(nil), audio...)
	replacement[len(replacement)-1]++
	updated, err := vault.Put(t.Context(), "/voice/first.wav", bytes.NewReader(replacement), docbank.PutOptions{MediaType: "audio/wav"})
	require.NoError(t, err)
	require.True(t, updated.Replaced)
	updatedText := "The replacement recording arrives at dock nine."
	source.Set(updated.Version.BlobHash, document.SuppliedTranscript{Provider: "beeper", Text: updatedText})
	updatedSelector := docbank.ProcessingSelector{NodeID: updated.Node.ID, ContentVersionID: updated.Version.ID, Profile: "audio"}
	updatedPlan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: updatedSelector})
	require.NoError(t, err)
	updatedJob, err := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: updatedSelector}, PlanFingerprint: updatedPlan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	waitForProcessingStatus(t, vault, updatedJob.ID, "completed")
	versions, err := vault.Versions(t.Context(), first.Node.ID, docbank.VersionsOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, versions.Total)
	assert.Contains(t, versionIDs(versions.Items), first.Version.ID)
	assert.Contains(t, versionIDs(versions.Items), updated.Version.ID)
}

func TestEmbeddedSuppliedAudioRejectsMismatchedEvidencePolicy(t *testing.T) {
	called := false
	provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
		Source: lookupTranscriptSource{lookup: func(string) (document.SuppliedTranscript, error) {
			called = true
			return document.SuppliedTranscript{Provider: "beeper", Text: "transcript"}, nil
		}},
		ProcessingProfile: suppliedAudioProfileTemplate(t),
	})
	require.NoError(t, err)
	profile := suppliedAudioProfileForProvider(t, provider)
	profile.EvidenceLexical.MaxUnitRunes--

	_, err = docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
		Profiles: map[string]docbank.ProcessingProfileConfig{
			"audio": {Profile: profile, RenditionProvider: provider},
		},
	}})
	require.ErrorContains(t, err, "evidence policy")
	assert.False(t, called)
}

func TestEmbeddedSuppliedAudioSubmitCloseWaitsForAcceptedWork(t *testing.T) {
	source := newGatedTranscriptSource()
	provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
		Source: source, ProcessingProfile: suppliedAudioProfileTemplate(t),
	})
	require.NoError(t, err)
	profile := suppliedAudioProfileForProvider(t, provider)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
		Profiles: map[string]docbank.ProcessingProfileConfig{"audio": {Profile: profile, RenditionProvider: provider}},
	}})
	require.NoError(t, err)

	receipt, err := vault.Put(t.Context(), "/voice.wav", bytes.NewReader(mediatest.WAV()), docbank.PutOptions{MediaType: "audio/wav"})
	require.NoError(t, err)
	source.Set(receipt.Version.BlobHash, document.SuppliedTranscript{Provider: "beeper", Text: "held transcript"})
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "audio"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
		PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	select {
	case <-source.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("transcript resolver was not reached")
	}

	closed := make(chan error, 1)
	go func() { closed <- vault.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("vault close did not cancel accepted processing")
	}
	select {
	case <-source.Canceled():
	default:
		t.Fatal("vault close returned before the accepted resolver observed cancellation")
	}
}

func TestEmbeddedSuppliedAudioRejectsInvalidInputBeforeProvider(t *testing.T) {
	tests := []struct {
		name, path, mediaType string
		data                  []byte
	}{
		{name: "malformed audio", path: "/voice.wav", mediaType: "audio/wav", data: []byte("not wav")},
		{name: "wrong MIME", path: "/voice.wav", mediaType: "audio/mpeg", data: mediatest.WAV()},
		{name: "extension mismatch", path: "/voice.mp3", mediaType: "audio/wav", data: mediatest.WAV()},
		{name: "unsupported family", path: "/voice.txt", mediaType: "text/plain", data: []byte("transcript")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newGatedTranscriptSource()
			provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
				Source: source, ProcessingProfile: suppliedAudioProfileTemplate(t),
			})
			require.NoError(t, err)
			profile := suppliedAudioProfileForProvider(t, provider)
			vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
				Profiles: map[string]docbank.ProcessingProfileConfig{"audio": {Profile: profile, RenditionProvider: provider}},
			}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			receipt, err := vault.Put(t.Context(), test.path, bytes.NewReader(test.data), docbank.PutOptions{MediaType: test.mediaType})
			require.NoError(t, err)
			selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "audio"}
			plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
			require.NoError(t, err)
			job, err := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
				PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
			})
			if test.name == "unsupported family" {
				require.NoError(t, err)
				jobStatus := waitForProcessingStatus(t, vault, job.ID, "failed")
				assert.Equal(t, "terminal", jobStatus.FailureCode)
			} else {
				require.Error(t, err)
			}
			select {
			case <-source.Entered():
				t.Fatal("provider ran for invalid input")
			default:
			}
			assert.Equal(t, 0, source.Calls())
		})
	}
}

func TestEmbeddedSuppliedAudioTranscriptClassifiesResolverOutcomes(t *testing.T) {
	t.Run("missing transcript is terminal unsupported input", func(t *testing.T) {
		provider := newTestSuppliedAudioProvider(t, func(string) (document.SuppliedTranscript, error) {
			return document.SuppliedTranscript{}, nil
		})
		vault, selector, plan := newSuppliedAudioVault(t, provider)
		job, err := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
			PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
		})
		require.NoError(t, err)
		status := waitForProcessingStatus(t, vault, job.ID, "failed")
		assert.Equal(t, "terminal", status.FailureCode)
		_, err = vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
		assert.Error(t, err)
	})

	t.Run("retryable resolver error is retried", func(t *testing.T) {
		var calls int
		provider := newTestSuppliedAudioProvider(t, func(_ string) (document.SuppliedTranscript, error) {
			calls++
			if calls == 1 {
				providerErr, err := document.NewRenditionProviderError(document.RenditionErrorTransient, "", 0, errors.New("temporary"))
				if err != nil {
					return document.SuppliedTranscript{}, err
				}
				return document.SuppliedTranscript{}, providerErr
			}
			return document.SuppliedTranscript{Provider: "beeper", Text: "retryable transcript"}, nil
		})
		vault, selector, plan := newSuppliedAudioVault(t, provider)
		job, err := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
			PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
		})
		require.NoError(t, err)
		status := waitForProcessingStatus(t, vault, job.ID, "retry_wait")
		assert.Equal(t, "retry_wait", status.State)
		time.Sleep(1200 * time.Millisecond)
		retryJob, err := vault.SubmitProcessing(t.Context(), docbank.StartProcessingRequest{
			PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
		})
		require.NoError(t, err)
		assert.Equal(t, job.RenditionJobID, retryJob.RenditionJobID)
		status = waitForProcessingStatus(t, vault, retryJob.ID, "completed")
		assert.Equal(t, "completed", status.State)
		assert.GreaterOrEqual(t, calls, 2)
	})
}

type gatedTranscriptSource struct {
	mu         sync.Mutex
	values     map[string]document.SuppliedTranscript
	entered    chan string
	release    chan struct{}
	calls      int
	canceled   chan struct{}
	cancelOnce sync.Once
}

func newGatedTranscriptSource() *gatedTranscriptSource {
	return &gatedTranscriptSource{values: make(map[string]document.SuppliedTranscript), entered: make(chan string, 8), release: make(chan struct{}), canceled: make(chan struct{})}
}

func (source *gatedTranscriptSource) Transcript(ctx context.Context, digest string) (document.SuppliedTranscript, error) {
	source.entered <- digest
	select {
	case <-source.release:
	case <-ctx.Done():
		source.cancelOnce.Do(func() { close(source.canceled) })
		return document.SuppliedTranscript{}, ctx.Err()
	}
	source.mu.Lock()
	source.calls++
	transcript, ok := source.values[digest]
	source.mu.Unlock()
	if !ok {
		return document.SuppliedTranscript{}, nil
	}
	return transcript, nil
}

func (source *gatedTranscriptSource) Set(digest string, transcript document.SuppliedTranscript) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.values[digest] = transcript
}

func (source *gatedTranscriptSource) Entered() <-chan string    { return source.entered }
func (source *gatedTranscriptSource) Canceled() <-chan struct{} { return source.canceled }
func (source *gatedTranscriptSource) Release()                  { close(source.release) }
func (source *gatedTranscriptSource) Calls() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls
}

type lookupTranscriptSource struct {
	lookup func(string) (document.SuppliedTranscript, error)
}

func (source lookupTranscriptSource) Transcript(_ context.Context, digest string) (document.SuppliedTranscript, error) {
	return source.lookup(digest)
}

func newTestSuppliedAudioProvider(t *testing.T, lookup func(string) (document.SuppliedTranscript, error)) *suppliedtranscript.Provider {
	t.Helper()
	provider, err := suppliedtranscript.New(suppliedtranscript.Profile{
		Source: lookupTranscriptSource{lookup: lookup}, ProcessingProfile: suppliedAudioProfileTemplate(t),
	})
	require.NoError(t, err)
	return provider
}

func newSuppliedAudioVault(t *testing.T, provider *suppliedtranscript.Provider) (*docbank.Vault, docbank.ProcessingSelector, docbank.ProcessingPlan) {
	t.Helper()
	profile := suppliedAudioProfileForProvider(t, provider)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{
		Profiles: map[string]docbank.ProcessingProfileConfig{"audio": {Profile: profile, RenditionProvider: provider}},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/voice.wav", bytes.NewReader(mediatest.WAV()), docbank.PutOptions{MediaType: "audio/wav"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "audio"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	return vault, selector, plan
}

func suppliedAudioProfile(t *testing.T, descriptor document.RenditionDescriptor) document.ProcessingProfileV1 {
	t.Helper()
	profile := embeddedProcessingProfile(t, descriptor)
	hash := func(value string) string {
		digest := sha256.Sum256([]byte(value))
		return hex.EncodeToString(digest[:])
	}
	profile.Rendition.AdapterContract = "supplied-transcript.in-process/v1"
	profile.Rendition.Descriptor = document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}
	profile.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactTranscript}
	profile.RetentionDisclosure.RetainTypedArtifacts = true
	profile.RetentionDisclosure.TrustBoundary = string(descriptor.TrustBoundary)
	profile.Rendition.UploadOptionsFingerprint = hash("supplied-transcript-upload")
	return profile
}

func suppliedAudioProfileTemplate(t *testing.T) document.ProcessingProfileV1 {
	t.Helper()
	return suppliedAudioProfile(t, document.RenditionDescriptor{
		TrustBoundary: document.RenditionTrustLocalProcess,
	})
}

func suppliedAudioProfileForProvider(t *testing.T, provider *suppliedtranscript.Provider) document.ProcessingProfileV1 {
	t.Helper()
	profile := suppliedAudioProfileTemplate(t)
	descriptor := provider.Descriptor()
	profile.Rendition.Descriptor = document.ProviderDescriptorV1{
		ID: descriptor.ID, Fingerprint: descriptor.Fingerprint,
	}
	profile.RetentionDisclosure.TrustBoundary = string(descriptor.TrustBoundary)
	return profile
}

func waitForProcessingStatus(t *testing.T, vault *docbank.Vault, jobID, want string) docbank.ProcessingStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: jobID})
		if err == nil && status.State == want {
			return status
		}
		time.Sleep(20 * time.Millisecond)
	}
	status, err := vault.ProcessingStatus(t.Context(), docbank.ProcessingStatusRequest{JobID: jobID})
	require.NoError(t, err)
	require.Equal(t, want, status.State, "status: %+v", status)
	return status
}

func versionIDs(versions []docbank.ContentVersion) []string {
	ids := make([]string, len(versions))
	for index, version := range versions {
		ids[index] = version.ID
	}
	return ids
}
