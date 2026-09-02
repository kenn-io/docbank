package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	documentmedia "go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/internal/store"
)

const (
	maxSourceMetadataOriginalBytes       = 64 << 20
	maxSourceMetadataWindowBytes         = documentmedia.MaxBytes
	maxSourceMetadataFTYPBytes           = 1 << 20
	maxSourceMetadataCR3DirectoryBytes   = 20 << 20
	maxSourceMetadataRAFDirectoryBytes   = 1 << 20
	maxSourceMetadataAggregateValueBytes = 1 << 20
	maxSourceMetadataXMLDepth            = 64
	sourceMetadataRAFHeaderBytes         = 160
	sourceMetadataRAFJPEGOffset          = 84
	sourceMetadataRAFDirectoryOffset     = 92
	sourceMetadataRAFDirectoryLength     = 96
	sourceMetadataRAFImageSizeTag        = 0x0111
	sourceMetadataRAFSignature           = "FUJIFILMCCD-RAW"
	sourceMetadataCR3Brand               = "crx "
	rdfNamespace                         = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
	xmlNamespace                         = "http://www.w3.org/XML/1998/namespace"
	xmpBasicNamespace                    = "http://ns.adobe.com/xap/1.0/"
	xmpDublinCoreNamespace               = "http://purl.org/dc/elements/1.1/"
	xmpPDFNamespace                      = "http://ns.adobe.com/pdf/1.3/"
)

// sourceMetadataExtractorDescriptor names the local parser bundle. Bump the
// trailing version whenever any parser changes what it extracts: every vault
// then re-extracts every original, so the bump must be deliberate. A test
// pins the resulting fingerprint so the bump cannot be forgotten or made by
// accident.
const sourceMetadataExtractorDescriptor = "docbank-source-metadata:pdfcpu-info+xmp+pages," +
	"ooxml-core+custom,rfc5322,ical,visual-container+jpeg-tiff-raf-cr3-exif+mp4-created,media-id3:v15"

var (
	// SourceMetadataExtractorFingerprint is the stable identity of the local
	// parser bundle. Any semantic parser change must change the descriptor.
	SourceMetadataExtractorFingerprint = fingerprintSourceMetadataExtractor(
		sourceMetadataExtractorDescriptor)

	// errSourceMetadataBMFFMalformed marks deterministic box structure defects
	// in verified bytes, which become durable warnings rather than retryable
	// storage errors.
	errSourceMetadataBMFFMalformed = errors.New("malformed ISO base media file structure")
	errSourceContentUnavailable    = errors.New("source content unavailable")

	sourceMetadataCanonUUID = [16]byte{
		0x85, 0xc0, 0xb6, 0x87, 0x82, 0x0f, 0x11, 0xe0,
		0x81, 0x11, 0xf4, 0xce, 0x46, 0x2b, 0x6a, 0x48,
	}
)

// IsSourceContentUnavailable reports whether source metadata processing could
// not open or verify the catalog-authorized source bytes.
func IsSourceContentUnavailable(err error) bool {
	return errors.Is(err, errSourceContentUnavailable)
}

func fingerprintSourceMetadataExtractor(descriptor string) string {
	digest := sha256.Sum256([]byte(descriptor))
	return hex.EncodeToString(digest[:])
}

type sourceMetadataCatalog interface {
	PublishSourceMetadata(ctx context.Context, sourceSHA256, fingerprint string, canonical []byte) (store.SourceMetadataGeneration, error)
}

type sourceMetadataBlobReader interface {
	verifiedBlobReader
	OpenSeekableContext(ctx context.Context, hash string) (io.ReadSeekCloser, int64, error)
}

// BackfillSourceMetadataTargets processes a selected batch while allowing
// later originals to progress past a corrupt or temporarily unavailable one.
func BackfillSourceMetadataTargets(ctx context.Context, catalog sourceMetadataCatalog, blobs sourceMetadataBlobReader, targets []store.SourceMetadataTarget) (int, error) {
	completed := 0
	var targetErrors error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return completed, errors.Join(targetErrors, err)
		}
		metadata, err := sourceMetadataForTarget(ctx, blobs, target)
		if err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("extracting source metadata target %s: %w", target.SourceSHA256, err))
			continue
		}
		canonical, _, err := document.MarshalSourceMetadataV1(metadata)
		if err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("canonicalizing source metadata target %s: %w", target.SourceSHA256, err))
			continue
		}
		if _, err := catalog.PublishSourceMetadata(ctx, target.SourceSHA256, SourceMetadataExtractorFingerprint, canonical); err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("publishing source metadata target %s: %w", target.SourceSHA256, err))
			continue
		}
		completed++
	}
	return completed, targetErrors
}

func sourceMetadataForTarget(
	ctx context.Context, blobs sourceMetadataBlobReader, target store.SourceMetadataTarget,
) (document.SourceMetadataV1, error) {
	if target.Size <= maxSourceMetadataOriginalBytes {
		stream, size, err := blobs.OpenStreamContext(ctx, target.SourceSHA256)
		if err != nil {
			return document.SourceMetadataV1{}, sourceContentUnavailable(
				fmt.Errorf("opening verified stream: %w", err))
		}
		if size != target.Size {
			return document.SourceMetadataV1{}, sourceContentUnavailable(errors.Join(
				fmt.Errorf("size changed: catalog=%d opened=%d", target.Size, size), stream.Close()))
		}
		data, readErr := io.ReadAll(io.LimitReader(stream, size+1))
		if err := errors.Join(readErr, stream.Close()); err != nil {
			return document.SourceMetadataV1{}, sourceContentUnavailable(
				fmt.Errorf("verifying stream: %w", err))
		}
		if int64(len(data)) != size {
			return document.SourceMetadataV1{}, sourceContentUnavailable(fmt.Errorf(
				"length changed: catalog=%d read=%d", size, len(data)))
		}
		if size > maxSourceMetadataWindowBytes && boundedLargeMediaSignature(data) {
			return extractLargeSourceMetadata(bytes.NewReader(data), size)
		}
		return ExtractSourceMetadata(data), nil
	}

	reader, size, err := blobs.OpenSeekableContext(ctx, target.SourceSHA256)
	if err != nil {
		return document.SourceMetadataV1{}, sourceContentUnavailable(
			fmt.Errorf("opening seekable content: %w", err))
	}
	if size != target.Size {
		return document.SourceMetadataV1{}, sourceContentUnavailable(errors.Join(
			fmt.Errorf("size changed: catalog=%d opened=%d", target.Size, size), reader.Close()))
	}
	if err := verifySeekableSource(ctx, reader, target.SourceSHA256, size); err != nil {
		return document.SourceMetadataV1{}, sourceContentUnavailable(errors.Join(
			fmt.Errorf("verifying seekable content: %w", err), reader.Close()))
	}
	metadata, extractErr := extractLargeSourceMetadata(&seekReaderAt{seeker: reader}, size)
	return metadata, errors.Join(extractErr, reader.Close())
}

func sourceContentUnavailable(err error) error {
	return errors.Join(errSourceContentUnavailable, err)
}

func emptySourceMetadata() document.SourceMetadataV1 {
	return document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1,
		Fields: []document.SourceMetadataFieldV1{}, Warnings: []document.SourceMetadataWarningV1{}}
}

func verifySeekableSource(ctx context.Context, reader io.ReadSeeker, expectedSHA256 string, expectedSize int64) error {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to start: %w", err)
	}
	hasher := sha256.New()
	buffer := make([]byte, 128<<10)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			read += int64(n)
			_, _ = hasher.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	if read != expectedSize {
		return fmt.Errorf("length changed: catalog=%d read=%d", expectedSize, read)
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != expectedSHA256 {
		return fmt.Errorf("SHA-256 changed: expected=%s actual=%s", expectedSHA256, actual)
	}
	return nil
}

type seekReaderAt struct {
	seeker io.ReadSeeker
}

func (r *seekReaderAt) ReadAt(target []byte, offset int64) (int, error) {
	if _, err := r.seeker.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r.seeker, target)
}

func extractLargeSourceMetadata(reader io.ReaderAt, size int64) (document.SourceMetadataV1, error) {
	header, err := readSourceMetadataRange(reader, 0, min(size, sourceMetadataRAFHeaderBytes))
	if err != nil {
		return document.SourceMetadataV1{}, fmt.Errorf("reading media signature: %w", err)
	}
	switch {
	case isSourceMetadataRAF(header):
		return extractRAFSourceMetadata(reader, size)
	case isSourceMetadataCR3(header):
		return extractCR3SourceMetadata(reader, size)
	case bytes.HasPrefix(header, []byte{0xff, 0xd8}), exifTIFFSignature(header):
		window, readErr := readSourceMetadataRange(reader, 0, min(size, maxSourceMetadataWindowBytes))
		if readErr != nil {
			return document.SourceMetadataV1{}, fmt.Errorf("reading metadata window: %w", readErr)
		}
		metadata := ExtractSourceMetadata(window)
		metadata.Warnings = append(metadata.Warnings, sourceWarning(
			"metadata_window_limited", "container", "bytes",
			"only the bounded leading metadata window was inspected"))
		return metadata, nil
	case len(header) >= 12 && string(header[4:8]) == "ftyp":
		compact, warning, compactErr := compactMP4SourceMetadata(reader, size)
		if compactErr != nil {
			return document.SourceMetadataV1{}, compactErr
		}
		if warning != nil {
			metadata := emptySourceMetadata()
			metadata.Warnings = append(metadata.Warnings, *warning)
			return metadata, nil
		}
		return ExtractSourceMetadata(compact), nil
	default:
		metadata := emptySourceMetadata()
		metadata.Warnings = append(metadata.Warnings, sourceWarning(
			"input_too_large", "container", "bytes",
			"verified original exceeds the bounded parser for this format"))
		return metadata, nil
	}
}

