package processing

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/capselfhosted"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestRegisteredCapOriginSubmission(t *testing.T) {
	fixture := newPublicationFixture(t)
	generic := newRemoteRecordingTestService(t, fixture, "operator:generic", 0, nil)
	legacyRequest := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1?token=synthetic", "before")
	legacyRequest.CanonicalURL = "https://cap.example.test/s/vid-1?token=synthetic"
	legacy, err := generic.SubmitRemoteRecording(t.Context(), legacyRequest)
	require.NoError(t, err)
	require.Equal(t, "unsupported", legacy.Outcome)
	raw := mediatest.WAV()
	imported, err := generic.ImportRecordingArtifact(t.Context(), remoteRecordingTestArtifact(
		uuid.New().String(), legacy.SourceID, legacy.OccurrenceID, "call.wav", "audio/wav",
		processingSHA256(raw), int64(len(raw)), raw))
	require.NoError(t, err)
	require.NotEmpty(t, imported.SourceVersionID)

	service := newRemoteRecordingTestService(t, fixture, "operator:generic", 0,
		map[string]MediaOriginPolicy{"team-cap": capOriginPolicy("team-cap", "credential:cap-a")})
	replayed, err := service.SubmitRemoteRecording(t.Context(), legacyRequest)
	require.NoError(t, err)
	require.Equal(t, legacy.SourceID, replayed.SourceID)
	newCapRequest := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1", "registered-after-import")
	newCap, err := service.SubmitRemoteRecording(t.Context(), newCapRequest)
	require.NoError(t, err)
	require.NotEqual(t, legacy.SourceID, newCap.SourceID)
	legacyRows, err := service.ListMediaSources(t.Context(), MediaListOptions{Limit: 20})
	require.NoError(t, err)
	var legacyRow MediaSourceRow
	for _, row := range legacyRows.Items {
		if row.SourceID == legacy.SourceID {
			legacyRow = row
			break
		}
	}
	require.Equal(t, imported.SourceVersionID, legacyRow.SourceVersionID)
	variants := []struct {
		name, reference, canonical string
	}{
		{"share-case", "https://CAP.example.test:443/s/vid-1?token=x", "https://cap.example.test/s/vid-1?canonical=1"},
		{"embed-fragment", "https://cap.example.test/embed/vid-1#t", "https://CAP.example.test:443/embed/vid-1"},
		{"share-without-canonical", "https://cap.example.test/s/vid-1?token=y", ""},
		{"canonical-recognizes-reference-with-trailing-slash", "https://cap.example.test/s/vid-1/", "https://cap.example.test/s/vid-1"},
	}
	var first MediaReceipt
	for index, variant := range variants {
		request := capRecordingRequest(uuid.New().String(), variant.reference, variant.name)
		request.CanonicalURL = variant.canonical
		request.CredentialBinding = "credential:cap-a"
		receipt, submitErr := service.SubmitRemoteRecording(t.Context(), request)
		require.NoError(t, submitErr)
		require.Equal(t, "access_required", receipt.Outcome)
		if index == 0 {
			first = receipt
		} else {
			require.Equal(t, first.SourceID, receipt.SourceID)
		}
	}
	require.NotEqual(t, legacy.SourceID, first.SourceID)

	for _, test := range []struct {
		name, canonical string
	}{
		{"mismatched origin", "https://other.example/s/vid-1"},
		{"mismatched video", "https://cap.example.test/s/vid-2"},
	} {
		request := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1", test.name)
		request.CanonicalURL = test.canonical
		_, err := service.SubmitRemoteRecording(t.Context(), request)
		require.ErrorIs(t, err, ErrMediaPlanInvalid)
	}

	var metadata strings.Builder
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
	require.Contains(t, metadata.String(), `"provider":"cap.self-hosted"`)
	require.NotContains(t, metadata.String(), "cap.example.test")
}

func TestMediaOriginLegacyFingerprintRemainsPinned(t *testing.T) {
	got, err := mediaOriginFingerprint(MediaOriginPolicy{
		OriginID: "legacy", Provider: "url", ResolverFingerprint: "r", IdentityFingerprint: "i", DisclosureFingerprint: "d",
		InputClasses: []string{"recording_reference"}, ReferencePrefixes: []string{"https://legacy.example/"},
	})
	require.NoError(t, err)
	require.Equal(t, "c45fdf8b79b729681981f44b97f2799af8081026cc4faa6c336071aa3b740ed4", got)
}

