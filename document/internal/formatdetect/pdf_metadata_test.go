package formatdetect_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestPDFMetadataEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		keyBits        int
		flags          model.PermissionFlags
		permissions    []string
	}{
		{name: "plain"},
		{name: "owner encrypted", keyBits: 128, flags: model.PermissionsPrint, permissions: []string{"Print", "High-quality print flag"}},
		{name: "bit 12 only", keyBits: 128, flags: model.PermissionsNone | model.PermissionPrintRev3, permissions: []string{"High-quality print flag"}},
		{name: "locked", password: "open-secret", keyBits: 128, flags: model.PermissionsPrint},
		{name: "revision 2", keyBits: 40, flags: model.PermissionFlags(0xFFC0), permissions: []string{"None"}},
		{name: "annotation and forms", keyBits: 128, flags: model.PermissionsNone | model.PermissionModAnnFillForm, permissions: []string{"Annotate and fill forms"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := mediatest.PDF()
			if tc.keyBits != 0 {
				var err error
				data, err = mediatest.EncryptPDF(data, tc.password, "owner-secret", tc.keyBits, tc.flags)
				require.NoError(t, err)
			}
			metadata, err := formatdetect.ReadPDFMetadata(data)
			require.NoError(t, err)
			assert.Equal(t, new(tc.keyBits != 0), metadata.Facts.Encrypted)
			assert.Equal(t, tc.permissions, metadata.Facts.Permissions)
			if tc.password != "" {
				assert.Nil(t, metadata.Facts.SignatureFieldCount)
				assert.Contains(t, metadata.Issues, formatdetect.PDFMetadataIssue{SourceField: "Password"})
			}
		})
	}
	for _, tc := range []struct {
		name, extra, fields string
		count               int64
		coverage            *bool
		issue, rewrite      bool
	}{
		{name: "unnamed signature widget", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Parent) /Kids [6 0 R 7 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 5 0 R /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 5 0 R >>", count: 1, coverage: new(true)},
		{name: "rewritten smaller", fields: "[5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>\n%" + strings.Repeat("x", 2048), count: 1, coverage: new(false), rewrite: true},
		{name: "dangling field reference", fields: "[5 0 R 9 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>", count: 1, coverage: new(true)},
		{name: "blank", fields: "[5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Blank) >>", count: 1},
		{name: "malformed", fields: "[5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [1 10 20 20] >> >>", count: 1, issue: true},
		{name: "latest first", fields: "[5 0 R 6 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Latest) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>|<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Older) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> >>", count: 2, coverage: new(true)},
		{name: "latest last", fields: "[6 0 R 5 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Latest) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>|<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Older) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> >>", count: 2, coverage: new(true)},
		{name: "malformed after valid", fields: "[5 0 R 6 0 R]", extra: "<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Latest) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>|<< /Subtype /Widget /Rect [0 0 1 1] /FT /Sig /T (Broken) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 -10 20 20] >> >>", count: 2, coverage: new(true), issue: true},
		{name: "inherited value", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Parent) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> /Kids [6 0 R] >>|<< /Rect [0 0 1 1] /T (Child) /Parent 5 0 R /Kids [7 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>", count: 1, coverage: new(false)},
		{name: "child value overrides parent", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Parent) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 -1 20 20] >> /Kids [6 0 R] >>|<< /Rect [0 0 1 1] /T (Child) /Parent 5 0 R /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> /Kids [7 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>", count: 1, coverage: new(true)},
		{name: "inherited widgets", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Group) /Kids [6 0 R] >>|<< /Rect [0 0 1 1] /T (Signature) /Kids [7 0 R 8 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>", count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := mediatest.PDFSignatureFields(tc.fields, strings.Split(tc.extra, "|")...)
			if tc.rewrite {
				var rewritten bytes.Buffer
				require.NoError(t, api.AddProperties(bytes.NewReader(data), &rewritten, map[string]string{"Amendment": "Edited"}, model.NewDefaultConfiguration()))
				require.Less(t, rewritten.Len(), len(data))
				data = rewritten.Bytes()
			}
			metadata, err := formatdetect.ReadPDFMetadata(data)
			require.NoError(t, err)
			require.Equal(t, new(tc.count), metadata.Facts.SignatureFieldCount)
			require.Equal(t, tc.coverage, metadata.Facts.LastSignatureCoversFile)
			if tc.issue {
				require.Contains(t, metadata.Issues, formatdetect.PDFMetadataIssue{SourceField: "ByteRange"})
			} else {
				require.NotContains(t, metadata.Issues, formatdetect.PDFMetadataIssue{SourceField: "ByteRange"})
			}
		})
	}
}

