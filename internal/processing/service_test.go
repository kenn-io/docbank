package processing

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestProcessingServiceSourceFenceIsBoundedCanonicalAuthority(t *testing.T) {
	ids := []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}
	normalized, err := normalizeFenceIDs(ids)
	require.NoError(t, err)
	require.Equal(t, []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}, normalized)
	require.Equal(t, []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}, ids)

	_, err = normalizeFenceIDs(nil)
	require.ErrorContains(t, err, "between 1")
	_, err = normalizeFenceIDs([]string{ids[0], ids[0]})
	require.ErrorContains(t, err, "duplicate")
	_, err = normalizeFenceIDs(make([]string, MaxSourceFenceIDs+1))
	require.ErrorContains(t, err, strconv.Itoa(MaxSourceFenceIDs))
}

func TestDerivativePurgeRequiresCanonicalContentVersionIDs(t *testing.T) {
	const canonical = "abcdefab-1234-4abc-8def-123456789abc"
	for _, id := range []string{
		canonical,
		"ABCDEFAB-1234-4ABC-8DEF-123456789ABC",
		"abcdefab12344abc8def123456789abc",
		"urn:uuid:" + canonical,
		"abcdefab-1234-1abc-8def-123456789abc",
		"abcdefab-1234-4abc-cdef-123456789abc",
	} {
		t.Run(id, func(t *testing.T) {
			got, err := normalizeDerivativePurgeRequest(DerivativePurgeRequest{ContentVersionIDs: []string{id}})
			if id == canonical {
				require.NoError(t, err)
				require.Equal(t, []string{canonical}, got.ContentVersionIDs)
			} else {
				require.ErrorIs(t, err, ErrInvalidPurgeRequest)
			}
		})
	}
}

func TestProcessingServicePlanFingerprintSealsDisclosure(t *testing.T) {
	plan := Plan{VaultUID: "00000000-0000-4000-8000-000000000001",
		Selector:           Selector{NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Profile: "private"},
		ProfileFingerprint: frontmatterHashForService("profile"),
		Flow: []FlowHop{{Capability: "rendition", ProviderID: "local", TrustBoundary: "local_process",
			InputClasses: []string{"original_file"}}}, RetainedClasses: []string{"sanitized_markdown"},
		ConsentRequired: true}
	first, err := planFingerprint(plan)
	require.NoError(t, err)
	second, err := planFingerprint(plan)
	require.NoError(t, err)
	require.Equal(t, first, second)
	plan.ConsentRequired = false
	plan.ConsentState = "active"
	granted, err := planFingerprint(plan)
	require.NoError(t, err)
	require.Equal(t, first, granted)
	plan.Flow[0].TrustBoundary = "hosted_provider"
	changed, err := planFingerprint(plan)
	require.NoError(t, err)
	require.NotEqual(t, first, changed)
}

func TestAggregateStatusNeverReportsUnfinishedEmbeddingsAsCompleted(t *testing.T) {
	embeddings := []store.EmbeddingJobStatus{{ID: "a", State: "completed"}, {ID: "b", State: "abandoned"}}
	status := aggregateStatus("a", nil, embeddings)
	require.Equal(t, "abandoned", status.State)
	require.Equal(t, 1, status.CompletedBindings)

	embeddings[1].State = "completed"
	require.Equal(t, "completed", aggregateStatus("a", nil, embeddings).State)

	embeddings[1].State = "unexpected"
	require.Equal(t, "unexpected", aggregateStatus("a", nil, embeddings).State)
}

func TestInspectionPolicyCanonicalizesDeclaredMediaTypeForDurableReplay(t *testing.T) {
	profile := configuredProfile{
		portable: document.ProcessingProfileV1{Rendition: &document.RenditionBindingV1{
			MaxDocumentBytes: 1024, DisclosureFingerprint: strings.Repeat("1", 64),
		}},
		record: store.ProcessingProfileRecord{Fingerprint: strings.Repeat("2", 64)},
		provider: inertRenditionProvider{descriptor: document.RenditionDescriptor{
			Fingerprint: strings.Repeat("3", 64),
		}},
	}
	policy := inspectionPolicy("document.txt", store.ContentVersion{
		ID: "00000000-0000-4000-8000-000000000001", BlobHash: strings.Repeat("4", 64),
		Size: 12, MimeType: "text/plain; charset=utf-8",
	}, profile)
	require.Equal(t, "text/plain", policy.DeclaredMediaType)
}

