package formatdetect_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

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
				data, err = mediatest.EncryptPDF(plain, tc.password, "owner-secret", 128)
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
				require.NotNil(t, metadata.Facts.PermissionBits)
				require.Contains(t, metadata.Facts.Permissions, "Bit  3: false (print(rev2), print quality(rev>=3))")
			} else {
				require.Equal(t, []string{"Full access"}, metadata.Facts.Permissions)
			}
		})
	}
	for _, tc := range []struct {
		name, extra, fields string
		count               int64
		coverage            *bool
		issue               bool
	}{
		{name: "blank", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Blank) >>", count: 1},
		{name: "signed", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 LENGTH] >> >>", count: 1, coverage: new(true)},
		{name: "appended", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [0 10 20 20] >> >>", count: 1, coverage: new(false)},
		{name: "malformed", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Signed) /V << /Type /Sig /Filter /Adobe.PPKLite /SubFilter /adbe.pkcs7.detached /Contents <00> /ByteRange [1 10 20 20] >> >>", count: 1, issue: true},
		{name: "latest first", fields: "[5 0 R 6 0 R]", extra: "<< /FT /Sig /T (Latest) /V << /ByteRange [0 10 20 LENGTH] >> >>|<< /FT /Sig /T (Older) /V << /ByteRange [0 10 20 20] >> >>", count: 2, coverage: new(true)},
		{name: "latest last", fields: "[6 0 R 5 0 R]", extra: "<< /FT /Sig /T (Latest) /V << /ByteRange [0 10 20 LENGTH] >> >>|<< /FT /Sig /T (Older) /V << /ByteRange [0 10 20 20] >> >>", count: 2, coverage: new(true)},
		{name: "malformed after valid", fields: "[5 0 R 6 0 R]", extra: "<< /FT /Sig /T (Latest) /V << /ByteRange [0 10 20 LENGTH] >> >>|<< /FT /Sig /T (Broken) /V << /ByteRange [0 -10 20 20] >> >>", count: 2, issue: true},
		{name: "inherited widgets", fields: "[5 0 R]", extra: "<< /FT /Sig /T (Group) /Kids [6 0 R] >>|<< /T (Signature) /Kids [7 0 R 8 0 R] >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>|<< /Subtype /Widget /Rect [0 0 1 1] /Parent 6 0 R >>", count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []string{"<< /Type /Catalog /Pages 2 0 R /AcroForm 4 0 R >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>", "<< /Fields " + tc.fields + " >>", tc.extra}
			// Keep the declared range width fixed when inserting the final file length.
			objects[4] = string(bytes.ReplaceAll([]byte(objects[4]), []byte("LENGTH"), []byte("000000")))
			objects = append(objects[:4], strings.Split(objects[4], "|")...)
			data := mediatest.PDFObjects("synthetic evidence", objects)
			data = bytes.ReplaceAll(data, []byte("000000]"), fmt.Appendf(nil, "%06d]", len(data)-20))
			metadata, err := formatdetect.ReadPDFMetadata(data)
			require.NoError(t, err)
			require.Equal(t, new(tc.count), metadata.Facts.SignatureFieldCount)
			require.Equal(t, tc.coverage, metadata.Facts.LastSignatureCoversFile)
			if tc.issue {
				require.Contains(t, metadata.Issues, formatdetect.PDFMetadataIssue{SourceField: "ByteRange"})
			}
		})
	}
}

func TestPDFMetadataReachableActionsAndAttachments(t *testing.T) {
	data := mediatest.PDFObjects("synthetic evidence", []string{
		"<< /Type /Catalog /Pages 2 0 R /Names << /EmbeddedFiles << /Names [(note.txt) 4 0 R] >> /JavaScript << /Names [(script) 6 0 R] >> >> /AF [4 0 R] /OpenAction 6 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Annots [8 0 R] >>",
		"<< /Type /Filespec /F (note.txt) /EF << /F 5 0 R >> >>",
		"<< /Type /EmbeddedFile /Length 4 >>\nstream\nnote\nendstream",
		"<< /S /JavaScript /JS (void 0) /Next 7 0 R >>",
		"<< /S /Launch /F (example.txt) >>",
		"<< /Type /Annot /Subtype /FileAttachment /Rect [0 0 10 10] /FS 4 0 R >>",
	})
	metadata, err := formatdetect.ReadPDFMetadata(data)
	require.NoError(t, err)
	require.Equal(t, new(int64(1)), metadata.Facts.EmbeddedFileCount)
	require.Equal(t, new(true), metadata.Facts.HasJavaScript)
	require.Equal(t, new(true), metadata.Facts.HasLaunchActions)
	_, err = formatdetect.CountPDFPages(data)
	require.ErrorIs(t, err, formatdetect.ErrPDFExternalReference)
	decoy := mediatest.PDFObjects("/JavaScript /Launch", []string{
		"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>", "<< /S /JavaScript /Next << /S /Launch >> >>",
	})
	metadata, err = formatdetect.ReadPDFMetadata(decoy)
	require.NoError(t, err)
	require.Equal(t, new(false), metadata.Facts.HasJavaScript)
	require.Equal(t, new(false), metadata.Facts.HasLaunchActions)
}
