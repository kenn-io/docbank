package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestExtractSourceMetadataFromSyntheticFormats(t *testing.T) {
	ooxml := syntheticOOXML(t)
	for _, testCase := range []struct {
		name      string
		payload   []byte
		keys      []string
		sensitive string
	}{
		{name: "PDF info and XMP", payload: syntheticMetadataPDF(9), keys: []string{"created", "creators", "keywords", "page_count", "subject", "title"}},
		{name: "OOXML core and custom", payload: ooxml, keys: []string{"created", "office.custom.matter_number", "title"}},
		{name: "RFC 5322 email", payload: []byte("From: Ada <ada@example.test>\r\nTo: Grace <grace@example.test>\r\nBcc: Private <private@example.test>\r\nSubject: Synthetic mail\r\nDate: Tue, 2 Jan 2024 03:04:05 -0700\r\n\r\nbody"), keys: []string{"email.bcc", "email.from", "email.sent", "email.subject", "email.to"}, sensitive: "email.bcc"},
		{name: "iCalendar", payload: []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic meeting\r\nDTSTART:20240102T030405\r\nDTEND:20240102T040405\r\nEND:VEVENT\r\nEND:VCALENDAR"), keys: []string{"calendar.end", "calendar.start", "title"}},
		{name: "JPEG EXIF and GPS", payload: syntheticEXIFJPEG(), keys: []string{"description", "image.exif.gps_latitude", "image.exif.gps_longitude"}, sensitive: "image.exif.gps_latitude"},
		{name: "ID3 media tags", payload: syntheticID3(), keys: []string{"creators", "media.id3.album", "title"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := ExtractSourceMetadata(testCase.payload)
			keys := make([]string, 0, len(metadata.Fields))
			sensitive := map[string]bool{}
			for _, field := range metadata.Fields {
				keys = append(keys, field.Key)
				sensitive[field.Key] = field.Sensitive
			}
			assert.ElementsMatch(t, testCase.keys, keys)
			if testCase.sensitive != "" {
				assert.True(t, sensitive[testCase.sensitive])
			}
			canonical, _, err := document.MarshalSourceMetadataV1(metadata)
			require.NoError(t, err)
			assert.NotContains(t, string(canonical), "source_path")
		})
	}
}

func TestExtractSourceMetadataDoesNotScanCompressedPayloadsForTagLikeText(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte("ID3xxxx-random-audio-TIT2\x00invented title\x00"),
		append([]byte("\xff\xd8\xff\xda\x00\x02"), []byte("compressed Artist=invented creator\x00\xff\xd9")...),
	} {
		metadata := ExtractSourceMetadata(payload)
		assert.Empty(t, metadata.Fields)
	}
}

func TestExtractSourceMetadataUsesVerifiedPDFPageTree(t *testing.T) {
	metadata := ExtractSourceMetadata(syntheticMetadataPDF(2))
	pageCount, found := sourceMetadataInteger(metadata, "page_count")
	require.True(t, found)
	assert.Equal(t, int64(2), pageCount,
		"an unrelated earlier /Count must not override the catalog page tree")

	malformed := ExtractSourceMetadata([]byte("%PDF-1.7 /Count 99"))
	_, found = sourceMetadataInteger(malformed, "page_count")
	assert.False(t, found)
	assert.Contains(t, sourceMetadataWarningCodes(malformed), "unparseable_pdf_pages")
}

func TestExtractSourceMetadataUsesOnlyTrailerInfoDictionary(t *testing.T) {
	payload := bytes.Replace(syntheticMetadataPDF(1), []byte("/Label (page-1)"),
		[]byte("/Author (Decoy)"), 1)
	metadata := ExtractSourceMetadata(payload)

	var creators []string
	for _, field := range metadata.Fields {
		if field.Key == "creators" {
			creators = field.Value.Strings
		}
	}
	assert.Equal(t, []string{"Ada", "Grace"}, creators)
}

func TestExtractSourceMetadataParsesMultipartAttachmentHeaders(t *testing.T) {
	payload := strings.Join([]string{
		"From: Ada <ada@example.test>",
		"Subject: Attachments",
		`Content-Type: multipart/mixed; boundary="synthetic-boundary"`,
		"",
		"--synthetic-boundary",
		"Content-Type: text/plain",
		"",
		"literal Content-Disposition: attachment is not a MIME part header",
		"--synthetic-boundary",
		"content-disposition: ATTACHMENT; filename=one.txt",
		"",
		"one",
		"--synthetic-boundary",
		"Content-Disposition:",
		" attachment; filename=two.txt",
		"",
		"two",
		"--synthetic-boundary--",
		"",
	}, "\r\n")
	metadata := ExtractSourceMetadata([]byte(payload))
	count, found := sourceMetadataInteger(metadata, "attachment_count")
	require.True(t, found)
	assert.Equal(t, int64(2), count)
}