func boundedLargeMediaSignature(data []byte) bool {
	return isSourceMetadataRAF(data) || isSourceMetadataCR3(data) || bytes.HasPrefix(data, []byte{0xff, 0xd8}) || exifTIFFSignature(data) ||
		len(data) >= 12 && string(data[4:8]) == "ftyp"
}

func isSourceMetadataRAF(data []byte) bool {
	return bytes.HasPrefix(data, []byte(sourceMetadataRAFSignature))
}

func isSourceMetadataCR3(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp" && string(data[8:12]) == sourceMetadataCR3Brand
}

func extractRAFSourceMetadata(reader io.ReaderAt, size int64) (document.SourceMetadataV1, error) {
	metadata := emptySourceMetadata()
	collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
	collector.string("media.container.format", "media.container", "Format", "raf", false)
	collector.string("media.container.kind", "media.container", "Kind", "image", false)

	header, err := readSourceMetadataRange(reader, 0, min(size, sourceMetadataRAFHeaderBytes))
	if err != nil {
		return document.SourceMetadataV1{}, fmt.Errorf("reading RAF header: %w", err)
	}
	if len(header) < sourceMetadataRAFHeaderBytes || !isSourceMetadataRAF(header) {
		collector.warn("unparseable_metadata", "media.container", "RAF", "RAF header is malformed")
		return canonicalSourceMetadataResult(metadata), nil
	}

	jpegOffset := int64(binary.BigEndian.Uint32(header[sourceMetadataRAFJPEGOffset:]))
	jpegLength := int64(binary.BigEndian.Uint32(header[sourceMetadataRAFJPEGOffset+4:]))
	directoryOffset := int64(binary.BigEndian.Uint32(header[sourceMetadataRAFDirectoryOffset:]))
	directoryLength := int64(binary.BigEndian.Uint32(header[sourceMetadataRAFDirectoryLength:]))
	if jpegOffset < sourceMetadataRAFHeaderBytes || directoryOffset < sourceMetadataRAFHeaderBytes ||
		!sourceMetadataRangeWithin(jpegOffset, jpegLength, size) ||
		!sourceMetadataRangeWithin(directoryOffset, directoryLength, size) ||
		sourceMetadataRangesOverlap(jpegOffset, jpegLength, directoryOffset, directoryLength) {
		collector.warn("unparseable_metadata", "media.container", "RAF", "RAF metadata offsets are invalid")
		return canonicalSourceMetadataResult(metadata), nil
	}

	jpegReadLength := min(jpegLength, maxSourceMetadataWindowBytes)
	jpeg, err := readSourceMetadataRange(reader, jpegOffset, jpegReadLength)
	if err != nil {
		return document.SourceMetadataV1{}, fmt.Errorf("reading RAF embedded JPEG: %w", err)
	}
	if !bytes.HasPrefix(jpeg, []byte{0xff, 0xd8}) {
		collector.warn("unparseable_metadata", "media.container", "RAF", "RAF embedded JPEG is malformed")
	} else {
		collector.extractJPEGExif(jpeg)
	}
	if jpegReadLength < jpegLength {
		collector.warn("metadata_window_limited", "media.container", "JPEG",
			"only the bounded RAF JPEG metadata window was inspected")
	}

	limited, err := collector.extractRAFDimensions(reader, directoryOffset, directoryLength)
	if err != nil {
		return document.SourceMetadataV1{}, err
	}
	if limited {
		collector.warn("metadata_window_limited", "media.container", "CFA",
			"only the bounded RAF raw-metadata directory was inspected")
	}
	return canonicalSourceMetadataResult(metadata), nil
}

func sourceMetadataRangeWithin(offset, length, size int64) bool {
	return offset >= 0 && length > 0 && offset <= size && length <= size-offset
}

func sourceMetadataRangesOverlap(leftOffset, leftLength, rightOffset, rightLength int64) bool {
	return leftOffset < rightOffset+rightLength && rightOffset < leftOffset+leftLength
}

func (c *metadataCollector) extractRAFDimensions(reader io.ReaderAt, offset, length int64) (bool, error) {
	if length < 4 {
		c.warn("unparseable_metadata", "media.container", "CFA", "RAF raw-metadata directory is malformed")
		return false, nil
	}
	header, err := readSourceMetadataRange(reader, offset, 4)
	if err != nil {
		return false, fmt.Errorf("reading RAF raw-metadata directory: %w", err)
	}
	entryCount := int64(binary.BigEndian.Uint32(header))
	position := offset + 4
	directoryEnd := offset + length
	limit := min(directoryEnd, offset+maxSourceMetadataRAFDirectoryBytes)
	for range entryCount {
		if position > directoryEnd-4 {
			c.warn("unparseable_metadata", "media.container", "CFA", "RAF raw-metadata directory is malformed")
			return false, nil
		}
		if position > limit-4 {
			return true, nil
		}
		entryHeader, readErr := readSourceMetadataRange(reader, position, 4)
		if readErr != nil {
			return false, fmt.Errorf("reading RAF raw-metadata entry: %w", readErr)
		}
		tag := binary.BigEndian.Uint16(entryHeader)
		length := int64(binary.BigEndian.Uint16(entryHeader[2:]))
		position += 4
		if length > directoryEnd-position {
			c.warn("unparseable_metadata", "media.container", "CFA", "RAF raw-metadata entry is malformed")
			return false, nil
		}
		if tag == sourceMetadataRAFImageSizeTag {
			if length < 4 {
				c.warn("unparseable_metadata", "media.container", "CFA", "RAF image-size entry is malformed")
				return false, nil
			}
			if position > limit-4 {
				return true, nil
			}
			dimensions, readErr := readSourceMetadataRange(reader, position, 4)
			if readErr != nil {
				return false, fmt.Errorf("reading RAF image dimensions: %w", readErr)
			}
			height := int64(binary.BigEndian.Uint16(dimensions))
			width := int64(binary.BigEndian.Uint16(dimensions[2:]))
			if width > 0 && height > 0 {
				c.integer("media.container.width_px", "media.container", "RAFImageWidth", width)
				c.integer("media.container.height_px", "media.container", "RAFImageLength", height)
			}
			return false, nil
		}
		if position > math.MaxInt64-length {
			c.warn("unparseable_metadata", "media.container", "CFA", "RAF raw-metadata offsets overflow")
			return false, nil
		}
		position += length
	}
	return false, nil
}

func extractCR3SourceMetadata(reader io.ReaderAt, size int64) (document.SourceMetadataV1, error) {
	metadata := emptySourceMetadata()
	collector := metadataCollector{record: &metadata, seen: map[string]bool{}}
	collector.string("media.container.format", "media.container", "Format", "cr3", false)
	collector.string("media.container.kind", "media.container", "Kind", "image", false)

	directories, warning, err := readCR3MetadataDirectories(reader, size)
	if err != nil {
		return document.SourceMetadataV1{}, err
	}
	if warning != nil {
		metadata.Warnings = append(metadata.Warnings, *warning)
		return canonicalSourceMetadataResult(metadata), nil
	}

	cmt1, found := directories["CMT1"]
	if !found {
		collector.warn("unparseable_metadata", "media.container", "CMT1", "CR3 is missing its primary image metadata")
		return canonicalSourceMetadataResult(metadata), nil
	}
	rootReader, root, ok := sourceMetadataTIFFRoot(cmt1)
	if !ok || len(root) == 0 {
		collector.warn("unparseable_metadata", "media.container", "CMT1", "CR3 primary image metadata is malformed")
		return canonicalSourceMetadataResult(metadata), nil
	}
	collector.extractExifRoot(rootReader, root)
	width, widthFound := exifUnsigned(rootReader, root[0x0100])
	height, heightFound := exifUnsigned(rootReader, root[0x0101])
	widthSource, heightSource := "ImageWidth", "ImageLength"

	if cmt2, found := directories["CMT2"]; found {
		exifReader, exif, valid := sourceMetadataTIFFRoot(cmt2)
		if !valid || len(exif) == 0 {
			collector.warn("unparseable_metadata", "image.exif", "CMT2", "CR3 EXIF metadata is malformed")
		} else {
			collector.extractExifDetails(exifReader, exif)
			if exifWidth, found := exifUnsigned(exifReader, exif[0xa002]); found && exifWidth > 0 {
				width, widthFound = exifWidth, true
				widthSource = "PixelXDimension"
			}
			if exifHeight, found := exifUnsigned(exifReader, exif[0xa003]); found && exifHeight > 0 {
				height, heightFound = exifHeight, true
				heightSource = "PixelYDimension"
			}
		}
	}
	if widthFound && width > 0 {
		collector.integer("media.container.width_px", "media.container", widthSource, width)
	}
	if heightFound && height > 0 {
		collector.integer("media.container.height_px", "media.container", heightSource, height)
	}
	if cmt4, found := directories["CMT4"]; found {
		gpsReader, gps, valid := sourceMetadataTIFFRootEntries(cmt4)
		if !valid || len(gps) == 0 {
			collector.warn("unparseable_metadata", "image.exif", "CMT4", "CR3 GPS metadata is malformed")
		} else {
			collector.exifGPS(gpsReader, gps)
		}
	}
	return canonicalSourceMetadataResult(metadata), nil
}

