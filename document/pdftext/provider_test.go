package pdftext

import (
	"context"
	"fmt"
	"image/color"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/scanfixture"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestPDFTextCorpus(t *testing.T) {
	p, err := New(Profile{MaxDocumentBytes: 16 << 20, MaxPages: 1000})
	require.NoError(t, err)
	for _, fixture := range scanfixture.Corpus() {
		expected := map[string][2]int{"born-digital": {3, 0}, "image-only": {0, 3}, "mixed-above-ratio": {8, 2}, "mixed-below-ratio": {1, 1}, "blank": {0, 1}, "low-quality": {0, 1}, "rotated": {0, 1}, "undecodable-font": {0, 1}}
		want, accepted := expected[fixture.Name]
		if !accepted && fixture.Name != "encrypted" && fixture.Name != "malformed" {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			upload := newTestUpload(fixture.Bytes)
			result, err := document.RenderRendition(t.Context(), p, upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
			if !accepted {
				require.Error(t, err)
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
			policy, err := document.NewEvidencePolicy(1 << 20)
			require.NoError(t, err)
			normalized, err := document.NormalizeEvidenceV1(result.Evidence, policy)
			require.NoError(t, err)
			renditionPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 1 << 20, MaxUnitRunes: 10000, MaxSegmentRunes: 1000})
			require.NoError(t, err)
			rendition, err := document.BuildRenditionV1(normalized, renditionPolicy)
			require.NoError(t, err)
			if want[0] == 0 {
				require.Empty(t, rendition.Markdown)
				require.Empty(t, rendition.LexicalSegments)
			}
		})
	}
}

func TestPDFTextWhitespaceOmissions(t *testing.T) {
	p, err := New(Profile{MaxDocumentBytes: 1 << 20, MaxPages: 3})
	require.NoError(t, err)
	for _, test := range []struct {
		name  string
		pages []string
		units int
	}{
		{"whitespace", []string{"   "}, 0},
		{"whitespace-and-blank", []string{"   ", ""}, 0},
		{"mixed", []string{"   ", "", "synthetic text"}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			upload := newTestUpload(pdfTextPages(test.pages))
			result, err := document.RenderRendition(t.Context(), p, upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
			require.NoError(t, err)
			require.Len(t, result.Evidence.Units, test.units)
			require.NotEmpty(t, result.Evidence.Omissions)
			require.Equal(t, "no_readable_text", result.Evidence.Omissions[0].Reason)
			require.Equal(t, int64(1), result.Evidence.Omissions[0].Locator.Start)
			if len(test.pages) > 1 {
				require.Len(t, result.Evidence.Omissions, 2)
				require.Equal(t, "no_text_layer", result.Evidence.Omissions[1].Reason)
				require.Equal(t, int64(2), result.Evidence.Omissions[1].Locator.Start)
			}
			policy, err := document.NewEvidencePolicy(1 << 20)
			require.NoError(t, err)
			evidence, err := document.NormalizeEvidenceV1(result.Evidence, policy)
			require.NoError(t, err)
			renditionPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 1 << 20, MaxUnitRunes: 10000, MaxSegmentRunes: 1000})
			require.NoError(t, err)
			rendition, err := document.BuildRenditionV1(evidence, renditionPolicy)
			require.NoError(t, err)
			if test.units == 0 {
				require.Empty(t, rendition.Markdown)
			} else {
				require.Contains(t, string(rendition.Markdown), "synthetic text")
				require.Equal(t, int64(3), rendition.Units[0].Locator.Start)
			}
		})
	}
}

func TestPDFTextPreservesLiteralMarkup(t *testing.T) {
	p, err := New(Profile{MaxDocumentBytes: 1 << 20, MaxPages: 1})
	require.NoError(t, err)
	const literal = "Order <invoice-1729> \\ *item* [ref](https://example.org) # Title"
	upload := newTestUpload(redactiontest.PDF(t, []string{literal}, "literal markup"))
	result, err := p.Render(t.Context(), upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	policy, err := document.NewEvidencePolicy(1 << 20)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(result.Evidence, policy)
	require.NoError(t, err)
	renditionPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 1 << 20, MaxUnitRunes: 10000, MaxSegmentRunes: 1000})
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalized, renditionPolicy)
	require.NoError(t, err)
	require.Contains(t, rendition.Units[0].Text, "invoice")
	require.Contains(t, rendition.LexicalSegments[0].Text, `Order \<invoice\-1729\> \\ \*item\* \[ref\]\(https\:\/\/example\.org\) \# Title`)
}

func TestPDFTextWithPhotoAndExternalLink(t *testing.T) {
	image := mediatest.JPEG(8, 8, color.White)
	content := "q 8 0 0 8 72 650 cm /Photo Do Q BT /F1 12 Tf 72 720 Td (invoice-1729) Tj ET"
	data := mediatest.PDFObjects("text photo and link", []string{
		"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /URI /URI (https://example.org) >> >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> /XObject << /Photo 6 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", len(image), image),
	})
	p, err := New(Profile{MaxDocumentBytes: 1 << 20, MaxPages: 1})
	require.NoError(t, err)
	upload := newTestUpload(data)
	result, err := p.Render(t.Context(), upload, mediatest.Authorization(p.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 1)
	require.Equal(t, `invoice\-1729`, result.Evidence.Units[0].Text)
	require.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	require.Empty(t, result.Evidence.Omissions)
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
	}{
		{"bytes", 1, 10, 1 << 20, false}, {"pages", 1 << 20, 2, 1 << 20, false}, {"output", 1 << 20, 10, 1, false}, {"canceled", 1 << 20, 10, 1 << 20, true},
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
			if test.canceled {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func newTestUpload(data []byte) *mediatest.Upload {
	return mediatest.NewUpload(data, document.AuthorizedUploadMetadata{Filename: "source.pdf", MediaFamily: "pdf", MediaType: "application/pdf"})
}

func pdfTextPages(pages []string) []byte {
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var kids []string
	for _, text := range pages {
		pageID := len(objects) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", pageID))
		content := "BT /F1 12 Tf 72 720 Td (" + text + ") Tj ET"
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", pageID+1), fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages))
	return mediatest.PDFObjects("page text", objects)
}
