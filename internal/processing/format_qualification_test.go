package processing

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/formatqualification"
)

func TestSourceMetadataQualificationsExecuteRegisteredFixtures(t *testing.T) {
	t.Parallel()
	type fixture struct {
		catalogID string
		evidence  string
		payload   func(*testing.T) []byte
		assert    func(*testing.T, document.SourceMetadataV1)
	}
	fixtures := []fixture{
		{catalogID: "calendar", evidence: "TestExtractSourceMetadataFromSyntheticFormats",
			payload: func(*testing.T) []byte {
				return []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic meeting\r\nDTSTART:20240102T030405\r\nDTEND:20240102T040405\r\nEND:VEVENT\r\nEND:VCALENDAR")
			}, assert: func(t *testing.T, metadata document.SourceMetadataV1) {
				t.Helper()
				_, found := sourceMetadataTimestamp(metadata, "calendar.start")
				assert.True(t, found)
			}},
		{catalogID: "docx", evidence: "TestExtractSourceMetadataFromSyntheticDOCX",
			payload: func(t *testing.T) []byte {
				t.Helper()
				return syntheticOOXMLFamily(t, "word/document.xml")
			},
			assert: assertQualifiedMetadataTitle("Synthetic office")},
		{catalogID: "eml", evidence: "TestExtractSourceMetadataFromSyntheticFormats",
			payload: func(*testing.T) []byte {
				return []byte("From: Ada <ada@example.test>\r\nTo: Grace <grace@example.test>\r\nSubject: Synthetic mail\r\nDate: Tue, 2 Jan 2024 03:04:05 -0700\r\n\r\nbody")
			}, assert: func(t *testing.T, metadata document.SourceMetadataV1) {
				t.Helper()
				subject, found := sourceMetadataString(metadata, "email.subject")
				require.True(t, found)
				assert.Equal(t, "Synthetic mail", subject)
			}},
		{catalogID: "gif", evidence: "TestExtractSourceMetadataReadsVisualContainerFacts",
			payload: func(*testing.T) []byte { return mediatest.GIF(16, 10, 2) },
			assert:  assertQualifiedContainerFormat("gif")},
		{catalogID: "jpeg", evidence: "TestExtractSourceMetadataFromSyntheticFormats",
			payload: func(*testing.T) []byte { return syntheticExifJPEG() },
			assert:  assertQualifiedContainerFormat("jpeg")},
		{catalogID: "mp3", evidence: "TestExtractID3TextEncodingsAndFrameBoundary",
			payload: func(*testing.T) []byte {
				return syntheticID3Tag(4, syntheticID3Frame{id: "TIT2", encoding: 3, text: []byte("Synthetic song")})
			}, assert: func(t *testing.T, metadata document.SourceMetadataV1) {
				t.Helper()
				title, found := sourceMetadataString(metadata, "title")
				require.True(t, found)
				assert.Equal(t, "Synthetic song", title)
			}},
		{catalogID: "mp4", evidence: "TestExtractSourceMetadataReadsMP4CreationTime",
			payload: qualifiedMP4MetadataFixture,
			assert: func(t *testing.T, metadata document.SourceMetadataV1) {
				t.Helper()
				created, found := sourceMetadataTimestamp(metadata, "created")
				require.True(t, found)
				assert.Equal(t, "2024-06-15T14:30:22Z", created.Normalized)
			}},
		{catalogID: "pdf", evidence: "TestExtractSourceMetadataUsesAuthoritativePDFInfo",
			payload: func(*testing.T) []byte { return syntheticMetadataPDF(1) },
			assert: func(t *testing.T, metadata document.SourceMetadataV1) {
				t.Helper()
				title, found := sourceMetadataString(metadata, "title")
				require.True(t, found)
				assert.Equal(t, "Quarterly report", title)
			}},
		{catalogID: "png", evidence: "TestExtractSourceMetadataReadsVisualContainerFacts",
			payload: func(*testing.T) []byte { return mediatest.PNG(12, 8, color.Black) },
			assert:  assertQualifiedContainerFormat("png")},
		{catalogID: "pptx", evidence: "TestExtractSourceMetadataFromSyntheticPPTX",
			payload: func(t *testing.T) []byte {
				t.Helper()
				return syntheticOOXMLFamily(t, "ppt/presentation.xml")
			},
			assert: assertQualifiedMetadataTitle("Synthetic office")},
		{catalogID: "tiff", evidence: "TestExtractSourceMetadataReadsTIFFPhotoFacts",
			payload: func(*testing.T) []byte { return syntheticRichExifTIFF() },
			assert:  assertQualifiedContainerFormat("tiff")},
		{catalogID: "webp", evidence: "TestExtractSourceMetadataReadsVisualContainerFacts",
			payload: func(*testing.T) []byte { return mediatest.WebP(20, 14) },
			assert:  assertQualifiedContainerFormat("webp")},
		{catalogID: "xlsx", evidence: "TestExtractSourceMetadataFromSyntheticXLSX",
			payload: func(t *testing.T) []byte {
				t.Helper()
				return syntheticOOXMLFamily(t, "xl/workbook.xml")
			},
			assert: assertQualifiedMetadataTitle("Synthetic office")},
	}

	exercised := make([]formatqualification.Qualification, 0, len(fixtures))
	for _, fixture := range fixtures {
		t.Run(fixture.catalogID, func(t *testing.T) {
			metadata, err := ExtractSourceMetadata(t.Context(), sourceMetadataTestSpool(t), fixture.payload(t))
			require.NoError(t, err)
			fixture.assert(t, metadata)
			query := formatqualification.Query{
				CatalogID: fixture.catalogID, Capability: formatqualification.CapabilityMetadata,
				Evidence: fixture.evidence, ImplementationID: SourceMetadataExtractorFingerprint,
				InputKind: formatqualification.InputOriginalFile,
			}
			qualification, found := formatqualification.Lookup(query)
			require.True(t, found, "live extractor fixture has no exact qualification: %+v", query)
			exercised = append(exercised, qualification)
		})
	}

	registered := slices.DeleteFunc(formatqualification.All(), func(qualification formatqualification.Qualification) bool {
		return qualification.Capability != formatqualification.CapabilityMetadata
	})
	assert.Equal(t, registered, exercised, "every registered metadata tuple must execute at the processing owner boundary")
}

func assertQualifiedContainerFormat(want string) func(*testing.T, document.SourceMetadataV1) {
	return func(t *testing.T, metadata document.SourceMetadataV1) {
		t.Helper()
		format, found := sourceMetadataString(metadata, "media.container.format")
		require.True(t, found)
		assert.Equal(t, want, format)
	}
}

func assertQualifiedMetadataTitle(want string) func(*testing.T, document.SourceMetadataV1) {
	return func(t *testing.T, metadata document.SourceMetadataV1) {
		t.Helper()
		title, found := sourceMetadataString(metadata, "title")
		require.True(t, found)
		assert.Equal(t, want, title)
	}
}

func qualifiedMP4MetadataFixture(t *testing.T) []byte {
	t.Helper()
	payload := mediatest.MP4(640, 368, 3500)
	movieHeader := bytes.Index(payload, []byte("mvhd"))
	require.GreaterOrEqual(t, movieHeader, 4)
	want := time.Date(2024, 6, 15, 14, 30, 22, 0, time.UTC)
	const mp4EpochToUnix = int64(2_082_844_800)
	binary.BigEndian.PutUint32(payload[movieHeader+8:movieHeader+12], uint32(want.Unix()+mp4EpochToUnix))
	return payload
}