func sourceMetadataTIFFRoot(data []byte) (exifReader, map[uint16][]byte, bool) {
	reader, ok := newExifReader(data)
	if !ok {
		return exifReader{}, nil, false
	}
	rootOffset := reader.u32(4)
	if rootOffset < 8 {
		return exifReader{}, nil, false
	}
	entries, _ := reader.typedEntries(rootOffset)
	values := make(map[uint16][]byte, len(entries))
	for tag, entry := range entries {
		values[tag] = entry.value
	}
	return reader, values, true
}

func sourceMetadataTIFFRootEntries(data []byte) (exifReader, map[uint16]exifEntry, bool) {
	reader, ok := newExifReader(data)
	if !ok {
		return exifReader{}, nil, false
	}
	rootOffset := reader.u32(4)
	if rootOffset < 8 {
		return exifReader{}, nil, false
	}
	entries, valid := reader.typedEntries(rootOffset)
	return reader, entries, valid
}

func readCR3MetadataDirectories(
	reader io.ReaderAt, sourceSize int64,
) (map[string][]byte, *document.SourceMetadataWarningV1, error) {
	moovOffset, moovSize, warning, err := findCR3Moov(reader, sourceSize)
	if err != nil || warning != nil {
		return nil, warning, err
	}

	for offset, remaining := moovOffset, moovSize; remaining > 0; {
		box, boxErr := readSourceMetadataMP4Box(reader, offset, remaining)
		if errors.Is(boxErr, errSourceMetadataBMFFMalformed) {
			warning := sourceWarning("unparseable_metadata", "media.container", "moov",
				"CR3 movie metadata box structure is malformed")
			return nil, &warning, nil
		}
		if boxErr != nil {
			return nil, nil, fmt.Errorf("reading CR3 movie metadata box at %d: %w", offset, boxErr)
		}
		if box.kind == "uuid" {
			payloadOffset := offset + box.headerSize
			payloadSize := box.size - box.headerSize
			if payloadSize < int64(len(sourceMetadataCanonUUID)) {
				warning := sourceWarning("unparseable_metadata", "media.container", "uuid",
					"CR3 Canon metadata container is malformed")
				return nil, &warning, nil
			}
			uuid, readErr := readSourceMetadataRange(reader, payloadOffset, int64(len(sourceMetadataCanonUUID)))
			if readErr != nil {
				return nil, nil, fmt.Errorf("reading CR3 metadata identifier: %w", readErr)
			}
			if bytes.Equal(uuid, sourceMetadataCanonUUID[:]) {
				return readCR3CanonDirectories(reader, payloadOffset+int64(len(uuid)), payloadSize-int64(len(uuid)))
			}
		}
		offset += box.size
		remaining -= box.size
	}
	missingWarning := sourceWarning("unparseable_metadata", "media.container", "uuid",
		"CR3 is missing its Canon metadata container")
	return nil, &missingWarning, nil
}

// maxSourceMetadataTopLevelBoxes bounds the top-level box walk. Real files
// hold a handful of boxes; a header-sized run of empty boxes is an attack on
// the walker's time, not metadata.
const maxSourceMetadataTopLevelBoxes = 4096

func findCR3Moov(
	reader io.ReaderAt, sourceSize int64,
) (int64, int64, *document.SourceMetadataWarningV1, error) {
	var moovOffset, moovSize int64
	boxes := 0
	for offset := int64(0); offset < sourceSize; {
		box, err := readSourceMetadataMP4Box(reader, offset, sourceSize-offset)
		boxes++
		if errors.Is(err, errSourceMetadataBMFFMalformed) || boxes > maxSourceMetadataTopLevelBoxes {
			warning := sourceWarning("unparseable_metadata", "media.container", "CR3",
				"CR3 top-level box structure is malformed")
			return 0, 0, &warning, nil
		}
		if err != nil {
			return 0, 0, nil, fmt.Errorf("reading CR3 box at %d: %w", offset, err)
		}
		if offset == 0 && box.kind != "ftyp" {
			warning := sourceWarning("unparseable_metadata", "media.container", "CR3",
				"CR3 is missing its file-type box")
			return 0, 0, &warning, nil
		}
		if box.kind == "moov" {
			if moovSize != 0 {
				warning := sourceWarning("unparseable_metadata", "media.container", "moov",
					"CR3 contains more than one movie metadata box")
				return 0, 0, &warning, nil
			}
			moovOffset = offset + box.headerSize
			moovSize = box.size - box.headerSize
		}
		offset += box.size
	}
	if moovSize == 0 {
		warning := sourceWarning("unparseable_metadata", "media.container", "moov",
			"CR3 is missing its movie metadata box")
		return 0, 0, &warning, nil
	}
	return moovOffset, moovSize, nil, nil
}

func readCR3CanonDirectories(
	reader io.ReaderAt, offset, size int64,
) (map[string][]byte, *document.SourceMetadataWarningV1, error) {
	directories := map[string][]byte{}
	var aggregate int64
	for remaining := size; remaining > 0; {
		box, err := readSourceMetadataMP4Box(reader, offset, remaining)
		if errors.Is(err, errSourceMetadataBMFFMalformed) {
			warning := sourceWarning("unparseable_metadata", "media.container", "uuid",
				"CR3 Canon metadata box structure is malformed")
			return nil, &warning, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading CR3 Canon metadata box at %d: %w", offset, err)
		}
		if box.kind == "CMT1" || box.kind == "CMT2" || box.kind == "CMT4" {
			if _, exists := directories[box.kind]; exists {
				warning := sourceWarning("unparseable_metadata", "media.container", box.kind,
					"CR3 contains duplicate metadata directories")
				return nil, &warning, nil
			}
			payloadSize := box.size - box.headerSize
			if payloadSize <= 0 || payloadSize > maxSourceMetadataCR3DirectoryBytes-aggregate {
				warning := sourceWarning("metadata_too_large", "media.container", box.kind,
					"CR3 metadata exceeds the bounded inspection limit")
				return nil, &warning, nil
			}
			payload, readErr := readSourceMetadataRange(reader, offset+box.headerSize, payloadSize)
			if readErr != nil {
				return nil, nil, fmt.Errorf("reading CR3 %s metadata: %w", box.kind, readErr)
			}
			directories[box.kind] = payload
			aggregate += payloadSize
		}
		offset += box.size
		remaining -= box.size
	}
	return directories, nil, nil
}

func readSourceMetadataRange(reader io.ReaderAt, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 || length > maxSourceMetadataWindowBytes || offset > math.MaxInt64-length {
		return nil, errors.New("source metadata range is invalid")
	}
	result := make([]byte, int(length))
	if length == 0 {
		return result, nil
	}
	if _, err := reader.ReadAt(result, offset); err != nil {
		return nil, err
	}
	return result, nil
}

type sourceMetadataMP4Box struct {
	kind       string
	headerSize int64
	size       int64
}

func compactMP4SourceMetadata(
	reader io.ReaderAt, sourceSize int64,
) ([]byte, *document.SourceMetadataWarningV1, error) {
	var ftyp []byte
	var moov []byte
	moovCount := 0
	fragmented := false
	boxes := 0
	for offset := int64(0); offset < sourceSize; {
		box, err := readSourceMetadataMP4Box(reader, offset, sourceSize-offset)
		boxes++
		if errors.Is(err, errSourceMetadataBMFFMalformed) || boxes > maxSourceMetadataTopLevelBoxes {
			warning := sourceWarning("unparseable_metadata", "media.container", "MP4",
				"MP4 top-level box structure is malformed")
			return nil, &warning, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("reading MP4 box at %d: %w", offset, err)
		}
		if (offset == 0 && box.kind != "ftyp") || sourceMetadataMP4StructuralBox(box.kind) {
			warning := sourceWarning("unparseable_metadata", "media.container", "MP4",
				"MP4 top-level box structure is malformed")
			return nil, &warning, nil
		}
		switch box.kind {
		case "ftyp":
			if offset == 0 {
				if box.size > maxSourceMetadataFTYPBytes {
					warning := sourceWarning("metadata_too_large", "media.container", "ftyp",
						"MP4 file-type metadata exceeds the bounded inspection limit")
					return nil, &warning, nil
				}
				ftyp, err = readSourceMetadataRange(reader, offset, box.size)
				if err != nil {
					return nil, nil, fmt.Errorf("reading MP4 file type: %w", err)
				}
			}
		case "moov":
			moovCount++
			if moovCount > 1 {
				warning := sourceWarning("unparseable_metadata", "media.container", "moov",
					"MP4 contains more than one movie metadata box")
				return nil, &warning, nil
			}
			if box.size > maxSourceMetadataWindowBytes-int64(len(ftyp)) {
				warning := sourceWarning("metadata_too_large", "media.container", "moov",
					"MP4 movie metadata exceeds the bounded inspection limit")
				return nil, &warning, nil
			}
			moov, err = readSourceMetadataRange(reader, offset, box.size)
			if err != nil {
				return nil, nil, fmt.Errorf("reading MP4 movie metadata: %w", err)
			}
		case "moof":
			fragmented = true
		}
		if box.size <= 0 || offset > math.MaxInt64-box.size {
			warning := sourceWarning("unparseable_metadata", "media.container", "MP4",
				"MP4 box offsets overflow the source length")
			return nil, &warning, nil
		}
		offset += box.size
	}
	if len(ftyp) == 0 || len(moov) == 0 {
		warning := sourceWarning("unparseable_metadata", "media.container", "MP4",
			"MP4 is missing required file-type or movie metadata")
		return nil, &warning, nil
	}
	compact := make([]byte, 0, len(ftyp)+len(moov)+8)
	compact = append(compact, ftyp...)
	compact = append(compact, moov...)
	if fragmented {
		compact = append(compact, 0, 0, 0, 8, 'm', 'o', 'o', 'f')
	}
	return compact, nil, nil
}

