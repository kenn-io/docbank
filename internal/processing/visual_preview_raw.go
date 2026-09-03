package processing

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"go.kenn.io/docbank/document"
)

const (
	visualPreviewRAWMaxIFDs            = 64
	visualPreviewRAWMaxIFDEntries      = 1024
	visualPreviewRAWCompressionTag     = 0x0103
	visualPreviewRAWJPEGCompression    = 7
	visualPreviewRAWStripOffsetsTag    = 0x0111
	visualPreviewRAWStripByteCountsTag = 0x0117
	visualPreviewRAWSubIFDsTag         = 0x014a
	visualPreviewRAWOffsetTag          = 0x0201
	visualPreviewRAWLengthTag          = 0x0202
	visualPreviewRAWOrientationTag     = 0x0112
)

type visualPreviewRAWLocation struct {
	offset      int64
	length      int64
	orientation int
}

func produceVisualPreviewCameraRAW(
	ctx context.Context,
	source io.ReadSeeker,
	sourceSize int64,
	mediaType string,
	base document.VisualPreviewV1,
) (VisualPreviewProduct, error) {
	readerAt := &seekReaderAt{seeker: source}
	var locations []visualPreviewRAWLocation
	var malformed bool
	var err error
	if mediaType == "image/x-fuji-raf" {
		location, found, invalid, inspectErr := inspectVisualPreviewRAF(readerAt, sourceSize)
		if found {
			locations = append(locations, location)
		}
		malformed, err = invalid, inspectErr
	} else {
		locations, malformed, err = inspectVisualPreviewTIFFRAW(readerAt, sourceSize)
	}
	if err != nil {
		return VisualPreviewProduct{}, sourceContentUnavailable(
			fmt.Errorf("inspecting camera RAW preview: %w", err))
	}
	if malformed {
		return failedVisualPreview(base, "decode_failed",
			"the verified camera RAW preview metadata is malformed"), nil
	}
	if len(locations) == 0 {
		base.State = document.VisualPreviewUnsupported
		base.Failure = &document.VisualPreviewFailureV1{
			Code:   "embedded_preview_unavailable",
			Detail: "the camera RAW original has no supported embedded JPEG preview",
		}
		return VisualPreviewProduct{Preview: base}, nil
	}
	slices.SortFunc(locations, func(left, right visualPreviewRAWLocation) int {
		if byLength := cmp.Compare(right.length, left.length); byLength != 0 {
			return byLength
		}
		return cmp.Compare(left.offset, right.offset)
	})
	var firstTerminal VisualPreviewProduct
	for index, location := range locations {
		preview := io.NewSectionReader(readerAt, location.offset, location.length)
		product, produceErr := produceVisualPreviewJPEGWithOrientation(ctx, preview, base, location.orientation)
		if produceErr != nil {
			return VisualPreviewProduct{}, produceErr
		}
		if product.Preview.State == document.VisualPreviewReady {
			return product, nil
		}
		if index == 0 {
			firstTerminal = product
		}
	}
	return firstTerminal, nil
}

func inspectVisualPreviewRAF(
	reader io.ReaderAt, sourceSize int64,
) (visualPreviewRAWLocation, bool, bool, error) {
	header, err := readSourceMetadataRange(reader, 0, min(sourceSize, sourceMetadataRAFHeaderBytes))
	if err != nil {
		return visualPreviewRAWLocation{}, false, false, err
	}
	if len(header) < sourceMetadataRAFHeaderBytes || !isSourceMetadataRAF(header) {
		return visualPreviewRAWLocation{}, false, true, nil
	}
	offset := int64(binary.BigEndian.Uint32(header[sourceMetadataRAFJPEGOffset:]))
	length := int64(binary.BigEndian.Uint32(header[sourceMetadataRAFJPEGOffset+4:]))
	if offset < sourceMetadataRAFHeaderBytes || !sourceMetadataRangeWithin(offset, length, sourceSize) {
		return visualPreviewRAWLocation{}, false, true, nil
	}
	return visualPreviewRAWLocation{offset: offset, length: length}, true, false, nil
}

func inspectVisualPreviewTIFFRAW(
	reader io.ReaderAt, sourceSize int64,
) ([]visualPreviewRAWLocation, bool, error) {
	header, err := readSourceMetadataRange(reader, 0, min(sourceSize, int64(8)))
	if err != nil {
		return nil, false, err
	}
	order, format, ok := exifTIFFHeader(header)
	if !ok || format != "tiff" || len(header) < 8 {
		return nil, true, nil
	}
	rootOffset := int64(order.Uint32(header[4:8]))
	if rootOffset < 8 || rootOffset >= sourceSize {
		return nil, true, nil
	}

	queue := []int64{rootOffset}
	seen := make(map[int64]struct{}, visualPreviewRAWMaxIFDs)
	rootOrientation := 0
	var candidates []visualPreviewRAWLocation
	malformed := false
	for len(queue) > 0 {
		offset := queue[0]
		queue = queue[1:]
		if offset == 0 {
			continue
		}
		if _, duplicate := seen[offset]; duplicate {
			continue
		}
		if len(seen) >= visualPreviewRAWMaxIFDs {
			return nil, true, nil
		}
		seen[offset] = struct{}{}

		entries, next, subIFDs, valid, readErr := readVisualPreviewRAWIFD(reader, sourceSize, order, offset)
		if readErr != nil {
			return nil, false, readErr
		}
		if !valid {
			malformed = true
			continue
		}
		if value, found := entries[visualPreviewRAWOrientationTag]; offset == rootOffset && found && value >= 1 && value <= 8 {
			rootOrientation = int(value)
		}
		candidateOrientation := rootOrientation
		if value, found := entries[visualPreviewRAWOrientationTag]; found && value >= 1 && value <= 8 {
			candidateOrientation = int(value)
		}
		previewOffset, hasOffset := entries[visualPreviewRAWOffsetTag]
		previewLength, hasLength := entries[visualPreviewRAWLengthTag]
		if hasOffset != hasLength {
			malformed = true
		} else if hasOffset {
			candidate := visualPreviewRAWLocation{
				offset: int64(previewOffset), length: int64(previewLength), orientation: candidateOrientation,
			}
			if !sourceMetadataRangeWithin(candidate.offset, candidate.length, sourceSize) {
				malformed = true
			} else {
				candidates = append(candidates, candidate)
			}
		}
		stripOffset, hasStripOffset := entries[visualPreviewRAWStripOffsetsTag]
		stripLength, hasStripLength := entries[visualPreviewRAWStripByteCountsTag]
		if hasStripOffset != hasStripLength {
			malformed = true
		} else if hasStripOffset && entries[visualPreviewRAWCompressionTag] == visualPreviewRAWJPEGCompression {
			candidate := visualPreviewRAWLocation{
				offset: int64(stripOffset), length: int64(stripLength), orientation: candidateOrientation,
			}
			if !sourceMetadataRangeWithin(candidate.offset, candidate.length, sourceSize) {
				malformed = true
			} else {
				candidates = append(candidates, candidate)
			}
		}
		queue = append(queue, subIFDs...)
		if next != 0 {
			queue = append(queue, next)
		}
	}
	if len(candidates) > 0 {
		return candidates, false, nil
	}
	return nil, malformed, nil
}

