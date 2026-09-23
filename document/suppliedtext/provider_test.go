package suppliedtext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestSuppliedTextBindingSeparatesSamePDFWithDifferentText(t *testing.T) {
	source, policy := strings.Repeat("a", 64), strings.Repeat("f", 64)
	first, err := SourceBinding("p", "row-a", source, strings.Repeat("b", 64), "utf-8", policy)
	require.NoError(t, err)
	repeat, err := SourceBinding("p", "row-a", source, strings.Repeat("b", 64), "utf-8", policy)
	require.NoError(t, err)
	second, err := SourceBinding("p", "row-b", source, strings.Repeat("c", 64), "utf-8", policy)
	require.NoError(t, err)
	require.Equal(t, first, repeat)
	require.NotEqual(t, first, second)
	for _, changed := range [][]string{
		{"other-package", "row-a", source, strings.Repeat("b", 64), "utf-8", policy},
		{"p", "row-b", source, strings.Repeat("b", 64), "utf-8", policy},
		{"p", "row-a", strings.Repeat("c", 64), strings.Repeat("b", 64), "utf-8", policy},
		{"p", "row-a", source, strings.Repeat("c", 64), "utf-8", policy},
		{"p", "row-a", source, strings.Repeat("b", 64), "windows-1252", policy},
		{"p", "row-a", source, strings.Repeat("b", 64), "utf-8", strings.Repeat("e", 64)},
	} {
		binding, err := SourceBinding(changed[0], changed[1], changed[2], changed[3], changed[4], changed[5])
		require.NoError(t, err)
		require.NotEqual(t, first, binding)
	}
	for _, args := range [][]string{
		{"", "row-a", source, strings.Repeat("b", 64), "utf-8", policy},
		{"p", "", source, strings.Repeat("b", 64), "utf-8", policy},
		{"p", "row-a", "bad", strings.Repeat("b", 64), "utf-8", policy},
		{"p", "row-a", source, strings.Repeat("b", 64), "", policy},
		{"p", "row-a", source, strings.Repeat("b", 64), "utf-8", "bad"},
	} {
		_, err := SourceBinding(args[0], args[1], args[2], args[3], args[4], args[5])
		require.Error(t, err)
	}
}

func TestSuppliedTextIsIndexedAndBlankTextCallsNoNativeExtractor(t *testing.T) {
	source := &countingSource{text: document.SuppliedText{Provider: "package", Text: "quenchwood ledger entry"}}
	provider, err := New(Profile{Source: source, SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 4096})
	require.NoError(t, err)
	require.Equal(t, "supplied-text.in-process-v1", provider.Descriptor().ID)
	upload := newTestUpload("pdf", "application/pdf")
	result, err := document.RenderRendition(t.Context(), provider, upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 1)
	require.Contains(t, result.Evidence.Units[0].Text, "quenchwood")
	require.Equal(t, "pdf", result.Evidence.Family)
	require.Equal(t, 1, source.calls)
	require.Contains(t, result.Receipt.Warnings, "degraded_provenance")
	require.Equal(t, upload.Metadata().SHA256, source.lastDigest)

	blank := &countingSource{}
	blankProvider, err := New(Profile{Source: blank, SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 4096})
	require.NoError(t, err)
	_, err = document.RenderRendition(t.Context(), blankProvider, newTestUpload("pdf", "application/pdf"), testAuthorization(blankProvider.Descriptor(), upload.Metadata()))
	var providerErr *document.RenditionProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, document.RenditionErrorUnsupportedInput, providerErr.Code())
	require.Equal(t, 1, blank.calls)
	directUpload := newTestUpload("pdf", "application/pdf")
	_, err = blankProvider.Render(t.Context(), directUpload, testAuthorization(blankProvider.Descriptor(), directUpload.Metadata()))
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, document.RenditionErrorUnsupportedInput, providerErr.Code())
	require.Zero(t, directUpload.reads)

	whitespace := &countingSource{text: document.SuppliedText{Provider: "package", Text: " \n"}}
	whitespaceProvider, err := New(Profile{Source: whitespace, SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 4096})
	require.NoError(t, err)
	_, err = document.RenderRendition(t.Context(), whitespaceProvider, newTestUpload("pdf", "application/pdf"), testAuthorization(whitespaceProvider.Descriptor(), upload.Metadata()))
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, document.RenditionErrorUnsupportedInput, providerErr.Code())
}

func TestSuppliedTextProviderRejectsDifferentInputBindingBeforeSourceCall(t *testing.T) {
	source := &countingSource{text: document.SuppliedText{Provider: "package", Text: "word"}}
	provider, err := New(Profile{Source: source, SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 100})
	require.NoError(t, err)
	upload := newTestUpload("pdf", "application/pdf")
	upload.metadata.InputBinding = strings.Repeat("b", 64)
	_, err = document.RenderRendition(t.Context(), provider, upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
	var providerErr *document.RenditionProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, document.RenditionErrorPolicyRejected, providerErr.Code())
	require.Zero(t, source.calls)
}