func readSourceMetadataMP4Box(
	reader io.ReaderAt, offset, remaining int64,
) (sourceMetadataMP4Box, error) {
	if remaining < 8 {
		return sourceMetadataMP4Box{}, fmt.Errorf("truncated MP4 box header: %w", errSourceMetadataBMFFMalformed)
	}
	header, err := readSourceMetadataRange(reader, offset, min(remaining, 16))
	if err != nil {
		return sourceMetadataMP4Box{}, err
	}
	box := sourceMetadataMP4Box{kind: string(header[4:8]), headerSize: 8}
	size32 := binary.BigEndian.Uint32(header[:4])
	switch size32 {
	case 0:
		box.size = remaining
	case 1:
		if len(header) < 16 {
			return sourceMetadataMP4Box{}, fmt.Errorf("truncated MP4 large-size header: %w", errSourceMetadataBMFFMalformed)
		}
		large := binary.BigEndian.Uint64(header[8:16])
		if large < 16 || large > math.MaxInt64 {
			return sourceMetadataMP4Box{}, fmt.Errorf("invalid MP4 large-size box: %w", errSourceMetadataBMFFMalformed)
		}
		box.headerSize = 16
		box.size = int64(large)
	default:
		box.size = int64(size32)
	}
	if box.size < box.headerSize || box.size > remaining {
		return sourceMetadataMP4Box{}, fmt.Errorf("MP4 box size exceeds its parent: %w", errSourceMetadataBMFFMalformed)
	}
	return box, nil
}

func sourceMetadataMP4StructuralBox(kind string) bool {
	switch kind {
	case "trak", "edts", "elst", "mdia", "minf", "stbl", "stsd", "stts", "ctts", "stsz", "stz2",
		"mvhd", "tkhd", "hdlr", "mdhd":
		return true
	default:
		return false
	}
}

// ExtractSourceMetadata performs bounded, format-signature-based local
// extraction. It returns warnings for unsupported or malformed input and does
// not guess from a filename, path, MIME declaration, or caller metadata.
func ExtractSourceMetadata(data []byte) document.SourceMetadataV1 {
	result := emptySourceMetadata()
	collector := metadataCollector{record: &result, seen: map[string]bool{}}
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		collector.extractPDF(data)
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		collector.extractOOXML(data)
	case bytes.HasPrefix(data, []byte("BEGIN:VCALENDAR")):
		collector.extractCalendar(data)
	case bytes.HasPrefix(data, []byte("ID3")):
		collector.extractID3(data)
	case isSourceMetadataRAF(data):
		metadata, err := extractRAFSourceMetadata(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			result.Warnings = append(result.Warnings, sourceWarning(
				"unparseable_metadata", "media.container", "RAF", "RAF metadata could not be read"))
			return canonicalSourceMetadataResult(result)
		}
		return metadata
	case isSourceMetadataCR3(data):
		metadata, err := extractCR3SourceMetadata(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			result.Warnings = append(result.Warnings, sourceWarning(
				"unparseable_metadata", "media.container", "CR3", "CR3 metadata could not be read"))
			return canonicalSourceMetadataResult(result)
		}
		return metadata
	case visualContainerSignature(data):
		collector.extractVisual(data)
	case exifTIFFSignature(data):
		collector.extractTIFFVisual(data)
	default:
		if message, err := mail.ReadMessage(bytes.NewReader(data)); err == nil {
			collector.extractEmail(message)
		} else {
			result.Warnings = append(result.Warnings, sourceWarning("unsupported_format", "container", "signature", "no supported embedded metadata container was recognized"))
		}
	}
	return canonicalSourceMetadataResult(result)
}

func canonicalSourceMetadataResult(metadata document.SourceMetadataV1) document.SourceMetadataV1 {
	if _, _, err := document.MarshalSourceMetadataV1(metadata); err == nil {
		return metadata
	}
	fallback := emptySourceMetadata()
	fallback.Warnings = append(fallback.Warnings, sourceWarning(
		"extraction_limit", "container", "metadata",
		"embedded metadata exceeded the canonical extraction bounds"))
	return fallback
}

type metadataCollector struct {
	record     *document.SourceMetadataV1
	seen       map[string]bool
	valueBytes int
}

func validSourceMetadataLabel(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= document.MaxSourceMetadataLabelBytes && utf8.ValidString(value)
}

func boundedSourceMetadataLabel(value, fallback string) string {
	value = strings.TrimSpace(value)
	if validSourceMetadataLabel(value) {
		return value
	}
	return fallback
}

func (c *metadataCollector) fieldLabelsAllowed(key, namespace, source string) bool {
	if document.SourceMetadataCanonicalKeyAllowed(key) && validSourceMetadataLabel(namespace) &&
		validSourceMetadataLabel(source) {
		return true
	}
	c.warn("invalid_label", namespace, source, "embedded metadata with an invalid label was omitted")
	return false
}

func (c *metadataCollector) reserveValueBytes(size int, namespace, source string) bool {
	if size < 0 || size > maxSourceMetadataAggregateValueBytes-c.valueBytes {
		c.warn("aggregate_value_limit", namespace, source, "additional embedded values were omitted")
		return false
	}
	c.valueBytes += size
	return true
}

func (c *metadataCollector) string(key, namespace, source, value string, sensitive bool) {
	value = strings.TrimSpace(value)
	if value == "" || c.seen[key] {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	if !utf8.ValidString(value) {
		c.warn("invalid_utf8", namespace, source, "embedded value was omitted")
		return
	}
	if len(value) > document.MaxSourceMetadataValueBytes {
		c.warn("value_too_large", namespace, source, "embedded value was omitted")
		return
	}
	if !c.reserveValueBytes(len(value), namespace, source) {
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace,
		SourceField: source, Sensitive: sensitive, Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new(value)}})
}
func (c *metadataCollector) strings(key, namespace, source string, values []string) {
	filtered := make([]string, 0, len(values))
	valueBytes := 0
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			if !utf8.ValidString(value) {
				c.warn("invalid_utf8", namespace, source, "embedded list value was omitted")
				continue
			}
			if len(value) > document.MaxSourceMetadataValueBytes {
				c.warn("value_too_large", namespace, source, "embedded list value was omitted")
				continue
			}
			filtered = append(filtered, value)
			valueBytes += len(value)
		}
	}
	if len(filtered) == 0 || c.seen[key] {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	if len(filtered) > document.MaxSourceMetadataListValues {
		c.warn("value_too_large", namespace, source, "embedded list was omitted")
		return
	}
	if !c.reserveValueBytes(valueBytes, namespace, source) {
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace,
		SourceField: source, Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataStringList, Strings: filtered}})
}
func (c *metadataCollector) integer(key, namespace, source string, value int64) {
	if c.seen[key] {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace, SourceField: source,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataInteger, Integer: &value}})
}
func (c *metadataCollector) exifNumber(key, source string, value float64) {
	const namespace = "image.exif"
	if c.seen[key] || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace, SourceField: source,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataNumber, Number: &value}})
}
func (c *metadataCollector) boolean(key, namespace, source string, value bool) {
	if c.seen[key] {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace, SourceField: source,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataBoolean, Boolean: &value}})
}
func (c *metadataCollector) timestamp(key, namespace, source, raw string) {
	raw = strings.TrimSpace(raw)
	if c.seen[key] || raw == "" {
		return
	}
	if !c.fieldLabelsAllowed(key, namespace, source) {
		return
	}
	if len(c.record.Fields) >= document.MaxSourceMetadataFields {
		c.warn("field_limit", namespace, source, "additional embedded fields were omitted")
		return
	}
	if len(raw) > document.MaxSourceMetadataValueBytes {
		c.warn("value_too_large", namespace, source, "embedded timestamp was omitted")
		return
	}
	stamp, ok := parseSourceTimestamp(raw)
	if !ok {
		c.warn("unparseable_timestamp", namespace, source, "embedded timestamp was not coerced")
		return
	}
	if !c.reserveValueBytes(len(stamp.Raw)+len(stamp.Normalized)+len(stamp.Offset), namespace, source) {
		return
	}
	c.seen[key] = true
	c.record.Fields = append(c.record.Fields, document.SourceMetadataFieldV1{Key: key, Namespace: namespace, SourceField: source,
		Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataTimestamp, Timestamp: &stamp}})
}
func (c *metadataCollector) warn(code, namespace, source, detail string) {
	if len(c.record.Warnings) >= document.MaxSourceMetadataWarnings {
		return
	}
	code = boundedSourceMetadataLabel(code, "extraction_warning")
	namespace = boundedSourceMetadataLabel(namespace, "container")
	source = boundedSourceMetadataLabel(source, "metadata")
	if !utf8.ValidString(detail) || len(detail) > document.MaxSourceMetadataValueBytes {
		detail = "embedded metadata warning detail was omitted"
	}
	c.record.Warnings = append(c.record.Warnings, sourceWarning(code, namespace, source, detail))
}
func sourceWarning(code, namespace, source, detail string) document.SourceMetadataWarningV1 {
	return document.SourceMetadataWarningV1{Code: code, Namespace: namespace, SourceField: source, Detail: detail}
}

