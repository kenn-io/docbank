package epubutil

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type cancelAfterParseContext struct {
	calls    int
	cancelAt int
}

func (ctx *cancelAfterParseContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelAfterParseContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelAfterParseContext) Value(any) any               { return nil }

func (ctx *cancelAfterParseContext) Err() error {
	ctx.calls++
	if ctx.calls >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestArchivePathAndBases(t *testing.T) {
	for _, test := range []struct{ ref, base, want string }{
		{" /Text/a.xhtml ", "OPS", "Text/a.xhtml"},
		{"Text\\a.xhtml", "OPS", "OPS/Text/a.xhtml"},
		{"Te\txt/a%20b.xhtml", "OPS", "OPS/Text/a b.xhtml"},
		{"../a.xhtml", "OPS", "a.xhtml"},
	} {
		got, err := ArchivePath(test.ref, test.base)
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}
	for _, ref := range []string{"../../escape", "https://example.com/x", "//example.com/x", "a?query", "a#fragment"} {
		_, err := ArchivePath(ref, "OPS")
		require.Error(t, err)
	}
	require.Equal(t, []string{"OPS", "OPS/a", "OPS/a/b", "OPS/a/b/c"}, ManifestBases("OPS", "a/", "b/", "c/"))
	require.Equal(t, "OPS/a", ResolveArchiveDir("OPS", "a/book.opf"))
	require.Equal(t, "Text", ResolveArchiveDir("OPS", "/Text/"))
	for _, test := range []struct {
		base, want string
	}{
		{"/", "."}, {"../", "."}, {"Text/", "OPS/Text"},
	} {
		got, err := ResolveArchiveBase("OPS", test.base)
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}
	for _, base := range []string{"../../", "https://example.com/", "a?query", "a#fragment"} {
		_, err := ResolveArchiveBase("OPS", base)
		require.Error(t, err)
	}
}

func TestReadPackagesPreservesDeclarationsAndOccurrences(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range []struct{ name, body string }{
		{"META-INF/container.xml", `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="a.opf"/><rootfile full-path="b.opf"/></rootfiles></container>`},
		{"a.opf", `<package xml:base="A/"><manifest xml:base="B/"><item id="a" href="chapter" media-type="application/xhtml+xml"/><item id="a" href="chapter" media-type="image/svg+xml"/></manifest><spine><itemref idref="a"/><itemref idref="a" linear="no"/></spine></package>`},
		{"b.opf", `<package><manifest><item id="b" href="chapter" media-type="application/xml"/></manifest></package>`},
	} {
		file, err := writer.Create(entry.name)
		require.NoError(t, err)
		_, err = file.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	archive, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)
	records, err := ReadPackages(archive.File, 4096)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Len(t, records[0].Manifest.Items, 2)
	require.Equal(t, "image/svg+xml", records[0].Manifest.Items[1].MediaType)
	require.Len(t, records[0].Spine.Items, 2)
	require.Equal(t, "no", records[0].Spine.Items[1].Linear)
	require.Equal(t, "A/", records[0].Base)
	require.Equal(t, "B/", records[0].Manifest.Base)
	_, err = ReadPackages(archive.File, 1)
	require.Error(t, err)
	_, err = ReadPackages(archive.File[:1], 4096)
	require.ErrorContains(t, err, "package document is missing")
	_, err = ReadPackages(archive.File[1:], 4096)
	require.ErrorContains(t, err, "container document is missing")
	file := archive.File[0]
	_, err = ReadZIPEntry(file, int64(file.UncompressedSize64))
	require.NoError(t, err)
	_, err = ReadZIPEntry(file, int64(file.UncompressedSize64)-1)
	require.Error(t, err)
}

func TestDecodeXMLContextCancelsDuringPackageParse(t *testing.T) {
	body := []byte(`<package>` + strings.Repeat(`<meta property="x" content="value"/>`, 1000) + `</package>`)
	ctx := &cancelAfterParseContext{cancelAt: 3}
	var record Package
	err := decodeXMLContext(ctx, body, &record)
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.calls, ctx.cancelAt)
}

func TestContextReaderStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader := contextReader{ctx: ctx, reader: strings.NewReader("payload")}
	buffer := make([]byte, 3)
	read, err := reader.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, 3, read)
	cancel()
	_, err = reader.Read(buffer)
	require.ErrorIs(t, err, context.Canceled)
}

func TestReadZIPEntryContextCancelsDuringDecompression(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("payload")
	require.NoError(t, err)
	payload := make([]byte, 128<<10)
	for index := range payload {
		payload[index] = byte((index*31 + index/251) % 251)
	}
	_, err = entry.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	archive, err := zip.NewReader(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	require.NoError(t, err)

	ctx := &cancelAfterParseContext{cancelAt: 6}
	_, err = ReadZIPEntryContext(ctx, archive.File[0], int64(len(payload)))
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.calls, ctx.cancelAt)
}
