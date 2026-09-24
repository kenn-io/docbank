package production

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
)

var zipLE = binary.LittleEndian

func readRecipientZIPAt(file *os.File, size, offset int64, count int) ([]byte, error) {
	if offset < 0 || count < 0 || int64(count) > size-offset {
		return nil, ErrRecipientArchive
	}
	data := make([]byte, count)
	if _, err := file.ReadAt(data, offset); err != nil {
		return nil, ErrRecipientArchive
	}
	return data, nil
}

// archive/zip accepts self-extracting prefixes and data after the end record.
// Walk the ZIP's actual local records, central directory, and end records so
// no bytes outside the public entries can accompany a recipient package.
func verifyRecipientZIPEnvelope(file *os.File, size int64, archive *zip.Reader) error {
	if size < 22 || archive == nil || len(archive.File) == 0 {
		return ErrRecipientArchive
	}
	cursor := int64(0)
	for _, entry := range archive.File {
		local, err := readRecipientZIPAt(file, size, cursor, 30)
		if err != nil || zipLE.Uint32(local) != 0x04034b50 ||
			zipLE.Uint16(local[4:]) != 20 ||
			zipLE.Uint16(local[6:]) != entry.Flags || zipLE.Uint16(local[8:]) != zip.Store ||
			zipLE.Uint16(local[10:]) != entry.ModifiedTime || zipLE.Uint16(local[12:]) != entry.ModifiedDate ||
			zipLE.Uint32(local[14:]) != 0 || zipLE.Uint32(local[18:]) != 0 || zipLE.Uint32(local[22:]) != 0 {
			return ErrRecipientArchive
		}
		nameSize, extraSize := int(zipLE.Uint16(local[26:])), int(zipLE.Uint16(local[28:]))
		body, err := readRecipientZIPAt(file, size, cursor+30, nameSize+extraSize)
		if err != nil || nameSize != len(entry.Name) || !bytes.Equal(body[:nameSize], []byte(entry.Name)) ||
			!bytes.Equal(body[nameSize:], packageZIPTimestampExtra) {
			return ErrRecipientArchive
		}
		dataOffset, err := entry.DataOffset()
		compressedSize, bounded := packageArchiveEntrySize(entry.CompressedSize64)
		if err != nil || dataOffset != cursor+30+int64(nameSize+extraSize) ||
			!bounded || dataOffset < 0 || dataOffset > size || compressedSize > size-dataOffset {
			return ErrRecipientArchive
		}
		cursor = dataOffset + compressedSize
		descriptorSize := 16
		if entry.CompressedSize64 > uint64(^uint32(0)) || entry.UncompressedSize64 > uint64(^uint32(0)) {
			descriptorSize = 24
		}
		descriptor, err := readRecipientZIPAt(file, size, cursor, descriptorSize)
		if err != nil || zipLE.Uint32(descriptor) != 0x08074b50 || zipLE.Uint32(descriptor[4:]) != entry.CRC32 {
			return ErrRecipientArchive
		}
		if descriptorSize == 16 {
			if uint64(zipLE.Uint32(descriptor[8:])) != entry.CompressedSize64 ||
				uint64(zipLE.Uint32(descriptor[12:])) != entry.UncompressedSize64 {
				return ErrRecipientArchive
			}
		} else if zipLE.Uint64(descriptor[8:]) != entry.CompressedSize64 ||
			zipLE.Uint64(descriptor[16:]) != entry.UncompressedSize64 {
			return ErrRecipientArchive
		}
		cursor += int64(descriptorSize)
	}
	directoryStart := cursor
	for _, entry := range archive.File {
		header, err := readRecipientZIPAt(file, size, cursor, 46)
		if err != nil || zipLE.Uint32(header) != 0x02014b50 ||
			zipLE.Uint32(header[34:]) != 0 || zipLE.Uint32(header[38:]) != entry.ExternalAttrs {
			return ErrRecipientArchive
		}
		nameSize, extraSize, commentSize := int(zipLE.Uint16(header[28:])),
			int(zipLE.Uint16(header[30:])), int(zipLE.Uint16(header[32:]))
		fields, err := readRecipientZIPAt(file, size, cursor+46, nameSize+extraSize+commentSize)
		if err != nil || nameSize != len(entry.Name) || commentSize != 0 ||
			!bytes.Equal(fields[:nameSize], []byte(entry.Name)) ||
			!bytes.Equal(fields[nameSize:nameSize+extraSize], entry.Extra) {
			return ErrRecipientArchive
		}
		cursor += 46 + int64(len(fields))
	}
	end, err := readRecipientZIPAt(file, size, size-22, 22)
	if err != nil || zipLE.Uint32(end) != 0x06054b50 || zipLE.Uint16(end[20:]) != 0 {
		return ErrRecipientArchive
	}
	count, directorySize, directoryOffset := uint64(zipLE.Uint16(end[10:])),
		uint64(zipLE.Uint32(end[12:])), uint64(zipLE.Uint32(end[16:]))
	zip64 := count == uint64(^uint16(0)) || directorySize == uint64(^uint32(0)) ||
		directoryOffset == uint64(^uint32(0))
	if zip64 {
		if cursor != size-98 {
			return ErrRecipientArchive
		}
		records, err := readRecipientZIPAt(file, size, cursor, 76)
		if err != nil || zipLE.Uint32(records) != 0x06064b50 || zipLE.Uint64(records[4:]) != 44 ||
			zipLE.Uint32(records[56:]) != 0x07064b50 || zipLE.Uint64(records[64:]) != uint64(cursor) {
			return ErrRecipientArchive
		}
		count, directorySize, directoryOffset = zipLE.Uint64(records[32:]),
			zipLE.Uint64(records[40:]), zipLE.Uint64(records[48:])
	} else if cursor != size-22 {
		return ErrRecipientArchive
	}
	if count != uint64(len(archive.File)) || directorySize != uint64(cursor-directoryStart) ||
		directoryOffset != uint64(directoryStart) {
		return ErrRecipientArchive
	}
	return nil
}