func (c *metadataCollector) extractPDF(data []byte) {
	metadata, err := documentmedia.ReadPDFMetadata(data)
	if err != nil {
		c.warn("unparseable_pdf_pages", "pdf.info", "Pages", "PDF page tree could not be verified")
		return
	}
	c.string("title", "pdf.info", "Title", metadata.Info.Title, false)
	c.strings("creators", "pdf.info", "Author", splitValues(metadata.Info.Author))
	c.string("subject", "pdf.info", "Subject", metadata.Info.Subject, false)
	c.strings("keywords", "pdf.info", "Keywords", splitValues(metadata.Info.Keywords))
	c.timestamp("created", "pdf.info", "CreationDate", metadata.Info.CreationDate)
	c.timestamp("modified", "pdf.info", "ModDate", metadata.Info.ModDate)
	c.integer("page_count", "pdf.info", "Pages", metadata.Pages)
	for _, issue := range metadata.Issues {
		namespace := "pdf.info"
		if issue.SourceField == "XMP" {
			namespace = "xmp"
		}
		c.warn("unparseable_pdf_metadata", namespace, issue.SourceField, "optional PDF metadata was omitted")
	}
	if len(metadata.XMP) != 0 {
		c.extractXMLText(metadata.XMP, "xmp")
	}
}

func (c *metadataCollector) extractOOXML(data []byte) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		c.warn("malformed_container", "office.core", "zip", "OOXML container could not be read")
		return
	}
	if len(reader.File) > 4096 {
		c.warn("container_limit", "office.core", "zip", "OOXML container has too many entries")
		return
	}
	var extracted int64
	for _, file := range reader.File {
		if file.UncompressedSize64 > 4<<20 {
			continue
		}
		if file.Name != "docProps/core.xml" && file.Name != "docProps/custom.xml" && file.Name != "docProps/app.xml" {
			continue
		}
		extracted += int64(file.UncompressedSize64)
		if extracted > 16<<20 {
			c.warn("container_limit", "office.core", "zip", "OOXML property data exceeds extraction limit")
			return
		}
		stream, err := file.Open()
		if err != nil {
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(stream, 4<<20+1))
		_ = stream.Close()
		if readErr != nil || len(payload) > 4<<20 {
			c.warn("malformed_container", "office.core", file.Name, "property document could not be read")
			continue
		}
		switch file.Name {
		case "docProps/app.xml":
			c.extractOfficeApp(payload)
		case "docProps/custom.xml":
			c.extractOfficeCustom(payload)
		default:
			c.extractXMLText(payload, strings.TrimSuffix(strings.ReplaceAll(file.Name, "docProps/", "office."), ".xml"))
		}
	}
}

type boundedSourceMetadataText struct {
	value    []byte
	overflow bool
}

func (text *boundedSourceMetadataText) write(value []byte) {
	remaining := document.MaxSourceMetadataValueBytes - len(text.value)
	if remaining <= 0 {
		text.overflow = text.overflow || len(value) != 0
		return
	}
	if len(value) > remaining {
		text.value = append(text.value, value[:remaining]...)
		text.overflow = true
		return
	}
	text.value = append(text.value, value...)
}

func (text *boundedSourceMetadataText) string() string {
	return string(text.value)
}

type sourceMetadataXMLMember struct {
	value    string
	language string
}

func (c *metadataCollector) extractXMLText(data []byte, namespace string) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	type element struct {
		name    xml.Name
		text    boundedSourceMetadataText
		members []sourceMetadataXMLMember
		lang    string
	}
	var stack []element
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			c.warnMalformedXML(namespace, "XML")
			return
		}
		switch value := token.(type) {
		case xml.StartElement:
			if len(stack) >= maxSourceMetadataXMLDepth {
				c.warn("xml_depth_limit", namespace, value.Name.Local, "embedded XML nesting exceeds the extraction limit")
				return
			}
			current := element{name: value.Name}
			for _, attribute := range value.Attr {
				if attribute.Name.Space == xmlNamespace && attribute.Name.Local == "lang" {
					current.lang = attribute.Value
				}
				if namespace == "xmp" && xmpAttributeAllowed(attribute.Name) {
					c.extractXMLValue(namespace, attribute.Name.Local, attribute.Value)
				}
			}
			stack = append(stack, current)
		case xml.CharData:
			for index := range stack {
				stack[index].text.write(value)
			}
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if current.text.overflow {
				c.warn("value_too_large", namespace, current.name.Local, "embedded XML value was omitted")
				continue
			}
			if namespace == "xmp" && current.name.Space == rdfNamespace && current.name.Local == "li" {
				for index := len(stack) - 1; index >= 0; index-- {
					if !xmpPropertyAllowed(stack[index].name) {
						continue
					}
					if len(stack[index].members) >= document.MaxSourceMetadataListValues {
						c.warn("value_too_large", namespace, stack[index].name.Local, "embedded XML collection was omitted")
						break
					}
					stack[index].members = append(stack[index].members, sourceMetadataXMLMember{
						value: current.text.string(), language: current.lang,
					})
					break
				}
				continue
			}
			if namespace == "xmp" && !xmpPropertyAllowed(current.name) {
				continue
			}
			if len(current.members) != 0 {
				c.extractXMLCollection(namespace, current.name.Local, current.members)
				continue
			}
			c.extractXMLValue(namespace, current.name.Local, current.text.string())
		}
	}
}

func (c *metadataCollector) warnMalformedXML(namespace, source string) {
	c.warn("malformed_metadata", namespace, source, "embedded XML metadata is malformed")
}

func xmpAttributeAllowed(name xml.Name) bool {
	return xmpPropertyAllowed(name)
}

func xmpPropertyAllowed(name xml.Name) bool {
	switch name.Space {
	case xmpBasicNamespace:
		return strings.EqualFold(name.Local, "CreateDate") || strings.EqualFold(name.Local, "ModifyDate")
	case xmpDublinCoreNamespace:
		switch strings.ToLower(name.Local) {
		case "title", "creator", "subject", "description", "language":
			return true
		default:
			return false
		}
	case xmpPDFNamespace:
		return strings.EqualFold(name.Local, "Keywords")
	default:
		return false
	}
}

func (c *metadataCollector) extractXMLCollection(namespace, name string, members []sourceMetadataXMLMember) {
	values := make([]string, 0, len(members))
	defaultValue := ""
	for _, member := range members {
		if value := strings.TrimSpace(member.value); value != "" {
			values = append(values, value)
			if strings.EqualFold(member.language, "x-default") {
				defaultValue = value
			}
		}
	}
	if len(values) == 0 {
		return
	}
	switch strings.ToLower(name) {
	case "creator", "author":
		c.strings("creators", namespace, name, values)
	case "keywords":
		c.strings("keywords", namespace, name, values)
	default:
		if defaultValue == "" {
			defaultValue = values[0]
		}
		c.extractXMLValue(namespace, name, defaultValue)
	}
}

func (c *metadataCollector) extractXMLValue(namespace, name, text string) {
	switch strings.ToLower(name) {
	case "title":
		c.string("title", namespace, name, text, false)
	case "creator", "author":
		c.strings("creators", namespace, name, splitValues(text))
	case "subject":
		c.string("subject", namespace, name, text, false)
	case "description":
		c.string("description", namespace, name, text, false)
	case "keywords":
		c.strings("keywords", namespace, name, splitValues(text))
	case "language":
		c.string("language", namespace, name, text, false)
	case "created", "createdate":
		c.timestamp("created", namespace, name, text)
	case "modified", "modifydate":
		c.timestamp("modified", namespace, name, text)
	default:
		if strings.HasPrefix(namespace, "office.custom") {
			key := "office.custom." + canonicalFieldName(name)
			if document.SourceMetadataCanonicalKeyAllowed(key) {
				c.string(key, namespace, name, text, false)
			}
		}
	}
}

func (c *metadataCollector) extractOfficeApp(data []byte) {
	var properties struct {
		Pages  string `xml:"Pages"`
		Slides string `xml:"Slides"`
		Words  string `xml:"Words"`
	}
	if err := xml.Unmarshal(data, &properties); err != nil {
		c.warnMalformedXML("office.core", "app.xml")
		return
	}
	for _, field := range []struct {
		name string
		text string
	}{{"Pages", properties.Pages}, {"Slides", properties.Slides}, {"Words", properties.Words}} {
		value, err := strconv.ParseInt(strings.TrimSpace(field.text), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(field.name) {
		case "pages", "slides":
			c.integer("page_count", "office.core", field.name, value)
		case "words":
			c.integer("office.core.word_count", "office.core", field.name, value)
		}
	}
}

func (c *metadataCollector) extractOfficeCustom(data []byte) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			c.warnMalformedXML("office.custom", "custom.xml")
			return
		}
		start, ok := token.(xml.StartElement)
		if !ok || !strings.EqualFold(start.Name.Local, "property") {
			continue
		}
		name := ""
		for _, attribute := range start.Attr {
			if strings.EqualFold(attribute.Name.Local, "name") {
				name = attribute.Value
			}
		}
		var text boundedSourceMetadataText
		depth := 1
		for depth > 0 {
			inner, innerErr := decoder.Token()
			if innerErr != nil {
				c.warnMalformedXML("office.custom", name)
				return
			}
			switch value := inner.(type) {
			case xml.StartElement:
				depth++
			case xml.EndElement:
				depth--
			case xml.CharData:
				text.write(value)
			}
		}
		if name == "" {
			continue
		}
		if text.overflow {
			c.warn("value_too_large", "office.custom", name, "embedded custom property was omitted")
			continue
		}
		key := "office.custom." + canonicalFieldName(name)
		if document.SourceMetadataCanonicalKeyAllowed(key) {
			c.string(key, "office.custom", name, text.string(), false)
		} else if !validSourceMetadataLabel(name) || len(key) > document.MaxSourceMetadataLabelBytes {
			c.warn("invalid_label", "office.custom", name, "embedded custom property with an invalid label was omitted")
		}
	}
}

