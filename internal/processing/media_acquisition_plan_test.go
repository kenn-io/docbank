package processing

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

// TestMediaAcquisitionPlanRejectsTamperingAndDifferentKey catches unsigned
// claim changes and daemon-restart token replay.
func TestMediaAcquisitionPlanRejectsTamperingAndDifferentKey(t *testing.T) {
	t.Parallel()
	var key [32]byte
	key[0] = 1
	body := []byte(`{"origin_id":"synthetic","principal":"operator"}`)
	token, err := signMediaToken(key, body)
	require.NoError(t, err)
	got, err := verifyMediaToken(key, token)
	require.NoError(t, err)
	require.Equal(t, body, got)

	_, signature, _ := strings.Cut(token, ".")
	tampered := base64.RawURLEncoding.EncodeToString([]byte(`{"principal":"other"}`)) + "." + signature
	_, err = verifyMediaToken(key, tampered)
	require.Error(t, err)
	key[0] = 2
	_, err = verifyMediaToken(key, token)
	require.Error(t, err)
}

func TestMediaAcquisitionPlanBindsCurrentPolicyPrincipalIncarnationAndExpiry(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	var key [32]byte
	key[0] = 9
	policy := MediaOriginPolicy{OriginID: "synthetic", Provider: "synthetic",
		ResolverFingerprint: processingHash("resolver-v1"), IdentityFingerprint: processingHash("identity-v1"),
		DisclosureFingerprint: processingHash("disclosure-v1"), InputClasses: []string{"recording_reference"},
		RetainedClasses:   []string{"recording_bytes", "caption_input"},
		ReferencePrefixes: []string{"https://recordings.invalid/"}, AcquisitionAvailable: true}
	newService := func(principal string, origins map[string]MediaOriginPolicy) *Service {
		service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
			Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: principal,
			MediaTokenKey: key, MediaOrigins: origins,
			Clock: func() time.Time { return now }})
		require.NoError(t, err)
		return service
	}
	service := newService("daemon:operator", map[string]MediaOriginPolicy{"synthetic": policy})
	request := RemoteRecordingRequest{ReferenceURL: "https://recordings.invalid/private-id?token=SECRET"}
	plan, err := service.PlanMediaAcquisition(t.Context(), request)
	require.NoError(t, err)
	require.NotContains(t, plan.PlanToken, "private-id")
	require.NotContains(t, plan.PlanToken, "SECRET")
	require.Equal(t, "required", plan.GrantState)

	grantReceipt, err := service.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000201", plan.PlanToken, nil)
	require.NoError(t, err)
	granted, err := service.PlanMediaAcquisition(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "granted", granted.GrantState)
	other, err := service.PlanMediaAcquisition(t.Context(), RemoteRecordingRequest{
		ReferenceURL: "https://recordings.invalid/different-private-id"})
	require.NoError(t, err)
	require.Equal(t, "required", other.GrantState)

	body, encodedSignature, _ := strings.Cut(plan.PlanToken, ".")
	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	require.NoError(t, err)
	signature[0] ^= 1
	tampered := body + "." + base64.RawURLEncoding.EncodeToString(signature)
	_, err = service.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000202", tampered, nil)
	require.ErrorIs(t, err, ErrMediaPlanInvalid)

	changed := policy
	changed.RetainedClasses = append(changed.RetainedClasses, "provider_metadata")
	staleService := newService("daemon:operator", map[string]MediaOriginPolicy{"synthetic": changed})
	_, err = staleService.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000203", plan.PlanToken, nil)
	require.ErrorIs(t, err, ErrMediaPlanInvalid)

	wrongPrincipal := newService("mcp:agent", map[string]MediaOriginPolicy{"synthetic": policy})
	_, err = wrongPrincipal.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000204", plan.PlanToken, nil)
	require.ErrorIs(t, err, ErrMediaPlanInvalid)

	now = now.Add(15 * time.Minute)
	replayed, err := service.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000201", plan.PlanToken, nil)
	require.NoError(t, err)
	require.NotEmpty(t, replayed.GrantID)
	future := now.Add(time.Hour)
	_, err = service.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000201", plan.PlanToken, &future)
	require.ErrorIs(t, err, store.ErrMediaOperationConflict)
	_, err = service.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000205", plan.PlanToken, nil)
	require.ErrorIs(t, err, ErrMediaPlanExpired)

	// Planning, granting and revoking touch only local catalog authority. M1
	// deliberately has no acquisition provider or network callback to invoke.
	revoked, err := service.RevokeMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000206", "synthetic")
	require.NoError(t, err)
	require.NotEmpty(t, revoked.RevokedAt)
	require.NotErrorIs(t, err, ErrMediaCapabilityUnavailable)
	withoutOrigins := newService("daemon:operator", nil)
	replayedGrant, err := withoutOrigins.GrantMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000201", plan.PlanToken, nil)
	require.NoError(t, err)
	require.Equal(t, grantReceipt, replayedGrant)
	_, err = withoutOrigins.GrantMediaAcquisition(t.Context(), uuid.New().String(), plan.PlanToken, nil)
	require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
	afterReplay, err := service.PlanMediaAcquisition(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "required", afterReplay.GrantState, "replaying a receipt must not restore revoked consent")
	replayedRevocation, err := withoutOrigins.RevokeMediaAcquisition(t.Context(),
		"00000000-0000-4000-8000-000000000206", "synthetic")
	require.NoError(t, err)
	require.Equal(t, revoked, replayedRevocation)
}