func TestRegisteredCapOriginRejectsLookalikes(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:boundary", 0,
		map[string]MediaOriginPolicy{"team-cap": capOriginPolicy("team-cap", "credential:cap-a")})
	lookalikes := []string{
		"https://cap.example.test.evil.example/s/vid-1",
		"https://cap.example.test:8443/s/vid-1",
		"http://cap.example.test/s/vid-1",
		"https://user@cap.example.test/s/vid-1",
		"https://cap.example.test/share/vid-1",
		"https://other.example/s/vid-1",
	}
	for index, reference := range lookalikes {
		t.Run(reference, func(t *testing.T) {
			withCanonical := capRecordingRequest(uuid.New().String(), reference, "with-canonical-"+strconv.Itoa(index))
			withCanonical.CanonicalURL = "https://other.example/reference-" + string(rune('a'+index))
			receipt, err := service.SubmitRemoteRecording(t.Context(), withCanonical)
			require.NoError(t, err)
			require.Equal(t, "unsupported", receipt.Outcome)

			withoutCanonical := capRecordingRequest(uuid.New().String(), reference, "without-canonical-"+strconv.Itoa(index))
			_, err = service.SubmitRemoteRecording(t.Context(), withoutCanonical)
			require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
		})
	}
}

func TestRegisteredCapOriginCredentialScope(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:credentials", 0,
		map[string]MediaOriginPolicy{"cap-a": capOriginPolicy("cap-a", "credential:cap-a")})
	foreign := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1", "foreign")
	foreign.CredentialBinding = "credential:cap-b"
	_, err := service.SubmitRemoteRecording(t.Context(), foreign)
	require.ErrorIs(t, err, ErrMediaCredentialScope)
	hinted := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1", "hinted-url")
	hinted.ProviderHint = "url"
	_, err = service.SubmitRemoteRecording(t.Context(), hinted)
	require.ErrorIs(t, err, ErrMediaPlanInvalid)
	canonicalOnly := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1/", "foreign-canonical")
	canonicalOnly.CanonicalURL = "https://cap.example.test/s/vid-1"
	canonicalOnly.CredentialBinding = "credential:cap-b"
	_, err = service.SubmitRemoteRecording(t.Context(), canonicalOnly)
	require.ErrorIs(t, err, ErrMediaCredentialScope)
	page, err := service.ListMediaSources(t.Context(), MediaListOptions{Limit: 10})
	require.NoError(t, err)
	require.Zero(t, page.Total)

	for index, binding := range []string{"", "credential:cap-a"} {
		request := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1", "accepted-"+string(rune('a'+index)))
		request.CredentialBinding = binding
		_, err := service.SubmitRemoteRecording(t.Context(), request)
		require.NoError(t, err)
	}
	generic := capRecordingRequest(uuid.New().String(), "https://other.example/reference", "generic")
	generic.CanonicalURL = "https://other.example/reference"
	generic.CredentialBinding = "credential:cap-b"
	receipt, err := service.SubmitRemoteRecording(t.Context(), generic)
	require.NoError(t, err)
	require.Equal(t, "unsupported", receipt.Outcome)
}

func TestRegisteredCapOriginPlanStaysUnavailable(t *testing.T) {
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:plan", 0,
		map[string]MediaOriginPolicy{"cap": capOriginPolicy("cap", "credential:cap-a")})
	request := capRecordingRequest(uuid.New().String(), "https://cap.example.test/s/vid-1", "acquire")
	request.Acquire = true
	_, err := service.SubmitRemoteRecording(t.Context(), request)
	require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
	_, err = service.PlanMediaAcquisition(t.Context(), request)
	require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
	page, err := service.ListMediaSources(t.Context(), MediaListOptions{Limit: 10})
	require.NoError(t, err)
	require.Zero(t, page.Total)
}

