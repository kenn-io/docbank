package processing

import (
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
)

// FuzzExtractSourceMetadata checks that the local extractor never panics and
// always returns a record the source-metadata contract accepts and can read
// back, whatever the input bytes look like.
func FuzzExtractSourceMetadata(f *testing.F) {
	for _, seed := range [][]byte{
		syntheticMetadataPDF(9),
		[]byte("From: Ada <ada@example.test>\r\nTo: Grace <grace@example.test>\r\n" +
			"Subject: Synthetic mail\r\nDate: Tue, 2 Jan 2024 03:04:05 -0700\r\n\r\nbody"),
		[]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic meeting\r\n" +
			"DTSTART:20240102T030405\r\nDTEND:20240102T040405\r\nEND:VEVENT\r\nEND:VCALENDAR"),
		syntheticExifJPEG(),
		syntheticRichExifTIFF(),
		syntheticRAF(),
		syntheticCR3(),
		syntheticID3Tag(4),
		mediatest.PNG(12, 8, color.Black),
		mediatest.GIF(16, 10, 2),
		mediatest.WebP(20, 14),
		mediatest.MP4(64, 48, 1000),
		[]byte("PK\x03\x04"),
		[]byte("%PDF-"),
		{},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		record := ExtractSourceMetadata(data)
		encoded, checksum, err := document.MarshalSourceMetadataV1(record)
		require.NoError(t, err, "extracted metadata must satisfy the contract")
		decoded, decodedChecksum, err := document.DecodeSourceMetadataV1(encoded)
		require.NoError(t, err)
		assert.Equal(t, checksum, decodedChecksum)
		assert.Len(t, decoded.Fields, len(record.Fields))
	})
}
