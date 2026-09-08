package geminiembed

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadPolicyReturnsNormalizedImmutableAuthorityWithoutCallbacks(t *testing.T) {
	profile := geminiTestProfile(t, 128)
	profile.MaxInputBytes = 0
	profile.Descriptor = geminiDescriptorFor(t, profile)
	wantFingerprint, err := PolicyFingerprint(profile)
	require.NoError(t, err)
	wantDescriptor := profile.Descriptor
	secrets := &countingSecrets{value: "synthetic-key"}
	requests := new(atomic.Int32)
	client := newGeminiTestClient(t, profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected synthetic egress")
	}))

	profile.CapabilityProfileFingerprint = testDisclosurePolicy
	profile.DisclosureFingerprint = testCapabilityProfile
	profile.MaxInputBytes = 7
	policy := client.UploadPolicy()

	assert.Equal(t, UploadPolicy{
		CapabilityProfileFingerprint: testCapabilityProfile,
		DisclosureFingerprint:        testDisclosurePolicy,
		MaxInputBytes:                defaultInputBytes,
		MaxSourceFrames:              10_000,
	}, policy)
	assert.Zero(t, secrets.calls.Load())
	assert.Zero(t, requests.Load())
	afterFingerprint, err := PolicyFingerprint(client.profile)
	require.NoError(t, err)
	assert.Equal(t, wantFingerprint, afterFingerprint)
	assert.Equal(t, wantDescriptor, client.Descriptor())

	policy.CapabilityProfileFingerprint = testDisclosurePolicy
	policy.DisclosureFingerprint = testCapabilityProfile
	policy.MaxInputBytes = 1
	policy.MaxSourceFrames = 1
	assert.Equal(t, UploadPolicy{
		CapabilityProfileFingerprint: testCapabilityProfile,
		DisclosureFingerprint:        testDisclosurePolicy,
		MaxInputBytes:                defaultInputBytes,
		MaxSourceFrames:              10_000,
	}, client.UploadPolicy())
}

func TestUploadPolicyNilReceiverReturnsZeroValue(t *testing.T) {
	var client *Client
	assert.Equal(t, UploadPolicy{}, client.UploadPolicy())
}
