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
	passwords []string
	supported bool
	err       error
}

func (provider *passwordCountingProvider) PDFPasswords() ([]string, bool, error) {
	provider.calls++
	return provider.passwords, provider.supported, provider.err
}

func TestInspectionAndPrepareReadPasswordsOnlyForEncryptedPDF(t *testing.T) {
	t.Parallel()
	encrypted, err := mediatest.EncryptedPDF("open", "owner", 128)
	require.NoError(t, err)
	ownerOnly, err := mediatest.EncryptedPDF("", "owner", 128)
	require.NoError(t, err)
	container := syntheticOOXMLFamily(t, "ppt/presentation.xml")
	// Mark the first ZIP entry encrypted in both headers, as in the media inspection test.
	container[6] |= 1
	central := bytes.Index(container, []byte("PK\x01\x02"))
	require.NotEqual(t, -1, central)
	container[central+8] |= 1
	passwordErr := errors.New("password file read failed")
	for _, tc := range []struct {
		name, mime string
		data       []byte
		supported  bool
		calls      int
		reason     media.CapabilityReason
		err        error
		passwords  []string
	}{
		{"plain.pdf", "application/pdf", mediatest.PDF(), true, 0, media.CapabilityReasonEligible, nil, nil},
		{"encrypted.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", container, true, 0, media.CapabilityReasonEncryptedContainer, nil, nil},
		{"locked.pdf", "application/pdf", encrypted, false, 1, media.CapabilityReasonEncryptedContainer, nil, nil},
		{"supported.pdf", "application/pdf", encrypted, true, 1, media.CapabilityReasonPDFPasswordRequired, nil, nil},
		{"empty-passwords-owner-only.pdf", "application/pdf", ownerOnly, true, 1, media.CapabilityReasonEligible, nil, []string{}},
		{"unreadable.pdf", "application/pdf", encrypted, true, 1, "", passwordErr, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			provider := &passwordCountingProvider{RenditionProvider: newWorkerProvider(t), passwords: tc.passwords, supported: tc.supported, err: tc.err}
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
				require.ErrorIs(t, err, ErrPDFPasswordsUnreadable)
				require.ErrorIs(t, err, tc.err)
				require.ErrorIs(t, err, ErrRenditionOperatorRequired)
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
