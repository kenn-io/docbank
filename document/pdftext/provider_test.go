package pdftext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/internal/scanfixture"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/upload"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestPDFTextCorpus(t *testing.T) {
	p, err := New(Profile{MaxDocumentBytes: 500 << 20, MaxPages: 5_000})
	require.NoError(t, err)
	for _, fixture := range scanfixture.Corpus() {
		expected := map[string][2]int{"born-digital": {3, 0}, "image-only": {0, 3}, "mixed-above-ratio": {8, 2}, "blank": {0, 1}, "undecodable-font": {0, 1}}
		want, accepted := expected[fixture.Name]
		if !accepted && fixture.Name != "encrypted" && fixture.Name != "malformed" {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			upload := newTestUpload(fixture.Bytes)
			result, err := document.RenderRendition(t.Context(), p, upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
			if !accepted {
				var providerError *document.RenditionProviderError
				require.ErrorAs(t, err, &providerError)
				require.Equal(t, document.RenditionErrorUnsupportedInput, providerError.Code())
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Evidence.Units, want[0])
			require.Len(t, result.Evidence.Omissions, want[1])
			for i, unit := range result.Evidence.Units {
				require.Equal(t, i, unit.Order)
				require.Equal(t, int64(i+1), unit.Locator.Start)
				require.Contains(t, unit.Text, "synthetic")
			}
			for i, omission := range result.Evidence.Omissions {
				reason := "no_text_layer"
				if fixture.Name == "undecodable-font" {
					reason = "no_readable_text"
				}
				require.Equal(t, reason, omission.Reason)
				require.Equal(t, int64(want[0]+i+1), omission.Locator.Start)
			}
		})
	}
}

func TestPDFTextWhitespaceOmissions(t *testing.T) {
	p, err := New(Profile{MaxDocumentBytes: 1 << 20, MaxPages: 3})
	require.NoError(t, err)
	upload := newTestUpload(mediatest.PDFTextPages([]string{"   ", "", "synthetic text"}, false))
	result, err := document.RenderRendition(t.Context(), p, upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 1)
	require.Equal(t, int64(3), result.Evidence.Units[0].Locator.Start)
	require.Len(t, result.Evidence.Omissions, 2)
	for i, reason := range []string{"no_readable_text", "no_text_layer"} {
		require.Equal(t, reason, result.Evidence.Omissions[i].Reason)
		require.Equal(t, int64(i+1), result.Evidence.Omissions[i].Locator.Start)
	}
}

func TestPDFTextPreservesLiteralMarkup(t *testing.T) {
	p, err := New(Profile{MaxDocumentBytes: 1 << 20, MaxPages: 1})
	require.NoError(t, err)
	const literal = "Order <invoice-1729> \\ *item* [ref](https://example.org) # Title"
	data := redactiontest.PDF(t, []string{"part-time", literal}, "literal markup")
	upload := newTestUpload(data)
	result, err := p.Render(t.Context(), upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.Contains(t, result.Evidence.Units[0].Text, `part\-time`)
	require.Contains(t, result.Evidence.Units[0].Text, `Order \<invoice\-1729\> \\ \*item\* \[ref\]\(https\:\/\/example\.org\) \# Title`)
}

func TestPDFTextLimitsAndCancellation(t *testing.T) {
	var data []byte
	for _, fixture := range scanfixture.Corpus() {
		if fixture.Name == "born-digital" {
			data = fixture.Bytes
		}
	}
	for _, test := range []struct {
		name         string
		bytes, pages int64
		output       int
		canceled     bool
		cause        string
	}{
		{"bytes", 1, 10, 1 << 20, false, "PDF exceeds byte limit"}, {"pages", 1 << 20, 2, 1 << 20, false, "PDF exceeds page limit"}, {"output", 1 << 20, 10, 300, false, "PDF evidence exceeds output limit"}, {"canceled", 1 << 20, 10, 1 << 20, true, "context canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := New(Profile{MaxDocumentBytes: test.bytes, MaxPages: test.pages})
			require.NoError(t, err)
			upload := newTestUpload(data)
			authorization := mediatest.Authorization(p.Descriptor(), upload.Metadata())
			authorization.MaxTotalResultBytes = test.output
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			_, err = p.Render(ctx, upload, authorization)
			require.Error(t, err)
			require.ErrorContains(t, errors.Unwrap(err), test.cause)
			if test.canceled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				var providerError *document.RenditionProviderError
				require.ErrorAs(t, err, &providerError)
				require.Equal(t, document.RenditionErrorPolicyRejected, providerError.Code())
			}
		})
	}
}

