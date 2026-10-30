package docx

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testPart struct{ name, body string }

func testParts() []testPart {
	return []testPart{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`},
		{"word/document.xml", `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Synthetic sample</w:t></w:r></w:p></w:body></w:document>`},
		{"docProps/core.xml", `<properties><modified>2026-01-01</modified></properties>`},
		{"word/styles.xml", `<styles/>`},
		{"word/comments.xml", `<comments/>`},
		{"word/media/image.png", "synthetic image bytes"},
		{"word/embeddings/sample.bin", "synthetic embedded file"},
		{"_xmlsignatures/sig1.xml", `<Signature/>`},
	}
}

func testArchive(t *testing.T, parts []testPart, method uint16, year int) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, part := range parts {
		f, err := w.CreateHeader(&zip.FileHeader{Name: part.name, Method: method,
			Modified: time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)})
		require.NoError(t, err)
		_, err = f.Write([]byte(part.body))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return b.Bytes()
}

func TestFingerprintIgnoresPackagingOnly(t *testing.T) {
	t.Parallel()
	parts := testParts()
	first := testArchive(t, parts, zip.Store, 2025)
	want, err := Fingerprint(t.Context(), first)
	require.NoError(t, err)
	require.Len(t, want, 64)
	for _, variant := range []struct {
		reverse bool
		method  uint16
		year    int
	}{
		{false, zip.Store, 2026}, {false, zip.Deflate, 2025}, {true, zip.Store, 2025},
	} {
		entries := slices.Clone(parts)
		if variant.reverse {
			slices.Reverse(entries)
		}
		other := testArchive(t, entries, variant.method, variant.year)
		require.NotEqual(t, sha256.Sum256(first), sha256.Sum256(other))
		got, err := Fingerprint(t.Context(), other)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for i := range parts {
		changed := slices.Clone(parts)
		changed[i].body += " "
		got, err := Fingerprint(t.Context(), testArchive(t, changed, zip.Store, 2025))
		require.NoError(t, err)
		require.NotEqual(t, want, got, parts[i].name)
	}
	renamed := slices.Clone(parts)
	renamed[len(renamed)-1].name = "_xmlsignatures/sig2.xml"
	got, err := Fingerprint(t.Context(), testArchive(t, renamed, zip.Store, 2025))
	require.NoError(t, err)
	require.NotEqual(t, want, got)
	got, err = Fingerprint(t.Context(), testArchive(t, parts[:len(parts)-1], zip.Store, 2025))
	require.NoError(t, err)
	require.NotEqual(t, want, got)
	unchanged := testArchive(t, parts, zip.Store, 2025)
	require.Equal(t, unchanged, first, "fingerprinting must not rewrite originals")
}

func TestFingerprintRejectsIncompleteEvidence(t *testing.T) {
	t.Parallel()
	parts := testParts()
	valid := testArchive(t, parts, zip.Store, 2025)
	corrupt := bytes.Replace(slices.Clone(valid), []byte("synthetic image bytes"), []byte("different image bytes"), 1)
	for name, data := range map[string][]byte{
		"not zip":        []byte("not a document"),
		"truncated":      valid[:len(valid)-8],
		"crc":            corrupt,
		"duplicate name": testArchive(t, append(slices.Clone(parts), parts[1]), zip.Store, 2025),
		"not docx":       testArchive(t, parts[2:], zip.Store, 2025),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Fingerprint(t.Context(), data)
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := Fingerprint(ctx, valid)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, got)
}

func TestFingerprintEnforcesInspectionLimits(t *testing.T) {
	t.Parallel()
	got, err := Fingerprint(t.Context(), make([]byte, (64<<20)+1))
	require.ErrorContains(t, err, "64 MiB")
	require.Empty(t, got)
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	_, err = w.CreateRaw(&zip.FileHeader{Name: "word/document.xml", Method: zip.Store,
		UncompressedSize64: (100 << 20) + 1})
	require.NoError(t, err)
	require.NoError(t, w.Close())
	got, err = Fingerprint(t.Context(), b.Bytes())
	require.Error(t, err)
	require.Empty(t, got)
}
