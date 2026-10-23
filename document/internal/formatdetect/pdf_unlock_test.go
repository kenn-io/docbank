package formatdetect_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/internal/formatdetect"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestUnlockPDF(t *testing.T) {
	for _, tc := range []struct {
		name, user string
		passwords  []string
		keyBits    int
	}{
		{"user password", "open", []string{"wrong", "open"}, 256},
		{"empty user password", "", nil, 256},
		{"owner password", "open", []string{"owner"}, 128},
		// Legacy PDF handlers compare PDFDocEncoding A3 for the UTF-8 password £.
		{"PDFDocEncoding", "\xa3", []string{"£"}, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := mediatest.EncryptedPDF(tc.user, "owner", tc.keyBits)
			require.NoError(t, err)
			plain, _, err := formatdetect.UnlockPDF(source, tc.passwords, 1<<20)
			require.NoError(t, err)
			pages, err := formatdetect.CountPDFPages(plain)
			require.NoError(t, err)
			require.Equal(t, int64(1), pages)
		})
	}
	source, err := mediatest.EncryptedPDF("open", "owner", 128)
	require.NoError(t, err)
	_, _, err = formatdetect.UnlockPDF(source, []string{"wrong"}, 1<<20)
	require.ErrorIs(t, err, formatdetect.ErrPDFEncrypted)
	_, _, err = formatdetect.UnlockPDF([]byte("broken PDF"), []string{"wrong", "open"}, 1<<20)
	require.Error(t, err)
	require.NotErrorIs(t, err, formatdetect.ErrPDFEncrypted)
}

func TestUnlockPDFBoundsExpandedObjectOutput(t *testing.T) {
	// Encrypted object streams compress these repetitive annotations.
	// Decryption writes ordinary objects, expanding them beyond the limit.
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"",
	}
	var annotations strings.Builder
	for i := range 1500 {
		fmt.Fprintf(&annotations, "%d 0 R ", i+4)
		objects = append(objects, "<< /Type /Annot /Subtype /Text /Rect [0 0 1 1] /Contents ("+strings.Repeat("synthetic ", 800)+") >>")
	}
	objects[2] = "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Annots [" + annotations.String() + "] >>"
	plain := mediatest.PDFObjects("compressed objects", objects)
	var packed, encrypted bytes.Buffer
	require.NoError(t, api.Optimize(bytes.NewReader(plain), &packed, model.NewDefaultConfiguration()))
	require.NoError(t, api.Encrypt(bytes.NewReader(packed.Bytes()), &encrypted, model.NewAESConfiguration("open", "owner", 128)))
	require.Less(t, encrypted.Len(), 1<<20)
	unlocked, _, err := formatdetect.UnlockPDF(encrypted.Bytes(), []string{"open"}, 1<<20)
	require.ErrorIs(t, err, formatdetect.ErrPDFExpandedBytes)
	require.Nil(t, unlocked)
}