type inertRenditionProvider struct{ descriptor document.RenditionDescriptor }

func (provider inertRenditionProvider) Descriptor() document.RenditionDescriptor {
	return provider.descriptor
}
func (inertRenditionProvider) Render(context.Context, document.AuthorizedUpload,
	document.RenditionAuthorization,
) (document.RenditionResult, error) {
	return document.RenditionResult{}, nil
}

func BenchmarkProcessingServiceSourceFence4096(b *testing.B) {
	ids := make([]string, MaxSourceFenceIDs)
	for index := range ids {
		ids[index] = fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := normalizeFenceIDs(ids); err != nil {
			b.Fatal(err)
		}
	}
}

func frontmatterHashForService(value string) string {
	return fmt.Sprintf("%064s", value)
}

func TestProcessingServiceWaitsForEmbeddingRetryAndHonorsCancellation(t *testing.T) {
	fixture, fake, _, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	fake.runtime.failures[request.BindingID] = []error{embeddingTransientError{}, embeddingTransientError{}, embeddingTransientError{}}
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: request.BindingID, descriptor: request.Descriptor}
	runtime, err := NewProviderEmbeddingRuntime(provider, fixture.blobs, t.TempDir(), fake.runtime.Classify)
	require.NoError(t, err)
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(request.Profile.CanonicalProfile, &portable))
	profile := configuredProfile{portable: portable, record: request.Profile,
		embedders:         map[string]document.EmbeddingProvider{request.BindingID: provider},
		embeddingRuntimes: map[string]*ProviderEmbeddingRuntime{request.BindingID: runtime}}
	var clockOffset atomic.Int64
	clockOffset.Store(int64(time.Second))
	service := &Service{catalog: fixture.catalog, blobs: fixture.blobs, gate: fake.gate,
		clock: func() time.Time { return time.Now().UTC().Add(time.Duration(clockOffset.Load())) }}
	version, err := fixture.catalog.ContentVersionByID(t.Context(), request.ContentVersionID)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	var jobs []string
	var runErr error
	finished := make(chan struct{})
	go func() {
		jobs, runErr = service.runEmbeddings(ctx, version, profile, request.Authorization.Principal, request.Authorization.Scope, "", nil)
		close(finished)
	}()
	t.Cleanup(func() { cancel(); <-finished })
	var pendingID string
	require.Eventually(t, func() bool {
		statuses, statusErr := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), version.ID, profile.record.Fingerprint)
		if statusErr != nil {
			return false
		}
		for _, status := range statuses {
			if status.State == "retry_wait" {
				pendingID = status.ID
				return true
			}
		}
		return false
	}, 3*time.Second, time.Millisecond)
	cancel()
	<-finished
	require.ErrorIs(t, runErr, context.Canceled)
	require.Equal(t, []string{pendingID}, jobs)
	require.Equal(t, 3, fake.runtime.callCount(request.BindingID), "waiting must not call the provider before backoff expires")
	clockOffset.Store(int64(2 * time.Minute))
	retried, err := service.runEmbeddings(t.Context(), version, profile, request.Authorization.Principal, request.Authorization.Scope, "", nil)
	require.NoError(t, err)
	require.Equal(t, jobs, retried)
	status, err := fixture.catalog.EmbeddingJobByID(t.Context(), jobs[0])
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
}