func (c *metadataCollector) extractEmail(message *mail.Message) {
	for _, item := range []struct {
		header, key string
		sensitive   bool
	}{{"From", "email.from", false}, {"To", "email.to", false}, {"Cc", "email.cc", false}, {"Bcc", "email.bcc", true}, {"Subject", "email.subject", false}} {
		if value := message.Header.Get(item.header); value != "" {
			c.string(item.key, "email", item.header, value, item.sensitive)
		}
	}
	if date := message.Header.Get("Date"); date != "" {
		c.timestamp("email.sent", "email", "Date", date)
	}
	if values := message.Header["Received"]; len(values) > 0 {
		c.strings("email.received", "email", "Received", values)
	}
	parts := 0
	count, err := countEmailAttachments(message.Header, message.Body, 0, &parts)
	if err != nil {
		c.warn("unparseable_attachments", "email", "Content-Disposition", "MIME attachment structure could not be verified")
	} else if count > 0 {
		c.integer("attachment_count", "email", "Content-Disposition", count)
	}
}

type emailMIMEHeader interface {
	Get(key string) string
}

func countEmailAttachments(
	header emailMIMEHeader, body io.Reader, depth int, parts *int,
) (int64, error) {
	const (
		maxMIMEDepth = 8
		maxMIMEParts = 4096
	)
	if depth > maxMIMEDepth {
		return 0, errors.New("MIME nesting exceeds the supported bound")
	}
	contentType := header.Get("Content-Type")
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		if strings.TrimSpace(contentType) == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("parse MIME content type: %w", err)
	}
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return 0, nil
	}
	boundary := parameters["boundary"]
	if boundary == "" {
		return 0, errors.New("multipart MIME content type has no boundary")
	}
	reader := multipart.NewReader(body, boundary)
	var count int64
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			return count, nil
		}
		if nextErr != nil {
			return 0, fmt.Errorf("read MIME part: %w", nextErr)
		}
		*parts++
		if *parts > maxMIMEParts {
			_ = part.Close()
			return 0, errors.New("MIME part count exceeds the supported bound")
		}
		dispositionValue := part.Header.Get("Content-Disposition")
		if dispositionValue != "" {
			disposition, _, dispositionErr := mime.ParseMediaType(dispositionValue)
			if dispositionErr != nil {
				_ = part.Close()
				return 0, fmt.Errorf("parse MIME content disposition: %w", dispositionErr)
			}
			if strings.EqualFold(disposition, "attachment") {
				count++
			}
		}
		nested, nestedErr := countEmailAttachments(part.Header, part, depth+1, parts)
		closeErr := part.Close()
		if err := errors.Join(nestedErr, closeErr); err != nil {
			return 0, err
		}
		count += nested
	}
}

func (c *metadataCollector) extractCalendar(data []byte) {
	lines := unfoldCalendar(string(data))
	for _, line := range lines {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		parts := strings.Split(name, ";")
		base := strings.ToUpper(parts[0])
		hasNamedTimezone := false
		for _, parameter := range parts[1:] {
			parameterName, timezone, found := strings.Cut(parameter, "=")
			if found && strings.EqualFold(parameterName, "TZID") && timezone != "" {
				hasNamedTimezone = true
				break
			}
		}
		switch base {
		case "SUMMARY":
			c.string("title", "calendar", name, value, false)
		case "DESCRIPTION":
			c.string("description", "calendar", name, value, false)
		case "DTSTART":
			if hasNamedTimezone {
				c.string("calendar.start.raw", "calendar", name, value, false)
				c.warn("unsupported_timezone", "calendar", name, "named calendar timezone prevented timestamp normalization")
			} else {
				c.timestamp("calendar.start", "calendar", name, value)
			}
		case "DTEND":
			if hasNamedTimezone {
				c.string("calendar.end.raw", "calendar", name, value, false)
				c.warn("unsupported_timezone", "calendar", name, "named calendar timezone prevented timestamp normalization")
			} else {
				c.timestamp("calendar.end", "calendar", name, value)
			}
		case "ORGANIZER":
			c.strings("creators", "calendar", name, []string{value})
		}
	}
}

func (c *metadataCollector) extractID3(data []byte) {
	if len(data) < 10 || data[3] < 3 || data[3] > 4 || data[5]&0xc0 != 0 {
		return
	}
	tagSize := synchsafeInt(data[6:10])
	if tagSize > len(data)-10 {
		return
	}
	tagEnd := 10 + tagSize
	for offset := 10; offset+10 <= tagEnd; {
		id := string(data[offset : offset+4])
		if strings.Trim(id, "\x00") == "" {
			return
		}
		if strings.IndexFunc(id, func(value rune) bool {
			return (value < 'A' || value > 'Z') && (value < '0' || value > '9')
		}) >= 0 {
			return
		}
		size := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if data[3] == 4 {
			size = synchsafeInt(data[offset+4 : offset+8])
		}
		unsupportedFlags := data[offset+9]
		offset += 10
		if size < 1 || size > tagEnd-offset {
			return
		}
		payload := data[offset : offset+size]
		offset += size
		if unsupportedFlags != 0 {
			continue
		}
		value, ok := decodeID3Text(data[3], payload)
		if ok {
			c.addID3Frame(id, value)
		}
	}
}

func decodeID3Text(version byte, payload []byte) (string, bool) {
	if len(payload) < 2 {
		return "", false
	}
	var value string
	switch payload[0] {
	case 0:
		runes := make([]rune, len(payload)-1)
		for index, character := range payload[1:] {
			runes[index] = rune(character)
		}
		value = string(runes)
	case 1:
		var ok bool
		value, ok = decodeID3UTF16(payload[1:], true)
		if !ok {
			return "", false
		}
	case 2:
		if version != 4 {
			return "", false
		}
		var ok bool
		value, ok = decodeID3UTF16(payload[1:], false)
		if !ok {
			return "", false
		}
	case 3:
		if version != 4 || !utf8.Valid(payload[1:]) {
			return "", false
		}
		value = string(payload[1:])
	default:
		return "", false
	}
	value = strings.Trim(value, "\x00")
	value = strings.ReplaceAll(value, "\x00", "; ")
	return value, value != ""
}

func decodeID3UTF16(data []byte, withBOM bool) (string, bool) {
	var order binary.ByteOrder = binary.BigEndian
	if withBOM {
		if len(data) < 2 {
			return "", false
		}
		switch {
		case bytes.Equal(data[:2], []byte{0xfe, 0xff}):
		case bytes.Equal(data[:2], []byte{0xff, 0xfe}):
			order = binary.LittleEndian
		default:
			return "", false
		}
		data = data[2:]
	}
	if len(data)%2 != 0 {
		return "", false
	}
	units := make([]uint16, len(data)/2)
	for index := range units {
		units[index] = order.Uint16(data[index*2:])
	}
	for index, unit := range units {
		if unit >= 0xd800 && unit <= 0xdbff {
			if index+1 >= len(units) || units[index+1] < 0xdc00 || units[index+1] > 0xdfff {
				return "", false
			}
		} else if unit >= 0xdc00 && unit <= 0xdfff && (index == 0 || units[index-1] < 0xd800 || units[index-1] > 0xdbff) {
			return "", false
		}
	}
	return string(utf16.Decode(units)), true
}
func synchsafeInt(value []byte) int {
	if len(value) < 4 {
		return 0
	}
	return int(value[0]&0x7f)<<21 | int(value[1]&0x7f)<<14 | int(value[2]&0x7f)<<7 | int(value[3]&0x7f)
}
func (c *metadataCollector) addID3Frame(frame, value string) {
	switch frame {
	case "TIT2":
		c.string("title", "media.id3", "TIT2", value, false)
	case "TPE1":
		c.strings("creators", "media.id3", "TPE1", splitValues(value))
	case "TALB":
		c.string("media.id3.album", "media.id3", "TALB", value, false)
	case "TDRC":
		c.timestamp("created", "media.id3", "TDRC", value)
	}
}

func visualContainerSignature(data []byte) bool {
	return bytes.HasPrefix(data, []byte{0xff, 0xd8}) ||
		bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) ||
		len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" ||
		bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a")) ||
		len(data) >= 12 && string(data[4:8]) == "ftyp"
}

func exifTIFFSignature(data []byte) bool {
	_, _, ok := exifTIFFHeader(data)
	return ok
}

func exifTIFFHeader(data []byte) (binary.ByteOrder, string, bool) {
	if len(data) < 4 {
		return nil, "", false
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return nil, "", false
	}
	switch order.Uint16(data[2:4]) {
	case 42:
		return order, "tiff", true
	case 0x4f52, 0x5352:
		return order, "orf", true
	case 0x0055:
		return order, "rw2", true
	default:
		return nil, "", false
	}
}

