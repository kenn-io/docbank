package formatdetect_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestPDFMetadataEvidence(t *testing.T) {
	plain := mediatest.PDF()
	for _, tc := range []struct{ name, password string }{{"plain", ""}, {"owner encrypted", ""}, {"locked", "open-secret"}} {
		t.Run(tc.name, func(t *testing.T) {
			data := plain
			if tc.name != "plain" {
				var err error
				data, err = mediatest.EncryptPDF(plain, tc.password, "owner-secret", 128, model.PermissionsPrint)
				require.NoError(t, err)
			}

			metadata, err := formatdetect.ReadPDFMetadata(data)
			require.NoError(t, err)
			require.Equal(t, tc.name != "plain", *metadata.Facts.Encrypted)
			if tc.password != "" {
				require.Nil(t, metadata.Facts.SignatureFieldCount)
				require.NotEmpty(t, metadata.Issues)
				return
			}
			require.Equal(t, int64(0), *metadata.Facts.SignatureFieldCount)
			require.Equal(t, int64(0), *metadata.Facts.EmbeddedFileCount)
			require.False(t, *metadata.Facts.HasJavaScript)
			require.False(t, *metadata.Facts.HasLaunchActions)
			require.Nil(t, metadata.Facts.LastSignatureCoversFile)
			if tc.name == "owner encrypted" {
				require.Equal(t, []string{"Print", "Print in high quality"}, metadata.Facts.Permissions)
			} else {
				require.Nil(t, metadata.Facts.Permissions)
			}
		})
	}
	for _, tc := range []struct {
		name, extra, fields string
		count               int64
		coverage            *bool
		issue               bool
	}{
		{name: "blank", fields: "[5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Blank) >>", count: 1},
		{name: "signed", fields: "[5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>", count: 1, coverage: new(true)},
		{name: "appended", fields: "[5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> >>", count: 1, coverage: new(false)},
		{name: "malformed", fields: "[5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [1 10 20 20] >> >>", count: 1, issue: true},
		{name: "latest first", fields: "[5 0 R 6 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Latest) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>|<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Older) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> >>", count: 2, coverage: new(true)},
		{name: "latest last", fields: "[6 0 R 5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Latest) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>|<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Older) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> >>", count: 2, coverage: new(true)},
		{name: "malformed after valid", fields: "[5 0 R 6 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Latest) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>|<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Broken) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 -10 20 20] >> >>", count: 2, issue: true},
		{name: "inherited value", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Parent) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> /Kids [6 0 R] >>|<< /Rect [0 0 1 1] /T (Child) /Parent 5 0 R /Kids [7 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>", count: 1, coverage: new(false)},
		{name: "child value overrides parent", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Parent) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 -1 20 20] >> /Kids [6 0 R] >>|<< /Rect [0 0 1 1] /T (Child) /Parent 5 0 R /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> /Kids [7 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>", count: 1, coverage: new(true)},
		{name: "inherited widgets", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Group) /Kids [6 0 R] >>|<< /Rect [0 0 1 1] /T (Signature) /Kids [7 0 R 8 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>", count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := mediatest.PDFSignatureFields(tc.fields, strings.Split(tc.extra, "|")...)
			metadata, err := formatdetect.ReadPDFMetadata(data)
			require.NoError(t, err)
			require.Equal(t, new(tc.count), metadata.Facts.SignatureFieldCount)
			require.Equal(t, tc.coverage, metadata.Facts.LastSignatureCoversFile)
			if tc.issue {
				require.Contains(t, metadata.Issues, formatdetect.PDFMetadataIssue{SourceField: "ByteRange"})
			}
		})
	}
	_, err := formatdetect.ReadPDFMetadata(mediatest.PDFSignatureFields("[5 0 R]", "<< /FT /Sig /T (Invalid) >>"))
	require.ErrorContains(t, err, "required entry=Rect missing")
}

func TestPDFMetadataReachableActionsAndAttachments(t *testing.T) {
	data := mediatest.PDFWithActionsAndAttachment()
	metadata, err := formatdetect.ReadPDFMetadata(data)
	require.NoError(t, err)
	require.Equal(t, new(int64(1)), metadata.Facts.EmbeddedFileCount)
	require.Equal(t, new(true), metadata.Facts.HasJavaScript)
	require.Equal(t, new(true), metadata.Facts.HasLaunchActions)
	_, err = formatdetect.CountPDFPages(data)
	require.ErrorIs(t, err, formatdetect.ErrPDFExternalReference)
	for _, tc := range []struct {
		name, catalog, page string
		javascript, launch  bool
	}{
		{name: "catalog additional action", catalog: "/AA << /WC << /Type /Action /S /JavaScript /JS (void 0) >> >>", javascript: true},
		{name: "annotation chain", page: "/Annots [<< /Type /Annot /Subtype /Link /Rect [0 0 1 1] /A << /S /JavaScript /JS (void 0) /Next [<< /Type /Action /S /Launch /F (example.txt) >>] >> >>]", javascript: true, launch: true},
		{name: "unreachable names"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := mediatest.PDFObjects("/JavaScript /Launch", []string{
				"<< /Type /Catalog /Pages 2 0 R " + tc.catalog + " >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
				"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " + tc.page + " >>", "<< /S /JavaScript /Next << /S /Launch >> >>",
			})
			metadata, err := formatdetect.ReadPDFMetadata(data)
			require.NoError(t, err)
			require.Equal(t, new(tc.javascript), metadata.Facts.HasJavaScript)
			require.Equal(t, new(tc.launch), metadata.Facts.HasLaunchActions)
		})
	}
	// Rebuild the object graph so equal payload bytes belong to distinct stream objects.
	distinct := mediatest.PDFObjects("distinct streams", []string{
		"<< /Type /Catalog /Pages 2 0 R /AF [<< /F (a) /EF << /F 4 0 R >> >> << /F (b) /EF << /F 5 0 R >> >>] >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] >>", "<< /Length 4 >>\nstream\nnote\nendstream", "<< /Length 4 >>\nstream\nnote\nendstream",
	})
	metadata, err = formatdetect.ReadPDFMetadata(distinct)
	require.NoError(t, err)
	require.Equal(t, new(int64(2)), metadata.Facts.EmbeddedFileCount)
}