func TestProcessingServiceCompletesEmbeddingAfterWorkerStops(t *testing.T) {
	fixture, fake, worker, original := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
	workerContext, stopWorker := context.WithCancel(t.Context())
	stopWorker()
	require.ErrorIs(t, worker.Run(workerContext), context.Canceled)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(original.Profile.CanonicalProfile, &profile))
	profile.Rendition = nil
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profile.RetentionDisclosure.RetainProviderMarkdown = false
	provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: original.BindingID, descriptor: original.Descriptor}
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Lifecycle: t.Context(),
		Principal: original.Authorization.Principal, Scope: original.Authorization.Scope,
		Profiles: map[string]ProfileConfig{"direct": {Profile: profile,
			EmbeddingProviders: map[string]document.EmbeddingProvider{original.BindingID: provider}}}})
	require.NoError(t, err)
	version, err := fixture.catalog.ContentVersionByID(t.Context(), original.ContentVersionID)
	require.NoError(t, err)
	selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: "direct"}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	job, err := service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err, "foreground processing must progress without a background worker")
	status, err := service.Status(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", status.State)
	require.Equal(t, 1, status.CompletedBindings)
}

func TestProcessingServiceAnnouncesFirstEmbeddingBeforeLaterEnqueues(t *testing.T) {
	for _, stop := range []string{"request-canceled", "later-consent-missing"} {
		t.Run(stop, func(t *testing.T) {
			fixture, fake, worker, original := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
			var profile document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(original.Profile.CanonicalProfile, &profile))
			profile.Rendition = nil
			profile.RetentionDisclosure.RetainSanitizedMarkdown = false
			profile.RetentionDisclosure.RetainProviderMarkdown = false
			binding := profile.Embeddings[0]
			profile.Embeddings = nil
			provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: original.BindingID, descriptor: original.Descriptor}
			providers := map[string]document.EmbeddingProvider{}
			for _, name := range []string{"first", "second", "third"} {
				binding.Name = name
				if name == "third" {
					binding.DisclosureFingerprint = workerHash("third-binding-disclosure")
				}
				profile.Embeddings = append(profile.Embeddings, binding)
				providers[name] = provider
			}
			service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
				Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Lifecycle: t.Context(),
				Principal: original.Authorization.Principal, Scope: original.Authorization.Scope,
				Profiles: map[string]ProfileConfig{"direct": {Profile: profile, EmbeddingProviders: providers}}})
			require.NoError(t, err)
			version, err := fixture.catalog.ContentVersionByID(t.Context(), original.ContentVersionID)
			require.NoError(t, err)
			selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: "direct"}
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err)
			var grants []store.ProcessingConsentGrantRequest
			for _, binding := range profile.Embeddings {
				if stop == "later-consent-missing" && binding.Name == "third" {
					continue
				}
				grants = append(grants, store.ProcessingConsentGrantRequest{
					Principal: original.Authorization.Principal, Scope: original.Authorization.Scope,
					ProfileFingerprint: plan.ProfileFingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
					InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
				})
			}
			_, err = fixture.catalog.GrantConsentSet(t.Context(), grants)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var announced []Job
			job, err := service.StartWithProgress(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint}, func(job Job) {
				announced = append(announced, job)
				jobs, err := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), version.ID, plan.ProfileFingerprint)
				require.NoError(t, err)
				require.Len(t, jobs, 1, "the first durable binding must be announced before later enqueues")
				if stop == "request-canceled" {
					cancel()
				}
			})
			jobs, readErr := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), version.ID, plan.ProfileFingerprint)
			require.NoError(t, readErr)
			if stop == "later-consent-missing" {
				require.ErrorIs(t, err, ErrConsentRequired)
				require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
				require.Len(t, jobs, 2)
				processed, workerErr := worker.RunJob(t.Context(), jobs[0].ID)
				require.NoError(t, workerErr)
				require.True(t, processed, "independent workers can process the already committed binding")
				status, statusErr := fixture.catalog.EmbeddingJobByID(t.Context(), jobs[0].ID)
				require.NoError(t, statusErr)
				require.Equal(t, "completed", status.State)
			} else {
				require.NoError(t, err)
				require.Len(t, jobs, 3)
				for _, job := range jobs {
					require.Equal(t, "completed", job.State)
				}
			}
			require.Len(t, announced, 1)
			require.Equal(t, announced[0].ID, job.ID)
			require.Equal(t, []string{job.ID}, announced[0].EmbeddingJobIDs)
			for _, durable := range jobs {
				require.Contains(t, job.EmbeddingJobIDs, durable.ID, "partial results must retain every durable binding identity")
			}
		})
	}
}