func sourceMetadataInteger(metadata document.SourceMetadataV1, key string) (int64, bool) {
	for _, field := range metadata.Fields {
		if field.Key == key && field.Value.Integer != nil {
			return *field.Value.Integer, true
		}
	}
	return 0, false
}

func sourceMetadataWarningCodes(metadata document.SourceMetadataV1) []string {
	codes := make([]string, 0, len(metadata.Warnings))
	for _, warning := range metadata.Warnings {
		codes = append(codes, warning.Code)
	}
	return codes
}

func syntheticMetadataPDF(pageCount int) []byte {
	kids := make([]string, 0, pageCount)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R /Count 99 >>",
		"",
	}
	for index := range pageCount {
		kids = append(kids, fmt.Sprintf("%d 0 R", index+3))
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Label (page-%d) >>", index+1))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount)
	objects = append(objects,
		"<< /Title (Quarterly report) /Author (Ada; Grace) /Subject (Synthetic) /Keywords (one,two) /CreationDate (D:20240102030405) >>")
	var output bytes.Buffer
	_, _ = output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output,
		"trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, len(objects), xref)
	return output.Bytes()
}

func syntheticOOXML(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	core, err := writer.Create("docProps/core.xml")
	require.NoError(t, err)
	_, err = io.WriteString(core, `<cp:coreProperties xmlns:cp="urn:cp" xmlns:dc="urn:dc" xmlns:dcterms="urn:dcterms"><dc:title>Synthetic office</dc:title><dcterms:created>2024-01-02T03:04:05Z</dcterms:created></cp:coreProperties>`)
	require.NoError(t, err)
	custom, err := writer.Create("docProps/custom.xml")
	require.NoError(t, err)
	_, err = io.WriteString(custom, `<Properties><property name="Matter Number"><lpwstr>MAT-001</lpwstr></property></Properties>`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return output.Bytes()
}

func syntheticID3() []byte {
	var frames bytes.Buffer
	for _, frame := range []struct{ id, value string }{
		{id: "TIT2", value: "Synthetic song"},
		{id: "TPE1", value: "Ada"},
		{id: "TALB", value: "Synthetic album"},
	} {
		payload := append([]byte{3}, []byte(frame.value)...)
		_, _ = frames.WriteString(frame.id)
		_ = binary.Write(&frames, binary.BigEndian, uint32(len(payload)))
		_, _ = frames.Write([]byte{0, 0})
		_, _ = frames.Write(payload)
	}
	size := frames.Len()
	header := []byte{'I', 'D', '3', 3, 0, 0,
		byte(size >> 21), byte(size >> 14), byte(size >> 7), byte(size)}
	return append(header, frames.Bytes()...)
}

func syntheticEXIFJPEG() []byte {
	const (
		rootOffset        = 8
		descriptionOffset = 38
		gpsOffset         = 54
		latitudeOffset    = 108
		longitudeOffset   = 132
	)
	tiff := make([]byte, 156)
	copy(tiff, "II")
	binary.LittleEndian.PutUint16(tiff[2:], 42)
	binary.LittleEndian.PutUint32(tiff[4:], rootOffset)
	binary.LittleEndian.PutUint16(tiff[rootOffset:], 2)
	putEXIFEntry(tiff[rootOffset+2:], 0x010e, 2, 16, descriptionOffset)
	putEXIFEntry(tiff[rootOffset+14:], 0x8825, 4, 1, gpsOffset)
	copy(tiff[descriptionOffset:], "Synthetic image\x00")
	binary.LittleEndian.PutUint16(tiff[gpsOffset:], 4)
	putEXIFInlineASCII(tiff[gpsOffset+2:], 1, "N")
	putEXIFEntry(tiff[gpsOffset+14:], 2, 5, 3, latitudeOffset)
	putEXIFInlineASCII(tiff[gpsOffset+26:], 3, "W")
	putEXIFEntry(tiff[gpsOffset+38:], 4, 5, 3, longitudeOffset)
	putEXIFRationals(tiff[latitudeOffset:], [3]uint32{51, 30, 0})
	putEXIFRationals(tiff[longitudeOffset:], [3]uint32{0, 6, 0})
	segment := append([]byte("Exif\x00\x00"), tiff...)
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(jpeg[4:], uint16(len(segment)+2))
	jpeg = append(jpeg, segment...)
	return append(jpeg, 0xff, 0xd9)
}

func putEXIFEntry(target []byte, tag, kind uint16, count, value uint32) {
	binary.LittleEndian.PutUint16(target, tag)
	binary.LittleEndian.PutUint16(target[2:], kind)
	binary.LittleEndian.PutUint32(target[4:], count)
	binary.LittleEndian.PutUint32(target[8:], value)
}

func putEXIFInlineASCII(target []byte, tag uint16, value string) {
	putEXIFEntry(target, tag, 2, 2, 0)
	copy(target[8:12], value+"\x00")
}

func putEXIFRationals(target []byte, values [3]uint32) {
	for index, value := range values {
		binary.LittleEndian.PutUint32(target[index*8:], value)
		binary.LittleEndian.PutUint32(target[index*8+4:], 1)
	}
}

func TestBackfillSourceMetadataPublishesOnlyAfterVerifiedEOF(t *testing.T) {
	payload := []byte("%PDF-1.7 /Title (Verified report)")
	target := store.SourceMetadataTarget{SourceSHA256: processingHash("a1"), Size: int64(len(payload))}
	catalog := &sourceMetadataCatalogStub{targets: []store.SourceMetadataTarget{target}}
	reader := &sourceMetadataReaderStub{payload: payload, closeErr: errors.New("verification failed")}
	completed, err := BackfillSourceMetadata(t.Context(), catalog, reader, 10)
	require.ErrorContains(t, err, "verification failed")
	assert.Zero(t, completed)
	assert.Zero(t, catalog.published)
	reader.closeErr = nil
	completed, err = BackfillSourceMetadata(t.Context(), catalog, reader, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, completed)
	assert.Equal(t, 1, catalog.published)
}

func TestBackfillSourceMetadataContinuesPastUnreadableTarget(t *testing.T) {
	payload := []byte("%PDF-1.7 /Title (Verified report)")
	targets := []store.SourceMetadataTarget{{SourceSHA256: processingHash("a1"), Size: int64(len(payload))}, {SourceSHA256: processingHash("b2"), Size: int64(len(payload))}}
	catalog := &sourceMetadataCatalogStub{targets: targets}
	reader := &sourceMetadataReaderStub{payload: payload, closeErrors: []error{errors.New("first corrupt"), nil}}
	completed, err := BackfillSourceMetadataTargets(t.Context(), catalog, reader, targets)
	require.ErrorContains(t, err, "first corrupt")
	assert.Equal(t, 1, completed)
	assert.Equal(t, 1, catalog.published)
}

type sourceMetadataCatalogStub struct {
	targets   []store.SourceMetadataTarget
	published int
}

func (s *sourceMetadataCatalogStub) MissingSourceMetadataTargets(context.Context, string, int) ([]store.SourceMetadataTarget, error) {
	return s.targets, nil
}
func (s *sourceMetadataCatalogStub) PublishSourceMetadata(context.Context, string, string, []byte) (store.SourceMetadataGeneration, error) {
	s.published++
	return store.SourceMetadataGeneration{}, nil
}

type sourceMetadataReaderStub struct {
	payload     []byte
	closeErr    error
	closeErrors []error
	calls       int
}

func (s *sourceMetadataReaderStub) OpenStreamContext(context.Context, string) (packstore.VerifiedReadCloser, int64, error) {
	closeErr := s.closeErr
	if s.calls < len(s.closeErrors) {
		closeErr = s.closeErrors[s.calls]
	}
	s.calls++
	return &verifiedSourceMetadataReader{Reader: bytes.NewReader(s.payload), closeErr: closeErr}, int64(len(s.payload)), nil
}

type verifiedSourceMetadataReader struct {
	*bytes.Reader

	closeErr error
}

func (r *verifiedSourceMetadataReader) Close() error   { return r.closeErr }
func (r *verifiedSourceMetadataReader) Verified() bool { return r.closeErr == nil }
func (r *verifiedSourceMetadataReader) Verify() error  { return r.closeErr }

var _ packstore.VerifiedReadCloser = (*verifiedSourceMetadataReader)(nil)