func TestPDFMetadataLongGraphsAndEmptyJavaScriptTree(t *testing.T) {
	for _, kind := range []string{"outline", "page links", "empty JavaScript tree"} {
		t.Run(kind, func(t *testing.T) {
			objects := []string{"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /JavaScript /JS (void 0) >> /Outlines 4 0 R >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] >>", "<< /Type /Outlines /First 5 0 R /Last 304 0 R /Count 300 >>"}
			for i := 5; i <= 304; i++ {
				links := ""
				if i < 304 {
					links += fmt.Sprintf(" /Next %d 0 R", i+1)
				}
				if i > 5 {
					links += fmt.Sprintf(" /Prev %d 0 R", i-1)
				}
				objects = append(objects, fmt.Sprintf("<< /Title (Bookmark) /Parent 4 0 R /Dest [3 0 R /Fit]%s >>", links))
			}
			if kind == "page links" {
				objects = []string{"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /JavaScript /JS (void 0) >> >>", ""}
				var kids strings.Builder
				for i := 3; i < 83; i++ {
					fmt.Fprintf(&kids, "%d 0 R ", i)
					next := i + 1
					if next == 83 {
						next = 3
					}
					objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Annots [<< /Type /Annot /Subtype /Link /Rect [0 0 10 10] /Dest [%d 0 R /Fit] >>] >>", next))
				}
				objects[1] = "<< /Type /Pages /Count 80 /Kids [" + kids.String() + "] >>"
			}
			if kind == "empty JavaScript tree" {
				objects = objects[:3]
				objects[0] = "<< /Type /Catalog /Pages 2 0 R /Names << /JavaScript << /Names [] >> >> >>"
			}
			data := mediatest.PDFObjects("synthetic graph", objects)
			_, err := formatdetect.CountPDFPages(data)
			require.NoError(t, err)
			metadata, err := formatdetect.ReadPDFMetadata(data)
			require.NoError(t, err)
			require.Empty(t, metadata.Issues)
			require.Equal(t, new(kind != "empty JavaScript tree"), metadata.Facts.HasJavaScript)
			require.Equal(t, new(false), metadata.Facts.HasLaunchActions)
			require.Equal(t, new(int64(0)), metadata.Facts.EmbeddedFileCount)
		})
	}
}

func TestPDFMetadataDeclaredEvidenceRegressions(t *testing.T) {
	restricted, err := mediatest.EncryptPDF(mediatest.PDF(), "", "owner-secret", 40, model.PermissionFlags(0xFFC0))
	require.NoError(t, err)
	forms, err := mediatest.EncryptPDF(mediatest.PDF(), "", "owner-secret", 128, model.PermissionsNone|model.PermissionModAnnFillForm)
	require.NoError(t, err)
	for _, tc := range []struct {
		name        string
		data        []byte
		permissions []string
		embedded    int64
		signatures  int64
		coverage    *bool
	}{
		{name: "revision 2", data: restricted, permissions: []string{"None"}},
		{name: "annotation and forms", data: forms, permissions: []string{"Annotate and fill forms"}},
		{name: "inline attachment aliases", data: mediatest.PDFInlineAttachments(), embedded: 1},
		{name: "tagged structure role", data: mediatest.PDFStructureRole()},
		{name: "unnamed signature widget", data: mediatest.PDFUnnamedSignatureWidget(), signatures: 1, coverage: new(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := formatdetect.CountPDFPagesWithEmptyPassword(tc.data)
			if tc.name == "tagged structure role" {
				require.ErrorIs(t, err, formatdetect.ErrPDFExternalReference)
			} else {
				require.NoError(t, err)
			}
			metadata, err := formatdetect.ReadPDFMetadata(tc.data)
			require.NoError(t, err)
			assert.Equal(t, tc.permissions, metadata.Facts.Permissions)
			assert.Equal(t, new(tc.embedded), metadata.Facts.EmbeddedFileCount)
			assert.Equal(t, new(tc.signatures), metadata.Facts.SignatureFieldCount)
			assert.Equal(t, tc.coverage, metadata.Facts.LastSignatureCoversFile)
			assert.Equal(t, new(false), metadata.Facts.HasJavaScript)
			assert.Equal(t, new(false), metadata.Facts.HasLaunchActions)
		})
	}
}
