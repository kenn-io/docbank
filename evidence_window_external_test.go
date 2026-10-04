package docbank_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank"
	"go.kenn.io/docbank/document/plaintext"
)

func TestEmbeddedEvidenceWindow(t *testing.T) {
	t.Parallel()
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
	require.NoError(t, err)
	vault, err := docbank.New(t.Context(), docbank.Config{Root: t.TempDir(), Processing: docbank.ProcessingOptions{Profiles: map[string]docbank.ProcessingProfileConfig{
		"private": {Profile: embeddedProcessingProfile(t, provider.Descriptor()), RenditionProvider: provider},
	}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	receipt, err := vault.Put(t.Context(), "/evidence.txt", strings.NewReader(strings.Repeat("aé界🙂z", 4000)), docbank.PutOptions{MediaType: "text/plain"})
	require.NoError(t, err)
	selector := docbank.ProcessingSelector{NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, Profile: "private"}
	plan, err := vault.PlanProcessing(t.Context(), docbank.ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.StartProcessing(t.Context(), docbank.StartProcessingRequest{PlanRequest: docbank.ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	rendition, err := vault.Rendition(t.Context(), docbank.RenditionRequest{Selector: selector})
	require.NoError(t, err)
	body, err := io.ReadAll(rendition.Reader)
	require.NoError(t, err)
	require.NoError(t, rendition.Reader.Close())
	request := docbank.EvidenceWindowRequest{VaultUID: vault.ID(), NodeID: receipt.Node.ID, ContentVersionID: receipt.Version.ID, ContentSHA256: receipt.Computed.SHA256, RenditionAttachmentID: rendition.AttachmentID, BuildID: rendition.BuildID, RenditionSHA256: rendition.SHA256}
	got, err := vault.ReadEvidenceWindow(t.Context(), request)
	require.NoError(t, err)
	runes := []rune(string(body))
	assert.Equal(t, string(runes[:8000]), got.Text)
	assert.False(t, got.EOF)
	assert.Equal(t, receipt.Version.ID, got.ContentVersionID)
	assert.Equal(t, receipt.Computed.SHA256, got.ContentSHA256)
	assert.Equal(t, rendition.AttachmentID, got.RenditionAttachmentID)
	assert.Equal(t, rendition.BuildID, got.BuildID)
	assert.Equal(t, rendition.SHA256, got.RenditionSHA256)
	assert.Equal(t, len(string(runes[:8000])), got.ResponseBytes)
	assert.Equal(t, "text/markdown", got.MediaType)
	request.MaxChars = 16000
	got, err = vault.ReadEvidenceWindow(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, string(runes[:16000]), got.Text)
	assert.Equal(t, 16000, got.NextOffset)
	require.Contains(t, string(body), "aé界🙂z")
	prefix, _, found := bytes.Cut(body, []byte("aé界🙂z"))
	require.True(t, found)
	offset := len([]rune(string(prefix)))
	request.Offset = offset + 1
	request.MaxChars = 3
	got, err = vault.ReadEvidenceWindow(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, "é界🙂", got.Text)
	assert.Equal(t, offset+1, got.ActualStart)
	assert.Equal(t, offset+4, got.ActualEnd)
	assert.Equal(t, offset+4, got.NextOffset)
	stale := request
	stale.BuildID = strings.Repeat("a", 64)
	got, err = vault.ReadEvidenceWindow(t.Context(), stale)
	require.ErrorIs(t, err, docbank.ErrEvidenceUnavailable)
	assert.Empty(t, got.Text)
	malformed := request
	malformed.ContentSHA256 = ""
	_, err = vault.ReadEvidenceWindow(t.Context(), malformed)
	require.ErrorIs(t, err, docbank.ErrInvalidEvidenceRequest)
	request.Offset = len([]rune(string(body))) + 1
	_, err = vault.ReadEvidenceWindow(t.Context(), request)
	require.ErrorIs(t, err, docbank.ErrInvalidRenditionWindow)
	require.NoError(t, vault.Close())
	_, err = vault.ReadEvidenceWindow(t.Context(), request)
	require.ErrorIs(t, err, docbank.ErrClosed)
}