func TestProcessingServiceCoverageBeforeProfileRegistration(t *testing.T) {
	fixture := newPublicationFixture(t)
	record := embeddingWorkerProfile(t, embeddingWorkerDescriptor(t))
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(record.CanonicalProfile, &profile))
	service := &Service{catalog: fixture.catalog, profiles: map[string]configuredProfile{
		"private": {portable: profile, record: record},
	}}
	version, err := fixture.catalog.ContentVersionByID(t.Context(), fixture.versionID)
	require.NoError(t, err)
	trashed, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "trashed.pdf",
		version.BlobHash, version.Size, version.MimeType)
	require.NoError(t, err)
	_, _, err = fixture.catalog.Trash(t.Context(), trashed.ID, -1)
	require.NoError(t, err)
	fence := SourceFence{VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{
		version.ID, trashed.CurrentVersionID, "00000000-0000-4000-8000-000000000001",
	}}
	coverage, err := service.Coverage(t.Context(), "private", fence)
	require.NoError(t, err)
	require.Equal(t, "partial", coverage.State)
	require.Len(t, coverage.Embeddings, 2)
	for _, binding := range coverage.Embeddings {
		require.Equal(t, "unavailable", binding.State)
		require.Equal(t, 3, binding.Total)
		require.Equal(t, 2, binding.Stale)
		require.Equal(t, 1, binding.Unavailable)
		require.Zero(t, binding.Complete)
	}
	_, _, err = fixture.catalog.Trash(t.Context(), version.NodeID, -1)
	require.NoError(t, err)
	coverage, err = service.Coverage(t.Context(), "private", fence)
	require.NoError(t, err)
	for _, binding := range coverage.Embeddings {
		require.Equal(t, "stale", binding.State)
		require.Equal(t, 3, binding.Stale)
		require.Zero(t, binding.Unavailable)
	}
}

func TestProcessingServiceCoverageMissingClassesTakePrecedenceOverRebuilding(t *testing.T) {
	for _, test := range []struct {
		name               string
		renditionRequired  bool
		rebuildingRequired bool
		missingRequired    bool
		want               string
	}{
		{"missing rendition with optional rebuilding", true, false, false, "partial"},
		{"missing rendition with required rebuilding", true, true, false, "partial"},
		{"missing required embedding", false, true, true, "partial"},
		{"missing optional embedding", false, true, false, "rebuilding"},
		{"missing optional embedding without required bindings", false, false, false, "partial"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, _, _, request := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile, "missing")
			var portable document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(request.Profile.CanonicalProfile, &portable))
			if test.renditionRequired {
				portable.Embeddings = portable.Embeddings[:1]
			}
			if !test.renditionRequired {
				portable.Rendition = nil
				portable.RetentionDisclosure.RetainSanitizedMarkdown = false
				portable.RetentionDisclosure.RetainProviderMarkdown = false
			}
			for i := range portable.Embeddings {
				if (portable.Embeddings[i].Name == request.BindingID && test.rebuildingRequired) ||
					(portable.Embeddings[i].Name == "missing" && test.missingRequired) {
					portable.Embeddings[i].Activation = document.EmbeddingRequired
				}
			}
			canonical, fingerprints, err := document.CanonicalProfile(portable)
			require.NoError(t, err)
			request.Profile = store.ProcessingProfileRecord{
				Fingerprint: fingerprints.Profile, CanonicalProfile: canonical,
				RenditionRequestFingerprint:    fingerprints.RenditionRequest,
				EvidenceLexicalFingerprint:     fingerprints.EvidenceLexical,
				RetentionDisclosureFingerprint: fingerprints.RetentionDisclosure,
				AttachmentPolicyFingerprint:    portable.RetentionDisclosure.AttachmentPolicyFingerprint,
				ConsentFingerprint:             portable.RetentionDisclosure.ConsentFingerprint,
				TrustBoundary:                  portable.RetentionDisclosure.TrustBoundary,
			}
			if portable.Rendition != nil {
				request.Profile.RenditionDisclosureFingerprint = portable.Rendition.DisclosureFingerprint
			}
			request.InputGeneration.ID = workerHash(test.name)
			request.InputGeneration.ProcessingProfileFingerprint = fingerprints.Profile
			request.Authorization.ProfileFingerprint = fingerprints.Profile
			// Enqueue registers the new profile before its consent check.
			_, err = fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
			_, err = fixture.catalog.GrantConsent(t.Context(), store.ProcessingConsentGrantRequest{
				Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
				ProfileFingerprint: fingerprints.Profile, DisclosureFingerprint: request.Authorization.DisclosureFingerprint,
				InputClasses: request.Authorization.InputClasses, RetainedArtifactClasses: request.Authorization.RetainedArtifactClasses,
			})
			require.NoError(t, err)
			_, err = fixture.catalog.EnqueueEmbeddingJob(t.Context(), request)
			require.NoError(t, err)
			service := &Service{catalog: fixture.catalog, profiles: map[string]configuredProfile{
				"private": {portable: portable, record: request.Profile},
			}}
			coverage, err := service.Coverage(t.Context(), "private", SourceFence{
				VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{request.ContentVersionID},
			})
			require.NoError(t, err)
			assert.Equal(t, test.want, coverage.State)
			for _, binding := range coverage.Embeddings {
				if binding.Name == request.BindingID {
					assert.Equal(t, "rebuilding", binding.State)
					assert.Equal(t, 1, binding.Rebuilding)
				} else {
					assert.Equal(t, "unavailable", binding.State)
				}
			}
		})
	}
}

