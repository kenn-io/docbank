package processing

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestMediaMaxBytesAllowsSuppliedMedia(t *testing.T) {
	t.Parallel()
	for _, maximum := range []int64{-1, 0, 1 << 30, 4 << 30, (4 << 30) + 1} {
		t.Run(strconv.FormatInt(maximum, 10), func(t *testing.T) {
			fixture := newPublicationFixture(t)
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(mediatest.WAV()))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), "call.wav",
				written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
			require.NoError(t, err)
			service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
				Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), MediaMaxBytes: maximum,
			})
			if maximum < 0 || maximum > 4<<30 {
				require.ErrorContains(t, err, "media byte limit")
				return
			}
			require.NoError(t, err)
			receipt, err := service.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
				OperationID: "00000000-0000-4000-8000-000000000703", Filename: "call.wav",
				MediaType: "audio/wav", SHA256: written.Hash, ByteLength: written.Size,
				ExistingContentVersionID: node.CurrentVersionID,
				Occurrence:               MediaOccurrenceInput{Ref: "message", Revision: "1"},
			})
			require.NoError(t, err)
			require.Equal(t, node.CurrentVersionID, receipt.ContentVersionID)
		})
	}
}

func TestStageMediaFourGiBCeiling(t *testing.T) {
	t.Parallel()
	raw := mediatest.WAV()
	_, err := stageMedia(t.Context(), bytes.NewReader(raw), io.Discard, int64(len(raw)), 4<<30, processingHash(string(raw)))
	require.NoError(t, err)
	_, err = stageMedia(t.Context(), bytes.NewReader(raw), io.Discard, (4<<30)+1, 4<<30, processingHash(string(raw)))
	require.ErrorContains(t, err, "byte_limit")
}

func TestAudioAdmissionUsesSmallerCeilingAndProfile(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	_, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, "synthetic:operator")
	require.NoError(t, err)
	configured := configuredProfile{portable: profile.Profile, provider: profile.RenditionProvider, mediaMaxBytes: 3 << 30}
	admitted := func(filename, mediaType string, size int64) int64 {
		version := store.ContentVersion{MimeType: mediaType, Size: size}
		return admittedSourceBytes(inspectionPolicy(filename, version, configured), configured)
	}
	require.Equal(t, int64(3<<30), admitted("call.wav", "audio/wav", 2<<30))
	configured.portable.Rendition.MaxDocumentBytes = 2 << 30
	require.Equal(t, int64(2<<30), admitted("call.wav", "audio/wav", 2<<30))
	configured.mediaMaxBytes = 64
	require.Equal(t, int64(1<<30), admitted("notes.txt", "text/plain", 2<<30), "the media ceiling does not bound documents")
}

func TestSmallAudioKeepsDocumentInspectionBound(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	_, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, "synthetic:operator")
	require.NoError(t, err)
	// A custom profile above 1 GiB queued audio work under the 1 GiB inspection
	// bound before WAV and MP3 could stream past it. Small sources must keep
	// that policy so their queued execution identities still match.
	configured := configuredProfile{portable: profile.Profile, provider: profile.RenditionProvider, mediaMaxBytes: 4 << 30}
	configured.portable.Rendition.MaxDocumentBytes = 2 << 30
	for _, size := range []int64{64, 1 << 30} {
		policy := inspectionPolicy("call.wav", store.ContentVersion{MimeType: "audio/wav", Size: size}, configured)
		require.Equal(t, int64(1<<30), policy.MaxSourceBytes, "size %d", size)
		require.Equal(t, int64(1<<30), policy.MaxExpandedBytes, "size %d", size)
		require.Equal(t, int64(1<<30), policy.MaxEntryBytes, "size %d", size)
	}
	large := inspectionPolicy("call.wav", store.ContentVersion{MimeType: "audio/wav", Size: (1 << 30) + 1}, configured)
	require.Equal(t, int64(2<<30), large.MaxSourceBytes)
}

func TestConfiguredCeilingAppliesToRetainedAudioPlanAndRuntime(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	raw := mediatest.WAV()
	written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "call.wav", written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	name, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, "synthetic:operator")
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), MediaMaxBytes: 64, Profiles: map[string]ProfileConfig{name: profile}})
	require.NoError(t, err)
	plan, err := service.Plan(t.Context(), Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: name})
	require.NoError(t, err)
	require.Equal(t, "source_bytes_exceeded", plan.RenditionIneligibleReason)
	work := store.RenditionJobWork{Profile: service.profiles[name].record, Job: store.RenditionJob{SourceSHA256: written.Hash}, Waiter: store.RenditionJobWaiter{ContentVersionID: node.CurrentVersionID}, ExecutionIdentity: document.RenditionExecutionIdentityV1{Upload: document.AuthorizedUploadMetadata{Filename: "call.wav", MediaType: "audio/wav", ByteLength: written.Size}}}
	_, err = service.renditions.Prepare(t.Context(), work, service.clock())
	require.ErrorIs(t, err, ErrSourceIneligible)
}

