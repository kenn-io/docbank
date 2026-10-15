package pdfproduction

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/internal/redactiontest"
)

func TestNativeFontDomainsBindStagedSource(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	data := redactiontest.FontPDF("BT /F1 12 Tf 72 720 Td (L R) Tj ET",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", "")
	identity := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256: hashBytes(data), Size: int64(len(data))}
	inspection, err := pagerender.InspectSource(data, identity, "application/pdf")
	require.NoError(t, err)
	source := Source{Reader: bytes.NewReader(data), Size: identity.Size, SHA256: identity.SHA256}
	domains, err := nativeFontDomains(t.Context(), runtime, bytes.NewReader(data), source,
		inspection.Frames)
	require.NoError(t, err)
	require.Equal(t, []bool{true}, domains)
	// A configured child that cannot start remains an optional proof failure.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	_, err = runtime.Inspect(t.Context(), data, identity, "application/pdf")
	require.ErrorIs(t, err, pagerender.ErrUnavailable)
	domains, err = nativeFontDomains(t.Context(), runtime, bytes.NewReader(data),
		source, inspection.Frames)
	require.NoError(t, err)
	require.Empty(t, domains)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = nativeFontDomains(canceled, runtime, bytes.NewReader(data), source, inspection.Frames)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNativeFontDomainsRejectCIDWithQualifiedProgram(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	data := redactiontest.FontPDF("BT /F1 24 Tf 72 720 Td <004C00200052> Tj ET",
		"<< /Type /Font /Subtype /Type0 /BaseFont /Helvetica /Encoding /Identity-H "+
			"/DescendantFonts [7 0 R] >>", "",
		"<< /Type /Font /Subtype /CIDFontType0 /BaseFont /Helvetica /CIDSystemInfo "+
			"<< /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> >>")
	identity := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256: hashBytes(data), Size: int64(len(data))}
	inspection, err := pagerender.InspectSource(data, identity, "application/pdf")
	require.NoError(t, err)
	source := Source{Reader: bytes.NewReader(data), Size: identity.Size, SHA256: identity.SHA256}
	domains, err := nativeFontDomains(t.Context(), runtime, bytes.NewReader(data), source,
		inspection.Frames)
	require.NoError(t, err)
	require.Equal(t, []bool{false}, domains)
	withNativeFont(t, data, func(ctx context.Context, proof *nativeFontProof,
		page requests.Page, object references.FPDF_PAGEOBJECT) {
		font, err := proof.instance.FPDFTextObj_GetFont(&requests.FPDFTextObj_GetFont{PageObject: object})
		require.NoError(t, err)
		matched, err := proof.qualifiedProgram(font.Font)
		require.NoError(t, err)
		require.True(t, matched, "the classifier must reject even when program bytes qualify")
	})
}