func TestAggregateStatusUsesBindingActivation(t *testing.T) {
	for _, activation := range []document.EmbeddingActivation{document.EmbeddingRequired, document.EmbeddingOptional} {
		for _, state := range []string{"failed", "abandoned"} {
			t.Run(string(activation)+"/"+state, func(t *testing.T) {
				embeddings := []store.EmbeddingJobStatus{{ID: "a", State: "completed", Activation: document.EmbeddingRequired}, {ID: "b", State: state, Activation: activation}}
				status := aggregateStatus("a", nil, embeddings)
				want := state
				if activation == document.EmbeddingOptional {
					want = "partial"
				}
				require.Equal(t, want, status.State)
				require.Equal(t, 1, status.CompletedBindings)
				embeddings = append(embeddings, store.EmbeddingJobStatus{ID: "c", State: "failed", Activation: document.EmbeddingRequired})
				require.Equal(t, "failed", aggregateStatus("a", nil, embeddings).State, "required failure must take precedence")
			})
		}
	}
}

func TestProcessingServiceRejectsRevokedRenditionWaiter(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, published, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	request.Authorization.Principal = "operator:revoked"
	grantWorkerConsent(t, fixture.catalog, request)
	_, rejected, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)
	_, err = fixture.catalog.RevokeConsent(t.Context(), store.ProcessingConsentRevocationRequest{
		Principal: request.Authorization.Principal, Scope: request.Authorization.Scope,
	})
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: workerRuntime{provider: provider},
		Gate: newTestOperationGate(), Owner: "rendition-waiter-test",
		LeaseDuration: time.Minute, IdleDelay: time.Millisecond,
	})
	require.NoError(t, err)
	processed, err := worker.RunJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.True(t, processed)
	current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, current.State)
	service := &Service{catalog: fixture.catalog}
	result, err := service.renditionResult(t.Context(), published.ID)
	require.NoError(t, err)
	require.Equal(t, renditionRun{jobID: job.ID, waiterID: published.ID,
		attachmentID: published.AttachmentID, authorizationGrantID: published.AuthorizationGrantID}, result)
	_, err = service.renditionResult(t.Context(), rejected.ID)
	require.ErrorIs(t, err, ErrConsentRequired)
}