func readVisualPreviewRAWIFD(
	reader io.ReaderAt,
	sourceSize int64,
	order binary.ByteOrder,
	offset int64,
) (map[uint16]uint32, int64, []int64, bool, error) {
	if !sourceMetadataRangeWithin(offset, 2, sourceSize) {
		return nil, 0, nil, false, nil
	}
	countBytes, err := readSourceMetadataRange(reader, offset, 2)
	if err != nil {
		return nil, 0, nil, false, err
	}
	count := int64(order.Uint16(countBytes))
	if count > visualPreviewRAWMaxIFDEntries {
		return nil, 0, nil, false, nil
	}
	tableLength := count*12 + 4
	if !sourceMetadataRangeWithin(offset+2, tableLength, sourceSize) {
		return nil, 0, nil, false, nil
	}
	table, err := readSourceMetadataRange(reader, offset+2, tableLength)
	if err != nil {
		return nil, 0, nil, false, err
	}
	values := make(map[uint16]uint32, 7)
	seenValues := make(map[uint16]struct{}, 7)
	var subIFDs []int64
	for index := range count {
		entry := table[index*12 : index*12+12]
		tag := order.Uint16(entry)
		kind := order.Uint16(entry[2:])
		items := order.Uint32(entry[4:])
		if tag == visualPreviewRAWSubIFDsTag {
			offsets, valid, readErr := readVisualPreviewRAWSubIFDs(
				reader, sourceSize, order, kind, items, entry[8:12])
			if readErr != nil {
				return nil, 0, nil, false, readErr
			}
			if !valid {
				return nil, 0, nil, false, nil
			}
			subIFDs = append(subIFDs, offsets...)
			continue
		}
		if tag != visualPreviewRAWCompressionTag &&
			tag != visualPreviewRAWStripOffsetsTag && tag != visualPreviewRAWStripByteCountsTag &&
			tag != visualPreviewRAWOffsetTag && tag != visualPreviewRAWLengthTag &&
			tag != visualPreviewRAWOrientationTag {
			continue
		}
		if _, duplicate := seenValues[tag]; duplicate {
			return nil, 0, nil, false, nil
		}
		seenValues[tag] = struct{}{}
		value, valid := visualPreviewRAWScalar(order, kind, items, entry[8:12])
		if !valid {
			if (tag == visualPreviewRAWStripOffsetsTag || tag == visualPreviewRAWStripByteCountsTag) &&
				(kind == 3 || kind == 4) && items > 1 {
				continue
			}
			return nil, 0, nil, false, nil
		}
		values[tag] = value
	}
	next := int64(order.Uint32(table[count*12:]))
	if next >= sourceSize {
		return nil, 0, nil, false, nil
	}
	return values, next, subIFDs, true, nil
}

func readVisualPreviewRAWSubIFDs(
	reader io.ReaderAt,
	sourceSize int64,
	order binary.ByteOrder,
	kind uint16,
	items uint32,
	inline []byte,
) ([]int64, bool, error) {
	if kind != 4 || items == 0 || items > visualPreviewRAWMaxIFDs {
		return nil, false, nil
	}
	values := inline
	if items > 1 {
		offset := int64(order.Uint32(inline))
		length := int64(items) * 4
		if !sourceMetadataRangeWithin(offset, length, sourceSize) {
			return nil, false, nil
		}
		var err error
		values, err = readSourceMetadataRange(reader, offset, length)
		if err != nil {
			return nil, false, err
		}
	}
	offsets := make([]int64, 0, items)
	for index := range items {
		offset := int64(order.Uint32(values[index*4:]))
		if offset == 0 || offset >= sourceSize {
			return nil, false, nil
		}
		offsets = append(offsets, offset)
	}
	return offsets, true, nil
}

func visualPreviewRAWScalar(order binary.ByteOrder, kind uint16, items uint32, inline []byte) (uint32, bool) {
	if items != 1 {
		return 0, false
	}
	switch kind {
	case 3:
		return uint32(order.Uint16(inline)), true
	case 4:
		return order.Uint32(inline), true
	default:
		return 0, false
	}
}