func TestRemoteRecordingReplaysBeforeOriginAdmission(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	policy := MediaOriginPolicy{OriginID: "original", Provider: "synthetic",
		ReferencePrefixes: []string{"https://recordings.invalid/"}}
	newService := func(principal string, origins map[string]MediaOriginPolicy) *Service {
		service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
			Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: principal,
			MediaOrigins: origins, MediaTokenKey: [32]byte{1}})
		require.NoError(t, err)
		return service
	}
	request := RemoteRecordingRequest{OperationID: uuid.New().String(),
		ReferenceURL: "https://recordings.invalid/call", CredentialBinding: "synthetic-binding",
		Occurrence: MediaOccurrenceInput{Ref: "call", Revision: "1", Filename: "call.wav"}}
	service := newService("operator:owner", map[string]MediaOriginPolicy{policy.OriginID: policy})
	first, err := service.SubmitRemoteRecording(t.Context(), request)
	require.NoError(t, err)
	changed := policy
	changed.OriginID, changed.Provider = "replacement", "replacement-provider"
	unrecognized := policy
	unrecognized.ReferencePrefixes = []string{"https://other.invalid/"}
	for _, test := range []struct {
		name    string
		origins map[string]MediaOriginPolicy
		admit   bool
	}{
		{"removed", nil, false},
		{"changed identity", map[string]MediaOriginPolicy{changed.OriginID: changed}, true},
		{"changed recognition", map[string]MediaOriginPolicy{unrecognized.OriginID: unrecognized}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			restarted := newService("operator:owner", test.origins)
			replayed, err := restarted.SubmitRemoteRecording(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, first, replayed)
			fresh := request
			fresh.OperationID, fresh.Occurrence.Ref = uuid.New().String(), "new-call"
			retained, err := restarted.SubmitRemoteRecording(t.Context(), fresh)
			if test.admit {
				require.NoError(t, err)
				require.NotEqual(t, first.SourceID, retained.SourceID)
			} else {
				require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
			}
		})
	}
	withoutOrigins := newService("operator:owner", nil)
	for _, field := range []string{"reference", "provider hint", "credential binding", "occurrence", "acquire", "processing"} {
		t.Run("changed request/"+field, func(t *testing.T) {
			changed := request
			switch field {
			case "reference":
				changed.ReferenceURL += "?revision=2"
			case "provider hint":
				changed.ProviderHint = policy.Provider
			case "credential binding":
				changed.CredentialBinding = "different-binding"
			case "occurrence":
				changed.Occurrence.Filename = "different.wav"
			case "acquire":
				changed.Acquire = true
			case "processing":
				changed.Processing = &MediaProcessingRequest{Profile: "speech"}
			}
			_, err := withoutOrigins.SubmitRemoteRecording(t.Context(), changed)
			require.ErrorIs(t, err, store.ErrMediaOperationConflict)
		})
	}
	other := newService("operator:other", nil)
	_, err = other.SubmitRemoteRecording(t.Context(), request)
	require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
}

func TestRemoteRecordingAcquireUnavailable(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	service := newRemoteRecordingTestService(t, fixture, "operator:acquire", 0,
		remoteRecordingTestOrigins())
	request := remoteRecordingTestRequest(uuid.New().String(),
		"https://recordings.invalid/acquire", "", "acquire")
	request.Acquire = true
	_, err := service.SubmitRemoteRecording(t.Context(), request)
	require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
	canonical := request
	canonical.OperationID = uuid.New().String()
	canonical.CanonicalURL = "https://recordings.invalid/acquire"
	_, err = service.SubmitRemoteRecording(t.Context(), canonical)
	require.ErrorIs(t, err, ErrMediaCapabilityUnavailable)
	hinted := canonical
	hinted.OperationID = uuid.New().String()
	hinted.Acquire = false
	hinted.ProviderHint = strings.Repeat("h", 129)
	_, err = service.SubmitRemoteRecording(t.Context(), hinted)
	require.ErrorContains(t, err, "provider hint")
	hinted = canonical
	hinted.OperationID = uuid.New().String()
	hinted.Acquire = false
	hinted.CredentialBinding = strings.Repeat("c", 257)
	_, err = service.SubmitRemoteRecording(t.Context(), hinted)
	require.ErrorContains(t, err, "credential binding")
	items, total, err := fixture.catalog.MediaSources(t.Context(), service.principal, 0, 10)
	require.NoError(t, err)
	require.Empty(t, items)
	require.Zero(t, total)
	var metadata bytes.Buffer
	require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &metadata))
	require.NotContains(t, metadata.String(), `"type":"media_acquisition_receipt"`)
}