func TestEmbeddingOnlyConsentPreconditions(t *testing.T) {
	for _, phase := range []string{"missing", "revoked", "expired-before", "expired-during", "allowed", "provider-authorization"} {
		t.Run(phase, func(t *testing.T) {
			fixture, fake, _, original := newRealEmbeddingWorker(t, document.EmbeddingInputOriginalFile)
			var profile document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(original.Profile.CanonicalProfile, &profile))
			profile.Rendition = nil
			profile.RetentionDisclosure.RetainSanitizedMarkdown = false
			profile.RetentionDisclosure.RetainProviderMarkdown = false
			provider := &embeddingWorkerProvider{runtime: fake.runtime, binding: original.BindingID, descriptor: original.Descriptor}
			config := ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(),
				Principal: original.Authorization.Principal, Scope: original.Authorization.Scope,
				Profiles: map[string]ProfileConfig{"direct": {Profile: profile, EmbeddingProviders: map[string]document.EmbeddingProvider{original.BindingID: provider}}}}
			if phase == "provider-authorization" {
				fake.runtime.failures[original.BindingID] = []error{errors.New("synthetic credential denied")}
				configured := config.Profiles["direct"]
				configured.EmbeddingClassifiers = map[string]func(error) (EmbeddingProviderFailure, time.Duration){original.BindingID: func(error) (EmbeddingProviderFailure, time.Duration) { return EmbeddingProviderAuthorization, 0 }}
				config.Profiles["direct"] = configured
			}
			service, err := NewService(config)
			require.NoError(t, err)
			version, err := fixture.catalog.ContentVersionByID(t.Context(), original.ContentVersionID)
			require.NoError(t, err)
			selector := Selector{NodeID: version.NodeID, ContentVersionID: version.ID, Profile: "direct"}
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err)
			if phase != "missing" {
				var expiry *time.Time
				if phase == "expired-before" || phase == "expired-during" {
					expiry = new(time.Now().Add(2 * time.Second))
				}
				_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, ExpiresAt: expiry})
				require.NoError(t, err)
				if phase == "revoked" {
					_, err = service.RevokeConsent(t.Context())
					require.NoError(t, err)
				}
				if phase == "expired-before" {
					<-time.After(time.Until(*expiry) + 20*time.Millisecond)
				}
				if phase == "expired-during" {
					fake.runtime.inspectInputs = func([]document.EmbeddingInput) { <-time.After(time.Until(*expiry) + 20*time.Millisecond) }
				}
			}
			job, err := service.Start(t.Context(), StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: false})
			jobs, readErr := fixture.catalog.EmbeddingJobsForVersionProfile(t.Context(), version.ID, plan.ProfileFingerprint)
			if phase == "missing" || phase == "revoked" || phase == "expired-before" {
				require.ErrorIs(t, readErr, store.ErrNotFound)
			} else {
				require.NoError(t, readErr)
			}
			switch phase {
			case "missing":
				require.ErrorIs(t, err, ErrConsentRequired)
				require.ErrorIs(t, err, store.ErrProcessingConsentRequired)
			case "revoked":
				require.ErrorIs(t, err, store.ErrProcessingConsentRevoked)
			case "expired-before", "expired-during":
				require.ErrorIs(t, err, store.ErrProcessingConsentExpired)
			default:
				require.NoError(t, err)
				status, statusErr := service.Status(t.Context(), job.ID)
				require.NoError(t, statusErr)
				if phase == "allowed" {
					require.Equal(t, "completed", status.State)
				} else {
					require.Equal(t, "authorization", status.FailureCode)
				}
			}
			if phase == "missing" || phase == "revoked" || phase == "expired-before" {
				require.Empty(t, jobs)
				require.Zero(t, fake.runtime.calls())
			}
		})
	}
}

