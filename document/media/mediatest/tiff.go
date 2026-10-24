package mediatest

import "encoding/binary"

// TIFFJPEGPreview describes an embedded JPEG and its optional TIFF metadata.
type TIFFJPEGPreview struct {
	Data        []byte
	Orientation uint16
	SingleStrip bool
}

// TIFFWithEmbeddedJPEG returns a TIFF with root orientation and linked JPEG previews.
//
//nolint:gosec // Synthetic fixture offsets and lengths fit in uint32.
func TIFFWithEmbeddedJPEG(rootOrientation uint16, previews ...TIFFJPEGPreview) []byte {
	const headerSize = 8
	const rootIFDSize = 2 + 12 + 4
	ifdOffsets := make([]int, len(previews))
	previewOffset := headerSize + rootIFDSize
	for index, preview := range previews {
		ifdOffsets[index] = previewOffset
		entries := 2
		if preview.SingleStrip {
			entries++
		}
		if preview.Orientation != 0 {
			entries++
		}
		previewOffset += 2 + entries*12 + 4
	}
	totalSize := previewOffset
	for _, preview := range previews {
		totalSize += len(preview.Data)
	}
	source := make([]byte, totalSize)
	copy(source, "II")
	binary.LittleEndian.PutUint16(source[2:4], 42)
	binary.LittleEndian.PutUint32(source[4:8], headerSize)
	writeEntry := func(offset int, tag, kind uint16, value uint32) {
		binary.LittleEndian.PutUint16(source[offset:offset+2], tag)
		binary.LittleEndian.PutUint16(source[offset+2:offset+4], kind)
		binary.LittleEndian.PutUint32(source[offset+4:offset+8], 1)
		binary.LittleEndian.PutUint32(source[offset+8:offset+12], value)
	}
	binary.LittleEndian.PutUint16(source[8:10], 1)
	writeEntry(10, 0x0112, 3, uint32(rootOrientation))
	if len(ifdOffsets) > 0 {
		binary.LittleEndian.PutUint32(source[22:26], uint32(ifdOffsets[0]))
	}
	for index, preview := range previews {
		ifdOffset := ifdOffsets[index]
		entryOffset := ifdOffset + 2
		offsetTag, lengthTag := uint16(0x0201), uint16(0x0202)
		if preview.SingleStrip {
			offsetTag, lengthTag = 0x0111, 0x0117
			writeEntry(entryOffset, 0x0103, 3, 7)
			entryOffset += 12
		}
		writeEntry(entryOffset, offsetTag, 4, uint32(previewOffset))
		writeEntry(entryOffset+12, lengthTag, 4, uint32(len(preview.Data)))
		entryOffset += 24
		if preview.Orientation != 0 {
			writeEntry(entryOffset, 0x0112, 3, uint32(preview.Orientation))
			entryOffset += 12
		}
		binary.LittleEndian.PutUint16(source[ifdOffset:ifdOffset+2], uint16((entryOffset-ifdOffset-2)/12))
		if index+1 < len(previews) {
			binary.LittleEndian.PutUint32(source[entryOffset:entryOffset+4], uint32(ifdOffsets[index+1]))
		}
		copy(source[previewOffset:], preview.Data)
		previewOffset += len(preview.Data)
	}
	return source
}