func TestPlanStreamsLargeSourceOnlyForAdmittedAudio(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	name, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, "synthetic:operator")
	require.NoError(t, err)
	textProvider := newWorkerProvider(t)
	descriptor := textProvider.Descriptor()
	descriptor.Fingerprint = ""
	descriptor.SupportedFormats = []document.RenditionFormatCapability{{
		MediaFamily: "text", MediaType: "text/plain", InputKind: document.RenditionInputOriginalFile,
	}}
	textProvider.descriptor, err = document.NewRenditionDescriptor(descriptor)
	require.NoError(t, err)
	textRecord := workerProcessingProfile(t, textProvider.Descriptor())
	var textProfile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(textRecord.CanonicalProfile, &textProfile))
	textProfile.Rendition.MaxDocumentBytes = 4 << 30
	textProfile.Rendition.TrustBoundary = string(textProvider.Descriptor().TrustBoundary)
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(),
		SpoolDirectory: t.TempDir(), MediaMaxBytes: 3 << 30, Profiles: map[string]ProfileConfig{
			name:   profile,
			"text": {Profile: textProfile, RenditionProvider: textProvider},
		}})
	require.NoError(t, err)
	for i, test := range []struct {
		filename, mediaType, profile string
		size                         int64
		long                         bool
		refusal                      string
	}{
		{"call.wav", "audio/wav", name, 2 << 30, true, ""},
		{"call.mp3", "audio/mpeg", name, 3 << 30, true, ""},
		{"call.wav", "audio/wav", name, 1 << 30, false, ""},
		{"call.wav", "audio/wav", name, (3 << 30) + 1, false, "source_bytes_exceeded"},
		{"call.m4a", "audio/mp4", name, 2 << 30, true, ""},
		{"call.m4a", "audio/x-m4a", name, (3 << 30) + 1, false, "source_bytes_exceeded"},
		{"notes.txt", "text/plain", "text", 2 << 30, false, "source_bytes_exceeded"},
	} {
		hash := fmt.Sprintf("%064x", i+1)
		node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), fmt.Sprintf("%d-%s", i, test.filename), hash, test.size,
			test.mediaType, store.BlobPhysical{Encoding: "raw", StoredBytes: test.size})
		require.NoError(t, err)
		selector := Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: test.profile}
		require.Equal(t, test.long, service.PlanStreamsLargeSource(t.Context(), selector), test.filename)
		if test.refusal != "" {
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err, test.filename)
			require.Equal(t, test.refusal, plan.RenditionIneligibleReason, test.filename)
		}
	}
	require.False(t, service.PlanStreamsLargeSource(t.Context(), Selector{NodeID: 1, ContentVersionID: "missing", Profile: name}))
}

func TestProviderRuntimeAuthorizesAfterPreparation(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	raw := mediatest.WAV()
	written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "call.wav", written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	name, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, "synthetic:operator")
	require.NoError(t, err)
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// Preparation of a large source on slow storage can outlast the 10-minute
	// interval. Time passes once the sealed spool copy exists.
	prepared := started.Add(11 * time.Minute)
	spool := t.TempDir()
	clock := func() time.Time {
		if entries, err := os.ReadDir(spool); err == nil && len(entries) > 0 {
			return prepared
		}
		return started
	}
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(), SpoolDirectory: spool,
		Clock: clock, Profiles: map[string]ProfileConfig{name: profile}})
	require.NoError(t, err)
	require.Equal(t, started, clock(), "the spool directory must start empty")
	work := store.RenditionJobWork{Profile: service.profiles[name].record, Job: store.RenditionJob{SourceSHA256: written.Hash}, Waiter: store.RenditionJobWaiter{ContentVersionID: node.CurrentVersionID}, ExecutionIdentity: document.RenditionExecutionIdentityV1{Upload: document.AuthorizedUploadMetadata{Filename: "call.wav", MediaType: "audio/wav", ByteLength: written.Size}}}
	execution, err := service.renditions.Prepare(t.Context(), work, started)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, execution.Upload.Close()) })
	_, err = document.SealRenditionExecutionAt(prepared, execution.Provider, execution.Upload,
		execution.Authorization, execution.EvidencePolicy, execution.RenditionPolicy)
	require.NoError(t, err)
}

func TestConfiguredCeilingChangeKeepsQueuedExecutionIdentity(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	raw := mediatest.WAV()
	written, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(raw))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "call.wav", written.Hash, written.Size, "audio/wav", processingBlobPhysical(t, written))
	require.NoError(t, err)
	version, err := f.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	name, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, "synthetic:operator")
	require.NoError(t, err)
	newCeilingService := func(maximum int64) *Service {
		service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(),
			SpoolDirectory: t.TempDir(), MediaMaxBytes: maximum, Profiles: map[string]ProfileConfig{name: profile}})
		require.NoError(t, err)
		return service
	}
	queuedUnder := newCeilingService(0)
	queued, err := queuedUnder.prepareRendition(t.Context(), node, version, queuedUnder.profiles[name], false)
	require.NoError(t, err)
	_, fingerprint, err := document.CanonicalRenditionExecutionIdentityV1(queued.identity)
	require.NoError(t, err)
	// The queued source stays within both ceilings, so changing the ceiling
	// must not change what the worker seals for it.
	for _, maximum := range []int64{1 << 20, 4 << 30} {
		service := newCeilingService(maximum)
		work := store.RenditionJobWork{Profile: service.profiles[name].record,
			Job:               store.RenditionJob{SourceSHA256: written.Hash, ExecutionIdentityFingerprint: fingerprint},
			Waiter:            store.RenditionJobWaiter{ContentVersionID: node.CurrentVersionID},
			ExecutionIdentity: queued.identity}
		execution, err := service.renditions.Prepare(t.Context(), renditionProviderRuntimeWork(work), service.clock())
		require.NoError(t, err, "ceiling %d", maximum)
		sealed, err := document.SealRenditionExecutionAt(service.clock(), execution.Provider, execution.Upload,
			execution.Authorization, execution.EvidencePolicy, execution.RenditionPolicy)
		require.NoError(t, err, "ceiling %d", maximum)
		_, err = renditionExecutionSnapshotForWork(work, sealed)
		require.NoError(t, err, "ceiling %d", maximum)
		require.NoError(t, execution.Upload.Close())
	}
}