func TestPDFMetadataReachableActionsAndAttachments(t *testing.T) {
	data := mediatest.PDFWithActionsAndAttachment()
	metadata, err := formatdetect.ReadPDFMetadata(data)
	require.NoError(t, err)
	require.Equal(t, new(int64(1)), metadata.Facts.EmbeddedFileCount)
	require.Equal(t, new(true), metadata.Facts.HasJavaScript)
	require.Equal(t, new(true), metadata.Facts.HasLaunchActions)
	for _, tc := range []struct {
		name, catalog, page string
		javascript, launch  bool
		data                []byte
	}{
		{name: "rendition script", catalog: "/OpenAction << /S /Rendition /JS (void 0) >>", javascript: true},
		{name: "catalog additional action", catalog: "/AA << /WC << /Type /Action /S /JavaScript /JS (void 0) >> >>", javascript: true},
		{name: "annotation chain", page: "/Annots [<< /Type /Annot /Subtype /Link /Rect [0 0 1 1] /A << /S /JavaScript /JS (void 0) /Next [<< /Type /Action /S /Launch /F (example.txt) >>] >> >>]", javascript: true, launch: true},
		{name: "tagged structure role", data: mediatest.PDFObjects("synthetic tagged PDF", []string{
			"<< /Type /Catalog /Pages 2 0 R /MarkInfo << /Marked true >> /StructTreeRoot 4 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] >>", "<< /Type /StructTreeRoot /K [5 0 R] /RoleMap << /Launch /P >> >>", "<< /Type /StructElem /S /Launch /P 4 0 R /Pg 3 0 R /K 0 >>"})},
		{name: "unreachable names"},
		{name: "empty JavaScript tree", catalog: "/Names << /JavaScript << /Names [] >> >>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			if data == nil {
				data = mediatest.PDFObjects("/JavaScript /Launch", []string{
					"<< /Type /Catalog /Pages 2 0 R " + tc.catalog + " >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
					"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " + tc.page + " >>", "<< /S /JavaScript /Next << /S /Launch >> >>",
				})
			}
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
	for _, tc := range []struct {
		name  string
		data  []byte
		count int64
	}{
		{"distinct streams", distinct, 2},
		{"inline aliases", mediatest.PDFObjects("synthetic inline attachments", []string{
			"<< /Type /Catalog /Pages 2 0 R /Names << /EmbeddedFiles << /Names [(note.txt) << /Type /Filespec /F (note.txt) /EF << /F 4 0 R >> >>] >> >> /AF [<< /Type /Filespec /F (note.txt) /EF << /F 4 0 R >> >>] >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] >>", "<< /Type /EmbeddedFile /Length 4 >>\nstream\nnote\nendstream"}), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata, err := formatdetect.ReadPDFMetadata(tc.data)
			require.NoError(t, err)
			require.Equal(t, new(tc.count), metadata.Facts.EmbeddedFileCount)
		})
	}
}

func TestPDFMetadataUnsupportedEncryption(t *testing.T) {
	data := mediatest.PDFObjects("unsupported encryption", []string{
		"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] >>", "<< /Filter /Adobe.PubSec >>",
	})
	data = bytes.Replace(data, []byte("/Root 1 0 R"), []byte("/Root 1 0 R /Encrypt 4 0 R /ID [<11111111111111111111111111111111> <11111111111111111111111111111111>]"), 1)
	metadata, err := formatdetect.ReadPDFMetadata(data)
	require.NoError(t, err)
	require.Equal(t, new(true), metadata.Facts.Encrypted)
	require.Nil(t, metadata.Facts.SignatureFieldCount)
	require.Contains(t, metadata.Issues, formatdetect.PDFMetadataIssue{SourceField: "Encryption"})
}
