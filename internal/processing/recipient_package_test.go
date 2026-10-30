package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
)

func recipientDocuments() []RecipientPackageDocument {
	return []RecipientPackageDocument{{Ordinal: 1, DocumentID: "CASE000101",
		AllocationID: "11111111-1111-4111-8111-111111111111", NamespaceID: "22222222-2222-4222-8222-222222222222",
		ArtifactManifestSHA256: strings.Repeat("a", 64), RecipeSHA256: strings.Repeat("b", 64), ReviewSHA256: strings.Repeat("c", 64),
		PageCount: 2, Labels: []string{"CASE000101", "CASE000102"},
		Text: RecipientPackageFile{Path: "VOL001/TEXT/000001.txt", Size: 12, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("SAFE\fSECOND\f")))},
		PDF:  RecipientPackageFile{Path: "VOL001/PDF/000001.pdf", Size: 3, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("PDF")))}}}
}

func TestRecipientPackageRejectsUnreviewedText(t *testing.T) {
	t.Parallel()
	source := &observedRecipientSource{Reader: strings.NewReader("PDF")}
	var out bytes.Buffer
	_, err := WriteRecipientPackage(t.Context(), &out, recipientDocuments(), func(context.Context, int) (io.ReadCloser, []byte, error) {
		return source, []byte("LEAK\fSECOND\f"), nil
	})
	require.ErrorIs(t, err, ErrRecipientPackageIntegrity)
	require.True(t, source.closed)
}

func TestRecipientPackage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	docs := recipientDocuments()
	receipt, err := WriteRecipientPackage(t.Context(), &out, docs, func(context.Context, int) (io.ReadCloser, []byte, error) {
		return io.NopCloser(strings.NewReader("PDF")), []byte("SAFE\fSECOND\f"), nil
	})
	require.NoError(t, err)
	require.NoError(t, VerifyRecipientPackage(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), receipt))
	z, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	require.NoError(t, err)
	names := []string{"VOL001/PDF/000001.pdf", "VOL001/TEXT/000001.txt", "VOL001/DATA.DAT", "VOL001/DATA.OPT", "mapping.json", "receipt.json"}
	require.Len(t, z.File, len(names))
	files := map[string][]byte{}
	for i, f := range z.File {
		require.Equal(t, names[i], f.Name)
		require.Empty(t, f.Extra)
		r, err := f.Open()
		require.NoError(t, err)
		files[f.Name], err = io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
	}
	require.Equal(t, "SAFE\fSECOND\f", string(files[names[1]]))
	require.Equal(t, "CASE000101,VOL001,PDF\\000001.pdf,Y,,,2\r\nCASE000102,VOL001,PDF\\000001.pdf,,,,\r\n", string(files["VOL001/DATA.OPT"]))
	profile, err := loadfile.ReadProfile("dat-concordance-v1")
	require.NoError(t, err)
	records, diagnostics, err := loadfile.ParseDAT(bytes.NewReader(files["VOL001/DATA.DAT"]), profile)
	require.NoError(t, err)
	require.Empty(t, diagnostics)
	require.Len(t, records, 1)
	var values []string
	for _, f := range records[0].Fields {
		values = append(values, f.Raw)
	}
	require.Equal(t, []string{"CASE000101", docs[0].NamespaceID, "CASE000101", "CASE000102", "VOL001/PDF/000001.pdf", "VOL001/TEXT/000001.txt", "2"}, values)
	require.Equal(t, 2, receipt.PageCount)
	require.Equal(t, "native-pdf-recipient-package/v1", receipt.Contract)
	require.Equal(t, "VOL001/TEXT/000001.txt", receipt.Documents[0].Text.Path)
}

func TestRecipientPackageAdmission(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"documents", "pages", "bytes", "duplicate", "OPT"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			docs := recipientDocuments()
			switch kind {
			case "documents":
				docs = recipientTestDocuments(17, 1)
			case "pages":
				docs = recipientTestDocuments(5, 13)
			case "bytes":
				docs[0].PDF.Size = MaxRecipientPackageBytes
			case "duplicate":
				docs = recipientTestDocuments(2, 1)
				docs[1].AllocationID = docs[0].AllocationID
			case "OPT":
				docs[0].DocumentID = "CASE,000101"
				docs[0].Labels[0] = docs[0].DocumentID
			}
			var out bytes.Buffer
			called := false
			_, err := WriteRecipientPackage(t.Context(), &out, docs, func(context.Context, int) (io.ReadCloser, []byte, error) {
				called = true
				return nil, nil, errors.New("unexpected PDF request")
			})
			require.Error(t, err)
			require.False(t, called)
			require.Zero(t, out.Len())
		})
	}
}