func newTestUpload(data []byte) *mediatest.Upload {
	return mediatest.NewUpload(data, document.AuthorizedUploadMetadata{Filename: "source.pdf", MediaFamily: "pdf", MediaType: "application/pdf"})
}

func TestPDFTextSealedAdmissionRetainsProviderBounds(t *testing.T) {
	data := redactiontest.PDF(t, []string{strings.Repeat(" ", 16384)}, "sealed PDF")
	p, err := New(Profile{MaxDocumentBytes: int64(len(data)), MaxPages: 1})
	require.NoError(t, err)
	metadata := newTestUpload(data).Metadata()
	authorization := mediatest.Authorization(p.Descriptor(), metadata)
	record, err := media.InspectCapability(t.Context(), bytes.NewReader(data), media.InspectionPolicy{
		Filename: metadata.Filename, DeclaredMediaType: metadata.MediaType, ExpectedBytes: metadata.ByteLength, ExpectedSHA256: metadata.SHA256,
		DescriptorFingerprint: p.Descriptor().Fingerprint, ProfileFingerprint: strings.Repeat("b", 64), DisclosureFingerprint: strings.Repeat("c", 64), InputKind: document.RenditionInputOriginalFile, PDFTextOnly: true,
		MaxSourceBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 1000, MaxPages: 100,
		MaxNestingDepth: 1, MaxTextLines: 1000, MaxCharacters: 1 << 20, MaxRecords: 1000, MaxSlides: 100, MaxSheets: 100, MaxCells: 1000, MaxSpineItems: 100, MaxResources: 100,
	})
	require.NoError(t, err)
	require.True(t, record.Eligible)
	sealed, err := upload.Authorize(t.Context(), upload.Source{Reader: io.NopCloser(bytes.NewReader(data)), Directory: t.TempDir()}, record, upload.UploadMetadata{Filename: metadata.Filename})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sealed.Close()) })
	authorization.CapabilityRecordChecksum = sealed.Metadata().CapabilityRecordChecksum
	authorization.ProviderMetadataChecksum = sealed.Metadata().ProviderMetadataChecksum
	_, err = document.RenderRendition(t.Context(), p, sealed, authorization)
	var providerError *document.RenditionProviderError
	require.ErrorAs(t, err, &providerError)
	require.Equal(t, document.RenditionErrorUnsupportedInput, providerError.Code())
	require.ErrorIs(t, errors.Unwrap(err), formatdetect.ErrPDFEntryBytes)
}

func TestPDFTextPageLimitBoundaries(t *testing.T) {
	for _, pages := range []int64{0, 100_000, 100_001} {
		_, err := New(Profile{MaxDocumentBytes: 1 << 20, MaxPages: pages})
		if pages == 100_000 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
}

func TestPDFTextPageReadFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code document.RenditionErrorCode
	}{
		{"memory", fmt.Errorf("read: %w", &pdfproduction.RSSLimitError{}), document.RenditionErrorCapacity},
		{"deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), document.RenditionErrorCapacity},
		{"invalid", errors.New("invalid page"), document.RenditionErrorPolicyRejected},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := New(Profile{MaxDocumentBytes: 1 << 20, MaxPages: 2})
			require.NoError(t, err)
			p.readPage = func(context.Context, []byte, int, int64, int) (pdfproduction.PDFTextPage, error) {
				return pdfproduction.PDFTextPage{}, test.err
			}
			upload := newTestUpload(mediatest.PDFTextPages([]string{"sample"}, false))
			_, err = p.Render(t.Context(), upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
			var providerError *document.RenditionProviderError
			require.ErrorAs(t, err, &providerError)
			require.Equal(t, test.code, providerError.Code())
			require.ErrorIs(t, err, test.err)
		})
	}
}
