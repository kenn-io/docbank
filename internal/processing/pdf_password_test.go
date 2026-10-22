package processing

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

type passwordCountingProvider struct {
	document.RenditionProvider

	calls     int
	supported bool
	err       error
}

func (provider *passwordCountingProvider) PDFPasswords() ([]string, bool, error) {
	provider.calls++
	return nil, provider.supported, provider.err
}

func TestInspectionReadsPasswordsOnlyForEncryptedPDF(t *testing.T) {
	t.Parallel()
	encrypted, err := mediatest.EncryptedPDF("open", "owner", 128)
	require.NoError(t, err)
	ownerOnly, err := mediatest.EncryptedPDF("", "owner", 128)
	require.NoError(t, err)
	passwordErr := errors.New("password file read failed")
	for _, tc := range []struct {
		name, mime string
		data       []byte
		supported  bool
		calls      int
		reason     media.CapabilityReason
		err        error
	}{
		{"plain.pdf", "application/pdf", mediatest.PDF(), true, 0, media.CapabilityReasonEligible, nil},
		{"audio.wav", "audio/wav", mediatest.WAV(), true, 0, media.CapabilityReasonEligible, nil},
		{"locked.pdf", "application/pdf", encrypted, false, 1, media.CapabilityReasonEncryptedContainer, nil},
		{"supported.pdf", "application/pdf", encrypted, true, 1, media.CapabilityReasonPDFPasswordRequired, nil},
		{"owner-only.pdf", "application/pdf", ownerOnly, true, 1, media.CapabilityReasonPDFPasswordRequired, nil},
		{"unreadable.pdf", "application/pdf", encrypted, true, 1, "", passwordErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			provider := &passwordCountingProvider{RenditionProvider: newWorkerProvider(t), supported: tc.supported, err: tc.err}
			stored := workerProcessingProfile(t, provider.Descriptor())
			var portable document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(stored.CanonicalProfile, &portable))
			profile := configuredProfile{portable: portable, record: stored, provider: provider}
			written, err := fixture.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(tc.data))
			require.NoError(t, err)
			node, err := fixture.catalog.CreateFile(t.Context(), fixture.catalog.RootID(), tc.name, written.Hash, written.Size, tc.mime, processingBlobPhysical(t, written))
			require.NoError(t, err)
			version, err := fixture.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
			require.NoError(t, err)
			capability, err := inspectRenditionSource(t.Context(), fixture.blobs, node, version, profile)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				require.Equal(t, tc.calls, provider.calls)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.reason, capability.Reason)
			require.Equal(t, tc.calls, provider.calls)
			if tc.reason == media.CapabilityReasonPDFPasswordRequired {
				_, err = prepareProviderExecution(t.Context(), fixture.blobs, t.TempDir(), node, version, profile, time.Now, false)
				require.NoError(t, err)
				_, err = prepareProviderExecution(t.Context(), fixture.blobs, t.TempDir(), node, version, profile, time.Now, true)
				require.ErrorIs(t, err, ErrPDFPasswordRequired)
			}
		})
	}
}
