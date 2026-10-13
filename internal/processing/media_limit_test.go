package processing

import (
	"bytes"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
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
			if maximum == 0 {
				require.Equal(t, int64(4<<30), service.mediaMaxBytes)
			}
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

func TestSuppliedProfilesFourGiBSourceOnly(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	for _, constructor := range []func(*store.Store, *blob.Store, string) (string, ProfileConfig, error){NewSuppliedMediaProfile, NewSuppliedCaptionProfile} {
		_, profile, err := constructor(f.catalog, f.blobs, "synthetic:operator")
		require.NoError(t, err)
		require.Equal(t, int64(4<<30), profile.Profile.Rendition.MaxDocumentBytes)
		require.Equal(t, int64(16<<20), profile.Profile.Rendition.MaxResponseBytes)
	}
}

func TestAudioAdmissionUsesSmallerCeilingAndProfile(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	_, profile, err := NewSuppliedMediaProfile(f.catalog, f.blobs, "synthetic:operator")
	require.NoError(t, err)
	configured := configuredProfile{portable: profile.Profile, provider: profile.RenditionProvider, mediaMaxBytes: 3 << 30}
	version := store.ContentVersion{MimeType: "audio/wav"}
	require.Equal(t, int64(3<<30), inspectionPolicy("call.wav", version, configured).MaxSourceBytes)
	configured.portable.Rendition.MaxDocumentBytes = 2 << 30
	require.Equal(t, int64(2<<30), inspectionPolicy("call.wav", version, configured).MaxSourceBytes)
	version.MimeType = "text/plain"
	require.Equal(t, int64(1<<30), inspectionPolicy("notes.txt", version, configured).MaxSourceBytes)
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