func TestRecipientPackageRejectsChangedMetadata(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"DAT", "OPT", "mapping", "text", "PDF"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var original bytes.Buffer
			receipt, err := WriteRecipientPackage(t.Context(), &original, recipientDocuments(), func(context.Context, int) (io.ReadCloser, []byte, error) {
				return io.NopCloser(strings.NewReader("PDF")), []byte("SAFE\fSECOND\f"), nil
			})
			require.NoError(t, err)
			pinned := slices.Clone(receipt.Documents)
			z, err := zip.NewReader(bytes.NewReader(original.Bytes()), int64(original.Len()))
			require.NoError(t, err)
			var rewritten bytes.Buffer
			writer := zip.NewWriter(&rewritten)
			for _, f := range z.File {
				r, err := f.Open()
				require.NoError(t, err)
				data, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				switch {
				case name == "DAT" && f.Name == "VOL001/DATA.DAT":
					data = bytes.Replace(data, []byte("CASE000102"), []byte("CASE000999"), 1)
					receipt.LoadFile = recipientFile(f.Name, data)
				case name == "OPT" && f.Name == "VOL001/DATA.OPT":
					data = bytes.Replace(data, []byte("PDF\\000001.pdf"), []byte("PDF\\000002.pdf"), 1)
					receipt.PageMap = recipientFile(f.Name, data)
				case name == "mapping" && f.Name == "mapping.json":
					data = bytes.Replace(data, []byte("loadfile.file.produced_pdf"), []byte("loadfile.file.native"), 1)
					receipt.Mapping = recipientFile(f.Name, data)
				case name == "text" && f.Name == "VOL001/TEXT/000001.txt":
					data = []byte("LEAK\fSECOND\f")
					receipt.Documents[0].Text = recipientFile(f.Name, data)
				case name == "PDF" && f.Name == "VOL001/PDF/000001.pdf":
					data = []byte("BAD")
					receipt.Documents[0].PDF = recipientFile(f.Name, data)
				}
				if f.Name == "receipt.json" {
					data, err = canonical.Marshal(receipt)
					require.NoError(t, err)
				}
				header := &zip.FileHeader{Name: f.Name, Method: zip.Store}
				header.SetMode(0600)
				w, err := writer.CreateHeader(header)
				require.NoError(t, err)
				_, err = w.Write(data)
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			expected := receipt
			expected.Documents = pinned
			require.ErrorIs(t, VerifyRecipientPackage(t.Context(), bytes.NewReader(rewritten.Bytes()), int64(rewritten.Len()), expected), ErrRecipientPackageIntegrity)
		})
	}
}

type unreadRecipientArchive struct{ t *testing.T }

func (r unreadRecipientArchive) ReadAt([]byte, int64) (int, error) {
	r.t.Fatal("oversized archive must not be read")
	return 0, io.EOF
}

func TestRecipientPackageReadBound(t *testing.T) {
	t.Parallel()
	receipt := RecipientPackageReceipt{Documents: recipientDocuments()}
	require.ErrorIs(t, VerifyRecipientPackage(t.Context(), unreadRecipientArchive{t}, MaxRecipientPackageBytes+1, receipt), ErrRecipientPackageLimit)
}

type observedRecipientSource struct {
	io.Reader

	closed bool
}

func (r *observedRecipientSource) Close() error { r.closed = true; return nil }

func TestRecipientPackageClosesFailedSource(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &observedRecipientSource{Reader: strings.NewReader("PDF")}
	var out bytes.Buffer
	_, err := WriteRecipientPackage(ctx, &out, recipientDocuments(), func(context.Context, int) (io.ReadCloser, []byte, error) {
		cancel()
		return source, []byte("SAFE\f"), nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, source.closed)
}

func recipientTestDocuments(count, pages int) []RecipientPackageDocument {
	docs := make([]RecipientPackageDocument, 0, count)
	for i := range count {
		d := recipientDocuments()[0]
		d.Ordinal = i + 1
		d.AllocationID = fmt.Sprintf("%08d-1111-4111-8111-111111111111", i+1)
		d.PageCount = pages
		d.Labels = nil
		for j := range pages {
			d.Labels = append(d.Labels, fmt.Sprintf("CASE%06d", i*pages+j))
		}
		d.DocumentID = d.Labels[0]
		d.PDF.Path = fmt.Sprintf("VOL001/PDF/%06d.pdf", i+1)
		d.Text.Path = fmt.Sprintf("VOL001/TEXT/%06d.txt", i+1)
		docs = append(docs, d)
	}
	return docs
}
