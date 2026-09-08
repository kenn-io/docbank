package geminiembed

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilesPolicyReturnsConfiguredImmutablePolicyWithoutCallbacks(t *testing.T) {
	tests := []struct {
		name      string
		transport Transport
		want      FilesPolicy
	}{
		{
			name: "inline", transport: TransportInline,
			want: FilesPolicy{Transport: TransportInline, Cleanup: "not_applicable"},
		},
		{
			name: "Files API", transport: TransportFilesAPI,
			want: FilesPolicy{Transport: TransportFilesAPI, RetentionCeiling: 48 * time.Hour,
				Cleanup: "attempted_delete_or_unconfirmed_retention_error"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := geminiTestProfile(t, 128)
			profile.Transport = test.transport
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

			policy := client.FilesPolicy()
			assert.Equal(t, test.want, policy)
			assert.Zero(t, secrets.calls.Load())
			assert.Zero(t, requests.Load())
			afterFingerprint, err := PolicyFingerprint(client.profile)
			require.NoError(t, err)
			assert.Equal(t, wantFingerprint, afterFingerprint)
			assert.Equal(t, wantDescriptor, client.Descriptor())

			policy.Transport = "mutated"
			policy.RetentionCeiling = time.Nanosecond
			policy.Cleanup = "mutated"
			assert.Equal(t, test.want, client.FilesPolicy())
		})
	}
}

func TestFilesPolicyNilReceiverReturnsZeroValue(t *testing.T) {
	var client *Client
	assert.Equal(t, FilesPolicy{}, client.FilesPolicy())
}