func TestProbeMediaOriginsRecordsEvidence(t *testing.T) {
	fixture := newPublicationFixture(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	origins := map[string]MediaOriginPolicy{
		"a": capOriginPolicy("a", "credential:cap-a"),
		"b": capOriginPolicy("b", "credential:cap-b"),
		"c": capOriginPolicy("c", "credential:cap-c"),
	}
	originB := origins["b"]
	originB.ExactOrigin = "https://cap-b.example.test"
	origins["b"] = originB
	originC := origins["c"]
	originC.ExactOrigin = "https://cap-c.example.test"
	origins["c"] = originC
	probes := map[string]MediaOriginProbe{
		"a": func(context.Context) (MediaOriginEvidence, error) {
			return MediaOriginEvidence{AdapterContract: capselfhosted.AdapterContract,
				DeploymentRevision: "v-synthetic", ProbeState: string(capselfhosted.ProbeVerified)}, nil
		},
		"b": func(context.Context) (MediaOriginEvidence, error) {
			return MediaOriginEvidence{}, errors.New("synthetic probe failure")
		},
		"c": func(context.Context) (MediaOriginEvidence, error) {
			return MediaOriginEvidence{AdapterContract: capselfhosted.AdapterContract,
				DeploymentRevision: "v-synthetic", ProbeState: string(capselfhosted.ProbeProviderUnavailable)}, nil
		},
	}
	service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:probes",
		MediaOrigins: origins, MediaOriginProbes: probes, MediaTokenKey: [32]byte{1},
		Clock: func() time.Time { return now }})
	require.NoError(t, err)
	before, err := service.MediaOrigins()
	require.NoError(t, err)
	require.Equal(t, "pending", before[0].ProbeState)
	require.NoError(t, service.ProbeMediaOrigins(t.Context()))
	after, err := service.MediaOrigins()
	require.NoError(t, err)
	require.Equal(t, "verified", after[0].ProbeState)
	require.Equal(t, "v-synthetic", after[0].DeploymentRevision)
	require.Equal(t, "provider_unavailable", after[1].ProbeState)
	require.Equal(t, "provider_unavailable", after[2].ProbeState)
	require.Equal(t, now, after[0].ProbedAt)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, service.ProbeMediaOrigins(canceled), context.Canceled)

	_, err = NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), MediaOrigins: origins,
		MediaOriginProbes: map[string]MediaOriginProbe{"missing": probes["a"]}, MediaTokenKey: [32]byte{1}})
	require.ErrorContains(t, err, "has no registered origin")
}

func TestNewServiceRejectsMediaOriginGuards(t *testing.T) {
	fixture := newPublicationFixture(t)
	base := func() ServiceConfig {
		return ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: newWorkerTestGate(),
			SpoolDirectory: t.TempDir(), Principal: "operator:guards", MediaTokenKey: [32]byte{1},
			MediaOrigins: map[string]MediaOriginPolicy{"team-cap": capOriginPolicy("team-cap", "credential:cap")}}
	}
	for _, test := range []struct {
		name string
		edit func(*ServiceConfig)
		want string
	}{
		{name: "nil probe", edit: func(cfg *ServiceConfig) {
			cfg.MediaOriginProbes = map[string]MediaOriginProbe{"team-cap": nil}
		}, want: "probe \"team-cap\" is nil"},
		{name: "missing recognizer", edit: func(cfg *ServiceConfig) {
			policy := cfg.MediaOrigins["team-cap"]
			policy.RecognizePath = nil
			cfg.MediaOrigins["team-cap"] = policy
		}, want: "has no recognizer"},
		{name: "noncanonical origin", edit: func(cfg *ServiceConfig) {
			policy := cfg.MediaOrigins["team-cap"]
			policy.ExactOrigin = "HTTPS://cap.example.test:443"
			cfg.MediaOrigins["team-cap"] = policy
		}, want: "exact origin is not canonical"},
		{name: "duplicate origin", edit: func(cfg *ServiceConfig) {
			cfg.MediaOrigins["other"] = capOriginPolicy("other", "credential:other")
		}, want: "duplicate exact origin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base()
			test.edit(&cfg)
			_, err := NewService(cfg)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func capOriginPolicy(id, binding string) MediaOriginPolicy {
	return MediaOriginPolicy{OriginID: id, Provider: capselfhosted.Provider,
		ResolverFingerprint: processingHash(id + ":resolver"), IdentityFingerprint: processingHash(id + ":identity"),
		DisclosureFingerprint: processingHash(id + ":disclosure"), InputClasses: []string{"recording_reference"},
		ExactOrigin: "https://cap.example.test", CredentialBinding: binding,
		RecognizePath: capselfhosted.RecognizeSharePath, AcquisitionAvailable: false}
}

func capRecordingRequest(operationID, reference, occurrence string) RemoteRecordingRequest {
	return RemoteRecordingRequest{OperationID: operationID, ReferenceURL: reference,
		ProviderHint: capselfhosted.Provider, Occurrence: MediaOccurrenceInput{Ref: occurrence,
			Revision: "1", Filename: occurrence + ".wav"}}
}