func (c *metadataCollector) extractVisual(data []byte) {
	metadata, err := documentmedia.DetectBytes(data, "")
	if err != nil {
		c.warn("unparseable_metadata", "media.container", "header", "visual container metadata is malformed or unsupported")
	} else {
		c.string("media.container.format", "media.container", "Format", string(metadata.Format), false)
		c.string("media.container.kind", "media.container", "Kind", string(metadata.Kind), false)
		c.integer("media.container.width_px", "media.container", "Width", metadata.Width)
		c.integer("media.container.height_px", "media.container", "Height", metadata.Height)
		if metadata.FrameCount > 0 {
			c.integer("media.container.frame_count", "media.container", "FrameCount", int64(metadata.FrameCount))
			c.boolean("media.container.animated", "media.container", "Animated", metadata.Animated)
		}
		if metadata.DurationKnown {
			c.integer("media.container.duration_ms", "media.container", "DurationMS", metadata.DurationMS)
		}
		if metadata.CreatedAt != nil {
			c.timestamp("created", "media.container", "mvhd.CreationTime", metadata.CreatedAt.Format(time.RFC3339))
		}
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xd8}) {
		c.extractJPEGExif(data)
	}
}

func (c *metadataCollector) extractJPEGExif(data []byte) {
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != 0xff {
			offset++
			continue
		}
		marker := data[offset+1]
		offset += 2
		if marker == 0xd9 || marker == 0xda {
			break
		}
		if offset+2 > len(data) {
			break
		}
		length := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if length < 2 || offset+length > len(data) {
			break
		}
		segment := data[offset+2 : offset+length]
		if marker == 0xe1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			c.extractExifTIFF(segment[6:])
		}
		offset += length
	}
	if len(c.record.Fields) == 0 {
		c.warn("unparseable_metadata", "image.exif", "APP1", "image contained no supported metadata values")
	}
}

func (c *metadataCollector) extractTIFFVisual(data []byte) {
	if reader, ok := newExifReader(data); ok {
		c.string("media.container.format", "media.container", "Format", reader.format, false)
		c.string("media.container.kind", "media.container", "Kind", "image", false)
		root := reader.entries(reader.u32(4))
		if width, found := exifUnsigned(reader, root[0x0100]); found && width > 0 {
			c.integer("media.container.width_px", "media.container", "ImageWidth", width)
		}
		if height, found := exifUnsigned(reader, root[0x0101]); found && height > 0 {
			c.integer("media.container.height_px", "media.container", "ImageLength", height)
		}
	}
	c.extractExifTIFF(data)
}

type exifReader struct {
	data   []byte
	order  binary.ByteOrder
	format string
}

type exifEntry struct {
	kind  uint16
	value []byte
}

func newExifReader(data []byte) (exifReader, bool) {
	if len(data) < 8 {
		return exifReader{}, false
	}
	order, format, ok := exifTIFFHeader(data)
	if !ok {
		return exifReader{}, false
	}
	return exifReader{data: data, order: order, format: format}, true
}
func (r exifReader) u16(offset int) (uint16, bool) {
	if offset < 0 || offset+2 > len(r.data) {
		return 0, false
	}
	return r.order.Uint16(r.data[offset:]), true
}
func (r exifReader) u32(offset int) uint32 {
	if offset < 0 || offset+4 > len(r.data) {
		return 0
	}
	return r.order.Uint32(r.data[offset:])
}
func (r exifReader) entries(offset uint32) map[uint16][]byte {
	entries, _ := r.typedEntries(offset)
	result := make(map[uint16][]byte, len(entries))
	for tag, entry := range entries {
		result[tag] = entry.value
	}
	return result
}

func (r exifReader) typedEntries(offset uint32) (map[uint16]exifEntry, bool) {
	result := map[uint16]exifEntry{}
	count, ok := r.u16(int(offset))
	if !ok || count > 1024 {
		return result, false
	}
	for index := range count {
		base := int(offset) + 2 + int(index)*12
		if base < 0 || base > len(r.data)-12 {
			return result, false
		}
		tag := r.order.Uint16(r.data[base:])
		kind := r.order.Uint16(r.data[base+2:])
		items := r.order.Uint32(r.data[base+4:])
		if _, duplicate := result[tag]; duplicate {
			return result, false
		}
		width := map[uint16]uint64{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}[kind]
		result[tag] = exifEntry{kind: kind}
		size := uint64(items) * width
		if width == 0 || size > 1<<20 {
			continue
		}
		start := base + 8
		if size > 4 {
			pointer := r.u32(base + 8)
			start = int(pointer)
		}
		sizeInt := int(size)
		if start < 0 || start > len(r.data) || sizeInt > len(r.data)-start {
			return result, false
		}
		result[tag] = exifEntry{kind: kind, value: r.data[start : start+sizeInt]}
	}
	return result, true
}
func exifASCII(value []byte) string {
	return strings.TrimSpace(strings.TrimRight(string(value), "\x00"))
}

func exifUnsigned(reader exifReader, value []byte) (int64, bool) {
	switch len(value) {
	case 1:
		return int64(value[0]), true
	case 2:
		return int64(reader.order.Uint16(value)), true
	case 4:
		return int64(reader.order.Uint32(value)), true
	default:
		return 0, false
	}
}

func exifRational(reader exifReader, value []byte, signed bool) (float64, bool) {
	if len(value) != 8 {
		return 0, false
	}
	if signed {
		numerator := int64(int32(reader.order.Uint32(value[:4])))   //nolint:gosec // EXIF SRATIONAL stores signed bits in an unsigned read buffer.
		denominator := int64(int32(reader.order.Uint32(value[4:]))) //nolint:gosec // EXIF SRATIONAL stores signed bits in an unsigned read buffer.
		if denominator == 0 {
			return 0, false
		}
		return float64(numerator) / float64(denominator), true
	}
	numerator := reader.order.Uint32(value[:4])
	denominator := reader.order.Uint32(value[4:])
	if denominator == 0 {
		return 0, false
	}
	return float64(numerator) / float64(denominator), true
}

func (c *metadataCollector) extractExifTIFF(data []byte) {
	reader, root, ok := sourceMetadataTIFFRoot(data)
	if !ok {
		c.warn("unparseable_metadata", "image.exif", "TIFF", "EXIF TIFF header is malformed")
		return
	}
	c.extractExifRoot(reader, root)
	isoFound := false
	if raw := root[0x8769]; len(raw) >= 4 {
		offset := reader.order.Uint32(raw)
		isoFound = c.extractExifDetails(reader, reader.entries(offset))
	}
	if !isoFound && reader.format == "rw2" {
		if iso, found := exifUnsigned(reader, root[0x0017]); found && iso > 0 {
			c.integer("image.exif.iso", "image.exif", "ISO", iso)
		}
	}
	typedRoot, rootValid := reader.typedEntries(reader.u32(4))
	if pointer, found := typedRoot[0x8825]; found {
		if !rootValid || pointer.kind != 4 || len(pointer.value) != 4 {
			c.warn("unparseable_metadata", "image.exif", "GPSInfoIFDPointer", "GPS metadata directory is malformed")
			return
		}
		offset := reader.order.Uint32(pointer.value)
		gps, valid := reader.typedEntries(offset)
		if !valid || len(gps) == 0 {
			c.warn("unparseable_metadata", "image.exif", "GPSInfoIFDPointer", "GPS metadata directory is malformed")
			return
		}
		c.exifGPS(reader, gps)
	}
}

func (c *metadataCollector) extractExifRoot(reader exifReader, root map[uint16][]byte) {
	c.string("image.exif.camera_make", "image.exif", "Make", exifASCII(root[0x010f]), false)
	c.string("image.exif.camera_model", "image.exif", "Model", exifASCII(root[0x0110]), false)
	c.string("description", "image.exif", "ImageDescription", exifASCII(root[0x010e]), false)
	if orientation, found := exifUnsigned(reader, root[0x0112]); found && orientation >= 1 && orientation <= 8 {
		c.integer("image.exif.orientation", "image.exif", "Orientation", orientation)
	}
	if artist := exifASCII(root[0x013b]); artist != "" {
		c.strings("creators", "image.exif", "Artist", splitValues(artist))
	}
	if stamp := exifASCII(root[0x0132]); stamp != "" {
		c.timestamp("modified", "image.exif", "DateTime", stamp)
	}
}