func TestDocumentSourceFenceFingerprintIsStableAndBindsExactAuthority(t *testing.T) {
	fence := SourceFence{VaultUID: "11111111-1111-4111-8111-111111111111", ContentVersionIDs: []string{
		"33333333-3333-4333-8333-333333333333",
		"22222222-2222-4222-8222-222222222222",
	}}
	canonical, err := sourceFenceCanonicalBytes(fence)
	require.NoError(t, err)
	wantCanonical := []byte("docbank-document-source-fence/v1" +
		"\x00\x00\x00\x24" + "11111111-1111-4111-8111-111111111111" +
		"\x00\x00\x00\x02" +
		"\x00\x00\x00\x24" + "22222222-2222-4222-8222-222222222222" +
		"\x00\x00\x00\x24" + "33333333-3333-4333-8333-333333333333")
	assert.Equal(t, hex.EncodeToString(wantCanonical), hex.EncodeToString(canonical))

	fingerprint, err := SourceFenceFingerprint(fence)
	require.NoError(t, err)
	assert.Equal(t, "sha256:3c2a6756783fd03230bb89fe15de79ba10b3a6c1511d56be4042994e66d707cc", fingerprint)
	assert.NotContains(t, fingerprint, fence.VaultUID)

	reordered := SourceFence{VaultUID: fence.VaultUID, ContentVersionIDs: []string{
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}}
	reorderedFingerprint, err := SourceFenceFingerprint(reordered)
	require.NoError(t, err)
	assert.Equal(t, fingerprint, reorderedFingerprint)

	changed := reordered
	changed.VaultUID = "44444444-4444-4444-8444-444444444444"
	changedFingerprint, err := SourceFenceFingerprint(changed)
	require.NoError(t, err)
	assert.NotEqual(t, fingerprint, changedFingerprint)
}

func TestDocumentSourceFenceFingerprintRejectsUint32LengthOverflow(t *testing.T) {
	var encoded []byte
	require.ErrorContains(t, appendSourceFenceUint32(&encoded, uint64(math.MaxUint32)+1), "uint32")
}

func TestProcessingServicePlanFingerprintSealsCompleteRuntimeDisclosure(t *testing.T) {
	plan := Plan{VaultUID: "00000000-0000-4000-8000-000000000001",
		Selector:           Selector{NodeID: 1, ContentVersionID: "00000000-0000-4000-8000-000000000002", Profile: "private"},
		ProfileFingerprint: frontmatterHashForService("profile"),
		Flow: []FlowHop{{Capability: "rendition", ProviderID: "local", TrustBoundary: "local_process",
			InputClasses: []string{"original_file"}}}, RetainedClasses: []string{"sanitized_markdown"},
		ConsentRequired: true}
	encoded, err := json.Marshal(plan)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	flow, ok := wire["Flow"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, flow)
	hop, ok := flow[0].(map[string]any)
	require.True(t, ok)
	hop["RuntimeDisclosure"] = map[string]any{
		"ImmediateProcessor": "docbank plaintext adapter",
		"UltimateProcessor":  "docbank process",
		"Endpoint":           "in-process",
		"Deployment":         frontmatterHashForService("deployment"),
		"Model":              "plain-text",
		"ModelRevision":      "builtin-1",
		"VectorSpace":        "not-applicable",
		"MetadataClasses":    []any{"byte_length", "content_hash", "detected_media_type", "synthetic_filename"},
		"RetainedArtifactRoles": []any{
			"normalized_evidence", "sanitized_markdown",
		},
	}
	decode := func(value map[string]any) Plan {
		t.Helper()
		body, marshalErr := json.Marshal(value)
		require.NoError(t, marshalErr)
		var result Plan
		require.NoError(t, json.Unmarshal(body, &result, json.RejectUnknownMembers(true)))
		return result
	}
	baseline, err := planFingerprint(decode(wire))
	require.NoError(t, err)

	for _, testCase := range []struct {
		name  string
		field string
		value any
	}{
		{name: "endpoint", field: "Endpoint", value: "https://processor.example/v2"},
		{name: "deployment", field: "Deployment", value: frontmatterHashForService("new-deployment")},
		{name: "model revision", field: "ModelRevision", value: "builtin-2"},
		{name: "metadata class", field: "MetadataClasses", value: []any{"byte_length", "content_hash"}},
		{name: "retention", field: "RetainedArtifactRoles", value: []any{"normalized_evidence"}},
		{name: "vector space", field: "VectorSpace", value: frontmatterHashForService("vector-space")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body, marshalErr := json.Marshal(wire)
			require.NoError(t, marshalErr)
			var changedWire map[string]any
			require.NoError(t, json.Unmarshal(body, &changedWire))
			changedFlow, flowOK := changedWire["Flow"].([]any)
			require.True(t, flowOK)
			require.NotEmpty(t, changedFlow)
			changedHop, hopOK := changedFlow[0].(map[string]any)
			require.True(t, hopOK)
			changedDisclosure, disclosureOK := changedHop["RuntimeDisclosure"].(map[string]any)
			require.True(t, disclosureOK)
			changedDisclosure[testCase.field] = testCase.value
			changed, fingerprintErr := planFingerprint(decode(changedWire))
			require.NoError(t, fingerprintErr)
			require.NotEqual(t, baseline, changed)
		})
	}
}