func TestSuppliedTextProviderIdentityAndFormats(t *testing.T) {
	base := Profile{Source: &countingSource{}, SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 100}
	first, err := New(base)
	require.NoError(t, err)
	repeat, err := New(base)
	require.NoError(t, err)
	require.Equal(t, first.Descriptor(), repeat.Descriptor())
	base.SourceBinding = strings.Repeat("b", 64)
	other, err := New(base)
	require.NoError(t, err)
	require.NotEqual(t, first.Descriptor().Fingerprint, other.Descriptor().Fingerprint)
	base.SourceBinding = strings.Repeat("a", 64)
	base.MaxDocumentChars++
	otherPolicy, err := New(base)
	require.NoError(t, err)
	require.NotEqual(t, first.Descriptor().PolicyFingerprint, otherPolicy.Descriptor().PolicyFingerprint)
	formats := first.Descriptor().SupportedFormats
	require.Len(t, formats, 4)
	for _, mediaType := range []string{"application/pdf", "image/tiff", "text/plain", "application/json"} {
		found := false
		for _, format := range formats {
			found = found || format.MediaType == mediaType && format.InputKind == document.RenditionInputOriginalFile
		}
		require.True(t, found, mediaType)
	}
	_, err = New(Profile{SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 100})
	require.Error(t, err)
	_, err = New(Profile{Source: &countingSource{}, SourceBinding: "bad", MaxDocumentChars: 100})
	require.Error(t, err)
}

func TestSuppliedTextRendersEveryDeclaredFormat(t *testing.T) {
	for _, format := range []struct{ family, mediaType string }{
		{"pdf", "application/pdf"}, {"image", "image/tiff"},
		{"text", "text/plain"}, {"structured", "application/json"},
	} {
		t.Run(format.mediaType, func(t *testing.T) {
			provider, err := New(Profile{Source: &countingSource{text: document.SuppliedText{Provider: "package", Text: "sender text"}}, SourceBinding: strings.Repeat("a", 64), MaxDocumentChars: 100})
			require.NoError(t, err)
			upload := newTestUpload(format.family, format.mediaType)
			result, err := document.RenderRendition(t.Context(), provider, upload, testAuthorization(provider.Descriptor(), upload.Metadata()))
			require.NoError(t, err)
			require.Equal(t, format.family, result.Evidence.Family)
		})
	}
}

type countingSource struct {
	text       document.SuppliedText
	calls      int
	lastDigest string
}

func (s *countingSource) SuppliedText(_ context.Context, digest string) (document.SuppliedText, error) {
	s.calls++
	s.lastDigest = digest
	return s.text, nil
}

type testUpload struct {
	*bytes.Reader

	metadata document.AuthorizedUploadMetadata
	reads    int
}

func (u *testUpload) Read(p []byte) (int, error) {
	u.reads++
	n, err := u.Reader.Read(p)
	if err != nil {
		return n, fmt.Errorf("read synthetic upload: %w", err)
	}
	return n, nil
}

func newTestUpload(family, mediaType string) *testUpload {
	data := []byte("synthetic sealed source")
	digest := sha256.Sum256(data)
	return &testUpload{Reader: bytes.NewReader(data), metadata: document.AuthorizedUploadMetadata{
		Filename: "source.pdf", MediaFamily: family, MediaType: mediaType, ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64), ProviderMetadataChecksum: strings.Repeat("3", 64), InputKind: document.RenditionInputOriginalFile,
	}}
}
func (*testUpload) Close() error                                  { return nil }
func (u *testUpload) Metadata() document.AuthorizedUploadMetadata { return u.metadata }
func testAuthorization(d document.RenditionDescriptor, m document.AuthorizedUploadMetadata) document.RenditionAuthorization {
	started := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: d.ID, DescriptorFingerprint: d.Fingerprint, PolicyFingerprint: d.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64), SourceSHA256: m.SHA256, SourceBytes: m.ByteLength,
		CapabilityRecordChecksum: m.CapabilityRecordChecksum, ProviderMetadataChecksum: m.ProviderMetadataChecksum,
		MediaFamily: m.MediaFamily, MediaType: m.MediaType, InputKind: m.InputKind,
		AllowedArtifactRoles: []document.EvidenceArtifactRole{document.EvidenceArtifactStructured},
		MaxArtifactBytes:     1 << 20, MaxArtifacts: 1, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"), ExpiresAt: started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
}