func (c *metadataCollector) extractExifDetails(reader exifReader, exif map[uint16][]byte) bool {
	if stamp := exifASCII(exif[0x9003]); stamp != "" {
		c.timestamp("created", "image.exif", "DateTimeOriginal", stamp)
	}
	isoFound := false
	if iso, found := exifUnsigned(reader, exif[0x8827]); found && iso > 0 {
		c.integer("image.exif.iso", "image.exif", "PhotographicSensitivity", iso)
		isoFound = true
	}
	if exposure, found := exifRational(reader, exif[0x829a], false); found && exposure > 0 {
		c.exifNumber("image.exif.exposure_time_seconds", "ExposureTime", exposure)
	}
	if aperture, found := exifRational(reader, exif[0x829d], false); found && aperture > 0 {
		c.exifNumber("image.exif.f_number", "FNumber", aperture)
	}
	if bias, found := exifRational(reader, exif[0x9204], true); found {
		c.exifNumber("image.exif.exposure_bias_ev", "ExposureBiasValue", bias)
	}
	if focalLength, found := exifRational(reader, exif[0x920a], false); found && focalLength > 0 {
		c.exifNumber("image.exif.focal_length_mm", "FocalLength", focalLength)
	}
	c.string("image.exif.lens_make", "image.exif", "LensMake", exifASCII(exif[0xa433]), false)
	c.string("image.exif.lens_model", "image.exif", "LensModel", exifASCII(exif[0xa434]), false)
	if width, found := exifUnsigned(reader, exif[0xa002]); found && width > 0 {
		c.integer("image.exif.pixel_width", "image.exif", "PixelXDimension", width)
	}
	if height, found := exifUnsigned(reader, exif[0xa003]); found && height > 0 {
		c.integer("image.exif.pixel_height", "image.exif", "PixelYDimension", height)
	}
	return isoFound
}
func (c *metadataCollector) exifGPS(reader exifReader, gps map[uint16]exifEntry) {
	hasCoordinates := exifEntriesContainAny(gps, 0x0001, 0x0002, 0x0003, 0x0004)
	if hasCoordinates {
		latitude, latitudeOK := exifGPSCoordinate(reader, gps[0x0001], gps[0x0002], "N", "S", 90)
		longitude, longitudeOK := exifGPSCoordinate(reader, gps[0x0003], gps[0x0004], "E", "W", 180)
		if !latitudeOK || !longitudeOK || latitude == 0 && longitude == 0 {
			c.warn("unparseable_metadata", "image.exif", "GPSLatitude/GPSLongitude", "GPS coordinates are incomplete or invalid")
		} else {
			c.exifGPSCoordinates(latitude, longitude)
		}
	}

	hasTimestamp := exifEntriesContainAny(gps, 0x0007, 0x001d)
	if hasTimestamp {
		stamp, ok := exifGPSTimestamp(reader, gps[0x001d], gps[0x0007])
		if !ok {
			c.warn("unparseable_metadata", "image.exif", "GPSDateStamp/GPSTimeStamp", "GPS timestamp is incomplete or invalid")
		} else {
			c.timestamp("image.exif.gps_timestamp", "image.exif", "GPSDateStamp/GPSTimeStamp", stamp)
		}
	}
}

func exifEntriesContainAny(entries map[uint16]exifEntry, tags ...uint16) bool {
	for _, tag := range tags {
		if _, found := entries[tag]; found {
			return true
		}
	}
	return false
}

func exifGPSCoordinate(reader exifReader, ref, rawValue exifEntry, positiveRef, negativeRef string, maximum float64) (float64, bool) {
	parts, ok := exifRationalTriple(reader, rawValue)
	if !ok || parts[1] >= 60 || parts[2] >= 60 {
		return 0, false
	}
	value := parts[0] + parts[1]/60 + parts[2]/3600
	if value > maximum {
		return 0, false
	}
	if ref.kind != 2 {
		return 0, false
	}
	switch exifStrictASCII(ref.value) {
	case positiveRef:
		return value, true
	case negativeRef:
		return -value, true
	default:
		return 0, false
	}
}

func exifRationalTriple(reader exifReader, entry exifEntry) ([3]float64, bool) {
	if entry.kind != 5 || len(entry.value) != 24 {
		return [3]float64{}, false
	}
	var values [3]float64
	for index := range values {
		denominator := reader.order.Uint32(entry.value[index*8+4:])
		if denominator == 0 {
			return [3]float64{}, false
		}
		values[index] = float64(reader.order.Uint32(entry.value[index*8:])) / float64(denominator)
	}
	return values, true
}

func exifGPSTimestamp(reader exifReader, rawDate, rawTime exifEntry) (string, bool) {
	if rawDate.kind != 2 {
		return "", false
	}
	date, err := time.Parse("2006:01:02", exifStrictASCII(rawDate.value))
	parts, ok := exifRationalTriple(reader, rawTime)
	if err != nil || !ok || math.Trunc(parts[0]) != parts[0] || math.Trunc(parts[1]) != parts[1] ||
		parts[0] >= 24 || parts[1] >= 60 || parts[2] >= 60 {
		return "", false
	}
	seconds := time.Duration(math.Round(parts[2] * float64(time.Second)))
	if seconds >= time.Minute {
		return "", false
	}
	stamp := date.Add(time.Duration(parts[0])*time.Hour + time.Duration(parts[1])*time.Minute + seconds)
	return stamp.UTC().Format(time.RFC3339Nano), true
}

func exifStrictASCII(value []byte) string {
	return strings.TrimRight(string(value), "\x00")
}

func (c *metadataCollector) exifGPSCoordinates(latitude, longitude float64) {
	const (
		latitudeKey     = "image.exif.gps_latitude"
		longitudeKey    = "image.exif.gps_longitude"
		latitudeSource  = "GPSLatitude"
		longitudeSource = "GPSLongitude"
	)
	if c.seen[latitudeKey] || c.seen[longitudeKey] ||
		!c.fieldLabelsAllowed(latitudeKey, "image.exif", latitudeSource) ||
		!c.fieldLabelsAllowed(longitudeKey, "image.exif", longitudeSource) {
		return
	}
	if len(c.record.Fields) > document.MaxSourceMetadataFields-2 {
		c.warn("field_limit", "image.exif", latitudeSource+"/"+longitudeSource, "GPS coordinates were omitted")
		return
	}
	latitudeValue := strconv.FormatFloat(latitude, 'f', 7, 64)
	longitudeValue := strconv.FormatFloat(longitude, 'f', 7, 64)
	if !c.reserveValueBytes(len(latitudeValue)+len(longitudeValue), "image.exif", latitudeSource+"/"+longitudeSource) {
		return
	}
	c.seen[latitudeKey] = true
	c.seen[longitudeKey] = true
	c.record.Fields = append(c.record.Fields,
		document.SourceMetadataFieldV1{Key: latitudeKey, Namespace: "image.exif", SourceField: latitudeSource, Sensitive: true,
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new(latitudeValue)}},
		document.SourceMetadataFieldV1{Key: longitudeKey, Namespace: "image.exif", SourceField: longitudeSource, Sensitive: true,
			Value: document.SourceMetadataValueV1{Kind: document.SourceMetadataString, String: new(longitudeValue)}},
	)
}

func splitValues(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == ',' })
}
func canonicalFieldName(value string) string {
	value = strings.ToLower(value)
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, value), "_")
}
func unfoldCalendar(value string) []string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\n ", "")
	value = strings.ReplaceAll(value, "\n\t", "")
	return strings.Split(value, "\n")
}

func parseSourceTimestamp(raw string) (document.SourceMetadataTimestampV1, bool) {
	value := raw
	if after, ok := strings.CutPrefix(value, "D:"); ok {
		value = after
		value = strings.ReplaceAll(value, "'", "")
		if len(value) >= 14 {
			value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[8:10] + ":" + value[10:12] + ":" + value[12:14] + value[14:]
		}
		if len(value) >= 5 && (value[len(value)-5] == '+' || value[len(value)-5] == '-') {
			value = value[:len(value)-2] + ":" + value[len(value)-2:]
		}
	}
	if len(value) == 8 && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		parsed, err := time.Parse("20060102", value)
		if err != nil {
			return document.SourceMetadataTimestampV1{}, false
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format("2006-01-02"), Precision: document.SourceMetadataPrecisionDate, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	if len(value) == 15 && value[8] == 'T' {
		value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[9:11] + ":" + value[11:13] + ":" + value[13:15]
	}
	if len(value) == 16 && strings.HasSuffix(value, "Z") && value[8] == 'T' {
		value = value[:4] + "-" + value[4:6] + "-" + value[6:8] + "T" + value[9:11] + ":" + value[11:13] + ":" + value[13:]
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
		kind := document.SourceMetadataTimezoneOffset
		off := value[len(value)-6:]
		if strings.HasSuffix(value, "Z") {
			kind = document.SourceMetadataTimezoneUTC
			off = ""
		}
		precision := document.SourceMetadataPrecisionSecond
		if strings.Contains(strings.Split(value, "T")[1], ".") {
			precision = document.SourceMetadataPrecisionFraction
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: value, Offset: off, Precision: precision, Timezone: kind}, true
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05.999999999", value); err == nil {
		precision := document.SourceMetadataPrecisionSecond
		if strings.Contains(value, ".") {
			precision = document.SourceMetadataPrecisionFraction
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format(localTimestampLayout(value)), Precision: precision, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	if parsed, err := mail.ParseDate(value); err == nil {
		_, offset := parsed.Zone()
		normalized := parsed.Format(time.RFC3339)
		off := normalized[len(normalized)-6:]
		kind := document.SourceMetadataTimezoneOffset
		if offset == 0 {
			kind = document.SourceMetadataTimezoneUTC
			off = ""
			normalized = parsed.UTC().Format(time.RFC3339)
		}
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: normalized, Offset: off, Precision: document.SourceMetadataPrecisionSecond, Timezone: kind}, true
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format("2006-01-02"), Precision: document.SourceMetadataPrecisionDate, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	if parsed, err := time.Parse("2006:01:02 15:04:05", value); err == nil {
		return document.SourceMetadataTimestampV1{Raw: raw, Normalized: parsed.Format("2006-01-02T15:04:05"), Precision: document.SourceMetadataPrecisionSecond, Timezone: document.SourceMetadataTimezoneOmitted}, true
	}
	return document.SourceMetadataTimestampV1{}, false
}

func localTimestampLayout(value string) string {
	if strings.Contains(value, ".") {
		return "2006-01-02T15:04:05.999999999"
	}
	return "2006-01-02T15:04:05"
}