func TestProcessingServiceCoverageReportsRebuildWhilePreviousGenerationServes(t *testing.T) {
	fixture := newPublicationFixture(t)
	provider := newWorkerProvider(t)
	profile := workerProcessingProfile(t, provider.Descriptor())
	var portable document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(profile.CanonicalProfile, &portable, json.RejectUnknownMembers(true)))
	portable.Rendition.TrustBoundary = string(provider.Descriptor().TrustBoundary)
	gate := newWorkerTestGate()
	service, err := NewService(ServiceConfig{
		Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: gate,
		SpoolDirectory: filepath.Join(t.TempDir(), "spool"),
		Profiles: map[string]ProfileConfig{"private": {
			Profile: portable, RenditionProvider: provider,
		}},
	})
	require.NoError(t, err)
	profile = service.profiles["private"].record
	fixture.profile = profile
	publisher, err := NewArtifactPublisher(fixture.catalog, fixture.blobs)
	require.NoError(t, err)
	_, err = publisher.PublishRendition(t.Context(), fixture.stage(t,
		publicationIDs{"coverage-old-build", "coverage-old-attachment", "coverage-old-generation"},
		"old searchable evidence", "old markdown",
	))
	require.NoError(t, err)

	request := workerJobRequest(fixture.versionID, profile, provider.Descriptor())
	grantWorkerConsent(t, fixture.catalog, request)
	job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
	require.NoError(t, err)

	coverage, err := service.Coverage(t.Context(), "private", SourceFence{
		VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{fixture.versionID},
	})
	require.NoError(t, err)
	assert.Equal(t, "rebuilding", coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Rebuilding)
	assert.Equal(t, 1, coverage.Renditions.PreviousServing)
	assert.Zero(t, coverage.Renditions.Complete)
	assert.Equal(t, "rebuilding", coverage.State)

	coverage, err = service.Coverage(t.Context(), "private", SourceFence{
		VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{
			fixture.versionID, "00000000-0000-4000-8000-000000000001",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "partial", coverage.State)
	assert.Equal(t, "rebuilding", coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Stale)
	assert.Equal(t, 1, coverage.Renditions.PreviousServing)

	now := time.Now().UTC()
	claim, err := fixture.catalog.ClaimRenditionJob(
		t.Context(), job.ID, "coverage-service-test", now, 5*time.Minute,
	)
	require.NoError(t, err)
	require.NoError(t, fixture.catalog.MarkRenditionJobFailed(
		t.Context(), claim, store.RenditionFailureTerminal, now.Add(time.Second),
	))
	coverage, err = service.Coverage(t.Context(), "private", SourceFence{
		VaultUID: fixture.catalog.VaultID(), ContentVersionIDs: []string{fixture.versionID},
	})
	require.NoError(t, err)
	assert.Equal(t, "complete", coverage.Renditions.State)
	assert.Equal(t, 1, coverage.Renditions.Complete)
	assert.Zero(t, coverage.Renditions.Rebuilding)
	assert.Zero(t, coverage.Renditions.PreviousServing)
	assert.Equal(t, "complete", coverage.State)
}
