package formatdetect

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json/jsontext"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/mail"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/filter"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

const (
	// MaxDocumentBytes is the absolute detector allocation and read ceiling.
	MaxDocumentBytes         = int64(500 << 20)
	maxSniffBytes            = int64(8 << 20)
	maxTextSniffBytes        = int64(50 << 20)
	maxZIPEntries            = 10_000
	maxZIPCentralDirectory   = uint32(16 << 20)
	maxZIPExpandedBytes      = uint64(500 << 20)
	maxZIPSingleExpandedByte = uint64(100 << 20)
	maxPDFTailBytes          = int64(64 << 10)
	maxPDFXRefBytes          = int64(4 << 10)
	maxPDFTokens             = 1 << 20
	maxPDFPageTreeDepth      = 256
	ooxmlContentTypesName    = "[Content_Types].xml"
)

// CompoundDirectoryNames validates one legacy compound-file directory. It is
// exported only so the Mistral compatibility suite can retain its allocation
// regression test while format detection is shared with core inspection.
func CompoundDirectoryNames(reader io.ReaderAt, size int64) (map[string]bool, error) {
	return compoundDirectoryNames(reader, size)
}

var (
	compoundFileMagic = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}
)

const compoundNoStream = uint32(0xffffffff)

type compoundDirectoryEntry struct {
	name               string
	entryType          byte
	left, right, child uint32
}

// DetectFormat validates a provider candidate from bounded bytes. Declared
// type is a hint only: container families must prove internal markers, while
// inherently ambiguous text formats also require syntactically safe UTF-8.
func DetectFormat(reader io.ReaderAt, size int64, declaredMediaType string) (CandidateFormat, error) {
	if reader == nil || size <= 0 {
		return CandidateFormat{}, errors.New("document format detection requires nonempty bytes")
	}
	if size > MaxDocumentBytes {
		return CandidateFormat{}, errors.New("document exceeds the format-detection byte limit")
	}
	mediaType, parameters, err := mime.ParseMediaType(declaredMediaType)
	if err != nil || len(parameters) != 0 || mediaType != strings.ToLower(mediaType) {
		return CandidateFormat{}, errors.New("document format detection requires a canonical media type")
	}
	prefix, err := readPrefix(reader, size, maxSniffBytes)
	if err != nil {
		return CandidateFormat{}, err
	}

	var detected CandidateFormat
	switch {
	case bytes.HasPrefix(prefix, []byte("%PDF-")):
		if err = validatePDFStructure(reader, size, prefix); err == nil {
			detected, _ = CandidateFormatByID(formatIDPDF)
		}
	case bytes.HasPrefix(prefix, []byte(`{\rtf`)):
		detected, _ = CandidateFormatByID("rtf")
	case bytes.HasPrefix(prefix, compoundFileMagic):
		detected, err = detectCompoundFormat(reader, size)
	case bytes.HasPrefix(prefix, []byte("PK\x03\x04")) || bytes.HasPrefix(prefix, []byte("PK\x05\x06")):
		detected, err = detectZIPFormat(reader, size)
	default:
		if size > maxTextSniffBytes {
			return CandidateFormat{}, errors.New("document text exceeds type-detection limit")
		}
		content, readErr := readPrefix(reader, size, maxTextSniffBytes)
		if readErr != nil {
			return CandidateFormat{}, readErr
		}
		detected, err = detectTextFormat(content, mediaType)
	}
	if err != nil {
		return CandidateFormat{}, err
	}
	if detected.MediaType != mediaType {
		return CandidateFormat{}, fmt.Errorf("document bytes are %s, not declared %s", detected.MediaType, mediaType)
	}
	return detected, nil
}

func validatePDFStructure(reader io.ReaderAt, size int64, prefix []byte) error {
	if len(prefix) < 9 || !validPDFVersion(prefix[5:8]) || !isPDFWhitespace(prefix[8]) {
		return errors.New("PDF header is invalid")
	}
	tailLength := min(size, maxPDFTailBytes)
	tailOffset := size - tailLength
	tail := make([]byte, tailLength)
	read, err := reader.ReadAt(tail, tailOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read PDF trailer: %w", err)
	}
	if int64(read) != tailLength {
		return errors.New("document bytes changed during PDF trailer read")
	}
	eofIndex := bytes.LastIndex(tail, []byte("%%EOF"))
	if eofIndex < 0 || len(trimPDFWhitespace(tail[eofIndex+len("%%EOF"):])) != 0 {
		return errors.New("PDF end marker is missing or not final")
	}
	beforeEOF := tail[:eofIndex]
	startXRefIndex := bytes.LastIndex(beforeEOF, []byte("startxref"))
	if startXRefIndex < 0 {
		return errors.New("PDF startxref is missing")
	}
	offsetText := trimPDFWhitespace(beforeEOF[startXRefIndex+len("startxref"):])
	digitEnd := 0
	for digitEnd < len(offsetText) && offsetText[digitEnd] >= '0' && offsetText[digitEnd] <= '9' {
		digitEnd++
	}
	if digitEnd == 0 || len(trimPDFWhitespace(offsetText[digitEnd:])) != 0 {
		return errors.New("PDF startxref offset is invalid")
	}
	xrefOffset, err := strconv.ParseInt(string(offsetText[:digitEnd]), 10, 64)
	if err != nil || xrefOffset <= 0 || xrefOffset >= tailOffset+int64(eofIndex) {
		return errors.New("PDF startxref offset is outside the document")
	}
	xrefLength := min(size-xrefOffset, maxPDFXRefBytes)
	xref := make([]byte, xrefLength)
	read, err = reader.ReadAt(xref, xrefOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read PDF cross-reference data: %w", err)
	}
	if int64(read) != xrefLength {
		return errors.New("document bytes changed during PDF cross-reference read")
	}
	if validPDFTableXRef(xref, beforeEOF[:startXRefIndex]) || validPDFStreamXRef(xref) {
		return nil
	}
	return errors.New("PDF cross-reference data is invalid")
}

var (
	ErrPDFEncrypted         = errors.New("PDF is encrypted")
	ErrPDFExpandedBytes     = errors.New("PDF expanded bytes exceed the bound")
	ErrPDFEntryBytes        = errors.New("PDF stream bytes exceed the entry bound")
	ErrPDFEntryCount        = errors.New("PDF stream count exceeds the entry bound")
	ErrPDFUnbounded         = errors.New("PDF stream cannot be bounded locally")
	ErrPDFExternalReference = errors.New("PDF contains an external reference")
)

// PDFLimits binds parser allocations and decoded stream measurements to one
// inspection policy.
type PDFLimits struct {
	MaxExpandedBytes int64
	MaxEntryBytes    int64
	MaxEntries       int64
}

// PDFMeasurements contains the finite PDF measurements established while
// resolving the authoritative cross-reference table.
type PDFMeasurements struct {
	Pages           int64
	CompressedBytes int64
	ExpandedBytes   int64
	Entries         int64
	MaxEntryBytes   int64
}

// InspectPDF validates the authoritative PDF object graph and measures every
// supported stream under the supplied limits. Opaque image filters are rejected
// because their decoded size cannot be established without rendering them.
func InspectPDF(data []byte, limits PDFLimits) (PDFMeasurements, error) {
	resourceLimits := model.DefaultResourceLimits()
	resourceLimits.MaxStreamBytes = limits.MaxEntryBytes
	resourceLimits.MaxDecodeBytes = min(limits.MaxEntryBytes, limits.MaxExpandedBytes)
	resourceLimits.MaxImageBytes = limits.MaxExpandedBytes
	resourceLimits.MaxImagePixels = limits.MaxExpandedBytes
	resourceLimits.MaxObjectCount = min(resourceLimits.MaxObjectCount, len(data)+1)
	resourceLimits.MaxXRefEntries = min(resourceLimits.MaxXRefEntries, len(data)+1)
	resourceLimits.MaxObjectStreamCount = min(resourceLimits.MaxObjectStreamCount, len(data)+1)
	resourceLimits.MaxObjectStreamFirst = min(resourceLimits.MaxObjectStreamFirst, limits.MaxEntryBytes)
	resourceLimits.MaxRecursionDepth = maxPDFPageTreeDepth

	configuration := &model.Configuration{
		Reader15:       true,
		ValidationMode: model.ValidationRelaxed,
		Offline:        true,
		Cmd:            model.VALIDATE,
		Limits:         resourceLimits,
	}
	context, err := api.ReadAndValidate(bytes.NewReader(data), configuration)
	if err != nil {
		if errors.Is(err, filter.ErrDecodeLimitExceeded) {
			return PDFMeasurements{}, ErrPDFExpandedBytes
		}
		return PDFMeasurements{}, fmt.Errorf("validate PDF object graph: %w", err)
	}
	if context.Encrypt != nil {
		return PDFMeasurements{}, ErrPDFEncrypted
	}
	external, err := pdfHasExternalReference(context)
	if err != nil {
		if errors.Is(err, filter.ErrDecodeLimitExceeded) {
			return PDFMeasurements{}, ErrPDFExpandedBytes
		}
		return PDFMeasurements{}, fmt.Errorf("inspect PDF object graph: %w", err)
	}
	if external {
		return PDFMeasurements{}, ErrPDFExternalReference
	}

	measurements := PDFMeasurements{Pages: int64(context.PageCount)}
	objectNumbers := make([]int, 0, len(context.Table))
	for number := range context.Table {
		objectNumbers = append(objectNumbers, number)
	}
	slices.Sort(objectNumbers)
	for _, number := range objectNumbers {
		entry := context.Table[number]
		if entry == nil || entry.Free {
			continue
		}
		stream, ok := pdfStreamDictionary(entry.Object)
		if !ok {
			continue
		}
		measurements.Entries++
		if measurements.Entries > limits.MaxEntries {
			return PDFMeasurements{}, ErrPDFEntryCount
		}
		encodedBytes := int64(len(stream.Raw))
		if encodedBytes > limits.MaxEntryBytes {
			return PDFMeasurements{}, ErrPDFEntryBytes
		}
		if encodedBytes > math.MaxInt64-measurements.CompressedBytes {
			return PDFMeasurements{}, ErrPDFEntryBytes
		}
		measurements.CompressedBytes += encodedBytes

		decodedBytes, err := boundedPDFStreamBytes(stream, limits, measurements.ExpandedBytes)
		if err != nil {
			return PDFMeasurements{}, fmt.Errorf("measure PDF stream %d: %w", number, err)
		}
		measurements.ExpandedBytes += decodedBytes
		measurements.MaxEntryBytes = max(measurements.MaxEntryBytes, decodedBytes)
	}
	return measurements, nil
}

func pdfHasExternalReference(context *model.Context) (bool, error) {
	if context.Root == nil {
		return false, errors.New("PDF catalog is missing")
	}
	pending := []types.Object{*context.Root}
	visited := make(map[int]bool, len(context.Table))
	for len(pending) != 0 {
		object := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		switch value := object.(type) {
		case types.IndirectRef:
			number := value.ObjectNumber.Value()
			if visited[number] {
				continue
			}
			visited[number] = true
			resolved, err := context.Dereference(value)
			if err != nil {
				return false, fmt.Errorf("dereference PDF object %d: %w", number, err)
			}
			if resolved != nil {
				pending = append(pending, resolved)
			}
		case types.Dict:
			external, err := pdfDictHasExternalReference(context, value)
			if err != nil || external {
				return external, err
			}
			for _, child := range value {
				pending = append(pending, child)
			}
		case types.Array:
			pending = append(pending, value...)
		case types.StreamDict:
			pending = append(pending, value.Dict)
		case *types.StreamDict:
			pending = append(pending, value.Dict)
		case types.ObjectStreamDict:
			pending = append(pending, value.Dict)
		case *types.ObjectStreamDict:
			pending = append(pending, value.Dict)
		case types.XRefStreamDict:
			pending = append(pending, value.Dict)
		case *types.XRefStreamDict:
			pending = append(pending, value.Dict)
		}
	}
	return false, nil
}

func pdfDictHasExternalReference(context *model.Context, dictionary types.Dict) (bool, error) {
	if dictionary.HasEntry("URI") {
		return true, nil
	}
	action, err := pdfDictName(context, dictionary, "S")
	if err != nil {
		return false, err
	}
	if action == "GoToR" || action == "Launch" {
		return true, nil
	}
	fileSystem, err := pdfDictName(context, dictionary, "FS")
	if err != nil {
		return false, err
	}
	if fileSystem == "URL" {
		return true, nil
	}
	// /Type is optional in a file specification, so the path entries decide.
	// They must be strings: an annotation's /F is an integer flags field, and a
	// stream's /F names external data, which is external in its own right.
	// An /EF entry means the file travels embedded rather than by reference.
	//
	// /DOS, /Mac, and /Unix hold the same path for one platform. A viewer on
	// that platform reads them, and a specification may carry them with no /F
	// at all, so all five keys count.
	if dictionary.HasEntry("EF") {
		return false, nil
	}
	for _, key := range []string{"F", "UF", "DOS", "Mac", "Unix"} {
		isPath, err := pdfDictHasStringEntry(context, dictionary, key)
		if err != nil || isPath {
			return isPath, err
		}
	}
	return false, nil
}

func pdfDictHasStringEntry(
	context *model.Context, dictionary types.Dict, key string,
) (bool, error) {
	object, ok := dictionary[key]
	if !ok {
		return false, nil
	}
	resolved, err := context.Dereference(object)
	if err != nil {
		return false, fmt.Errorf("dereference PDF dictionary %q: %w", key, err)
	}
	switch resolved.(type) {
	case types.StringLiteral, types.HexLiteral:
		return true, nil
	}
	return false, nil
}

func pdfDictName(context *model.Context, dictionary types.Dict, key string) (string, error) {
	object, ok := dictionary[key]
	if !ok {
		return "", nil
	}
	resolved, err := context.Dereference(object)
	if err != nil {
		return "", fmt.Errorf("dereference PDF dictionary %q: %w", key, err)
	}
	name, ok := resolved.(types.Name)
	if !ok {
		return "", nil
	}
	return name.Value(), nil
}

func pdfStreamDictionary(object types.Object) (*types.StreamDict, bool) {
	switch value := object.(type) {
	case types.StreamDict:
		return &value, true
	case *types.StreamDict:
		return value, true
	case types.ObjectStreamDict:
		return &value.StreamDict, true
	case *types.ObjectStreamDict:
		return &value.StreamDict, true
	case types.XRefStreamDict:
		return &value.StreamDict, true
	case *types.XRefStreamDict:
		return &value.StreamDict, true
	default:
		return nil, false
	}
}

func boundedPDFStreamBytes(stream *types.StreamDict, limits PDFLimits, expanded int64) (int64, error) {
	for _, pipeline := range stream.FilterPipeline {
		switch pipeline.Name {
		case filter.DCT, filter.JBIG2, filter.JPX:
			return 0, ErrPDFUnbounded
		}
	}
	remaining := limits.MaxExpandedBytes - expanded
	if remaining <= 0 && len(stream.Raw) != 0 {
		return 0, ErrPDFExpandedBytes
	}
	decodeLimit := min(limits.MaxEntryBytes, max(remaining, int64(1)))
	decoded, err := stream.DecodeLengthWithLimit(-1, decodeLimit)
	if errors.Is(err, filter.ErrDecodeLimitExceeded) {
		if limits.MaxEntryBytes <= remaining {
			return 0, ErrPDFEntryBytes
		}
		return 0, ErrPDFExpandedBytes
	}
	if errors.Is(err, filter.ErrUnsupportedFilter) {
		return 0, ErrPDFUnbounded
	}
	if err != nil {
		return 0, fmt.Errorf("decode PDF stream: %w", err)
	}
	decodedBytes := int64(len(decoded))
	if decodedBytes > limits.MaxEntryBytes {
		return 0, ErrPDFEntryBytes
	}
	if decodedBytes > remaining {
		return 0, ErrPDFExpandedBytes
	}
	return decodedBytes, nil
}

func validPDFVersion(version []byte) bool {
	return len(version) == 3 && version[1] == '.' &&
		((version[0] == '1' && version[2] >= '0' && version[2] <= '7') ||
			(version[0] == '2' && version[2] == '0'))
}

func validPDFTableXRef(xref, beforeStartXRef []byte) bool {
	position := 0
	line, ok := nextPDFLine(xref, &position)
	if !ok || !bytes.Equal(trimPDFWhitespace(line), []byte("xref")) {
		return false
	}
	header, ok := nextNonemptyPDFLine(xref, &position)
	if !ok {
		return false
	}
	headerFields := bytes.Fields(header)
	if len(headerFields) != 2 {
		return false
	}
	first, firstErr := strconv.ParseUint(string(headerFields[0]), 10, 64)
	count, countErr := strconv.ParseUint(string(headerFields[1]), 10, 64)
	if firstErr != nil || countErr != nil || count == 0 || first > math.MaxUint64-count {
		return false
	}
	validatedRecords := uint64(0)
	for validatedRecords < count {
		record, ok := nextPDFLine(xref, &position)
		if !ok {
			break
		}
		if !validPDFXRefRecord(record) {
			if position == len(xref) {
				break
			}
			return false
		}
		validatedRecords++
	}
	if validatedRecords == 0 {
		return false
	}

	return validPDFTrailer(beforeStartXRef, first, count)
}

func validPDFStreamXRef(xref []byte) bool {
	streamIndex := firstPDFKeyword(xref, "stream")
	if streamIndex < 0 {
		return false
	}
	tokens, ok := tokenizePDF(xref[:streamIndex])
	if !ok || len(tokens) < 5 || tokens[2] != "obj" {
		return false
	}
	objectNumber, err := strconv.ParseUint(tokens[0], 10, 64)
	if err != nil || objectNumber == 0 {
		return false
	}
	generation, err := strconv.ParseUint(tokens[1], 10, 64)
	if err != nil || generation > 65_535 {
		return false
	}
	dictionary, next, ok := parsePDFDictionaryTokens(tokens, 3, 0)
	if !ok || next != len(tokens) {
		return false
	}
	_, sizeOK := pdfPositiveInteger(dictionary["Size"])
	return sizeOK && len(dictionary["Type"]) == 1 && dictionary["Type"][0] == "/XRef" &&
		validPDFRootReference(dictionary["Root"]) && validPDFWidths(dictionary["W"]) &&
		validPDFStreamLength(dictionary["Length"])
}

func validPDFTrailer(data []byte, first, count uint64) bool {
	for end := len(data); end > 0; {
		trailerIndex := lastPDFKeyword(data[:end], "trailer")
		if trailerIndex < 0 {
			return false
		}
		trailer, ok := parsePDFDictionary(data[trailerIndex+len("trailer"):])
		if ok {
			if !validPDFRootReference(trailer["Root"]) {
				return false
			}
			size, sizeOK := pdfPositiveInteger(trailer["Size"])
			return sizeOK && first+count <= size
		}
		end = trailerIndex
	}
	return false
}

func nextPDFLine(data []byte, position *int) ([]byte, bool) {
	if *position >= len(data) {
		return nil, false
	}
	start := *position
	for *position < len(data) && data[*position] != '\n' && data[*position] != '\r' {
		*position++
	}
	line := data[start:*position]
	if *position < len(data) && data[*position] == '\r' {
		*position++
	}
	if *position < len(data) && data[*position] == '\n' {
		*position++
	}
	return line, true
}

func nextNonemptyPDFLine(data []byte, position *int) ([]byte, bool) {
	for {
		line, ok := nextPDFLine(data, position)
		if !ok {
			return nil, false
		}
		if line = trimPDFWhitespace(line); len(line) != 0 {
			return line, true
		}
	}
}

func validPDFXRefRecord(line []byte) bool {
	fields := bytes.Fields(line)
	if len(fields) != 3 || len(fields[0]) != 10 || len(fields[1]) != 5 || len(fields[2]) != 1 ||
		(fields[2][0] != 'n' && fields[2][0] != 'f') {
		return false
	}
	return decimalBytes(fields[0]) && decimalBytes(fields[1])
}

func decimalBytes(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func lastPDFKeyword(data []byte, keyword string) int {
	for end := len(data); end > 0; {
		index := bytes.LastIndex(data[:end], []byte(keyword))
		if index < 0 {
			return -1
		}
		beforeOK := index == 0 || isPDFTokenBoundary(data[index-1])
		after := index + len(keyword)
		afterOK := after == len(data) || isPDFTokenBoundary(data[after])
		if beforeOK && afterOK {
			return index
		}
		end = index
	}
	return -1
}

func firstPDFKeyword(data []byte, keyword string) int {
	for start := 0; start < len(data); {
		relative := bytes.Index(data[start:], []byte(keyword))
		if relative < 0 {
			return -1
		}
		index := start + relative
		beforeOK := index == 0 || isPDFTokenBoundary(data[index-1])
		after := index + len(keyword)
		afterOK := after == len(data) || isPDFTokenBoundary(data[after])
		if beforeOK && afterOK {
			return index
		}
		start = index + 1
	}
	return -1
}

func isPDFTokenBoundary(char byte) bool {
	return isPDFWhitespace(char) || strings.ContainsRune("()<>[]{}/%", rune(char))
}

func tokenizePDF(data []byte) ([]string, bool) {
	tokens := make([]string, 0, 32)
	for position := 0; position < len(data); {
		if len(tokens) >= maxPDFTokens {
			return nil, false
		}
		char := data[position]
		if isPDFWhitespace(char) {
			position++
			continue
		}
		if char == '%' {
			for position < len(data) && data[position] != '\r' && data[position] != '\n' {
				position++
			}
			continue
		}
		if position+1 < len(data) && (string(data[position:position+2]) == "<<" ||
			string(data[position:position+2]) == ">>") {
			tokens = append(tokens, string(data[position:position+2]))
			position += 2
			continue
		}
		if char == '(' {
			start, depth, escaped := position, 0, false
			for ; position < len(data); position++ {
				current := data[position]
				if escaped {
					escaped = false
					continue
				}
				if current == '\\' {
					escaped = true
					continue
				}
				if current == '(' {
					depth++
				} else if current == ')' {
					depth--
					if depth == 0 {
						position++
						break
					}
				}
			}
			if depth != 0 {
				return nil, false
			}
			tokens = append(tokens, string(data[start:position]))
			continue
		}
		if char == '<' {
			start := position
			position++
			for position < len(data) && data[position] != '>' {
				position++
			}
			if position == len(data) {
				return nil, false
			}
			position++
			tokens = append(tokens, string(data[start:position]))
			continue
		}
		if strings.ContainsRune("[]{}", rune(char)) {
			tokens = append(tokens, string(char))
			position++
			continue
		}
		start := position
		if char == '/' {
			position++
		}
		for position < len(data) && !isPDFTokenBoundary(data[position]) {
			position++
		}
		if position == start || (position == start+1 && char == '/') {
			return nil, false
		}
		tokens = append(tokens, string(data[start:position]))
	}
	return tokens, true
}

func parsePDFDictionary(data []byte) (map[string][]string, bool) {
	tokens, ok := tokenizePDF(data)
	if !ok {
		return nil, false
	}
	dictionary, next, ok := parsePDFDictionaryTokens(tokens, 0, 0)
	return dictionary, ok && next == len(tokens)
}

func parsePDFDictionaryTokens(
	tokens []string,
	position int,
	depth int,
) (map[string][]string, int, bool) {
	if depth > 32 || position >= len(tokens) || tokens[position] != "<<" {
		return nil, position, false
	}
	position++
	dictionary := make(map[string][]string)
	for position < len(tokens) && tokens[position] != ">>" {
		key := tokens[position]
		if len(key) < 2 || key[0] != '/' {
			return nil, position, false
		}
		key = key[1:]
		if _, exists := dictionary[key]; exists {
			return nil, position, false
		}
		position++
		valueStart := position
		var ok bool
		position, ok = skipPDFObject(tokens, position, depth+1)
		if !ok {
			return nil, position, false
		}
		dictionary[key] = tokens[valueStart:position]
	}
	if position >= len(tokens) || tokens[position] != ">>" {
		return nil, position, false
	}
	return dictionary, position + 1, true
}

func skipPDFObject(tokens []string, position, depth int) (int, bool) {
	if depth > 32 || position >= len(tokens) {
		return position, false
	}
	switch tokens[position] {
	case "<<":
		_, next, ok := parsePDFDictionaryTokens(tokens, position, depth)
		return next, ok
	case "[":
		position++
		for position < len(tokens) && tokens[position] != "]" {
			var ok bool
			position, ok = skipPDFObject(tokens, position, depth+1)
			if !ok {
				return position, false
			}
		}
		return position + 1, position < len(tokens)
	case ">>", "]":
		return position, false
	default:
		if position+2 < len(tokens) && decimalString(tokens[position]) &&
			decimalString(tokens[position+1]) && tokens[position+2] == "R" {
			return position + 3, true
		}
		return position + 1, true
	}
}

func pdfPositiveInteger(value []string) (uint64, bool) {
	if len(value) != 1 || !decimalString(value[0]) {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value[0], 10, 64)
	return parsed, err == nil && parsed > 0
}

func validPDFRootReference(value []string) bool {
	if len(value) != 3 || value[2] != "R" {
		return false
	}
	objectNumber, objectErr := strconv.ParseUint(value[0], 10, 64)
	generation, generationErr := strconv.ParseUint(value[1], 10, 64)
	return objectErr == nil && objectNumber > 0 && generationErr == nil && generation <= 65_535
}

func validPDFWidths(value []string) bool {
	if len(value) != 5 || value[0] != "[" || value[4] != "]" {
		return false
	}
	total := uint64(0)
	for _, width := range value[1:4] {
		parsed, err := strconv.ParseUint(width, 10, 64)
		if err != nil || parsed > 8 {
			return false
		}
		total += parsed
	}
	return total > 0
}

func validPDFStreamLength(value []string) bool {
	if _, ok := pdfPositiveInteger(value); ok {
		return true
	}
	return validPDFRootReference(value)
}

func decimalString(value string) bool {
	return decimalBytes([]byte(value))
}

func trimPDFWhitespace(value []byte) []byte {
	for len(value) > 0 && isPDFWhitespace(value[0]) {
		value = value[1:]
	}
	for len(value) > 0 && isPDFWhitespace(value[len(value)-1]) {
		value = value[:len(value)-1]
	}
	return value
}

func isPDFWhitespace(char byte) bool {
	return char == 0 || char == '\t' || char == '\n' || char == '\f' || char == '\r' || char == ' '
}

func detectCompoundFormat(reader io.ReaderAt, size int64) (CandidateFormat, error) {
	names, err := compoundDirectoryNames(reader, size)
	if err != nil {
		return CandidateFormat{}, err
	}
	ids := make([]string, 0, 2)
	if names["WordDocument"] {
		ids = append(ids, "doc")
	}
	if names["Workbook"] || names["Book"] {
		ids = append(ids, "xls")
	}
	if names["PowerPoint Document"] {
		ids = append(ids, "ppt")
	}
	if names["__properties_version1.0"] {
		ids = append(ids, "msg")
	}
	if len(ids) != 1 {
		return CandidateFormat{}, errors.New("compound document has missing or ambiguous family markers")
	}
	format, _ := CandidateFormatByID(ids[0])
	return format, nil
}

func compoundDirectoryNames(reader io.ReaderAt, size int64) (map[string]bool, error) {
	const (
		freeSector        = uint32(0xffffffff)
		endOfChain        = uint32(0xfffffffe)
		fatSector         = uint32(0xfffffffd)
		difatSector       = uint32(0xfffffffc)
		maxDIFATSectors   = 1_024
		maxDirectoryBytes = int64(8 << 20)
	)
	if size < 512 {
		return nil, errors.New("compound document header is truncated")
	}
	header := make([]byte, 512)
	if _, err := reader.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("read compound document header: %w", err)
	}
	if !bytes.Equal(header[:8], compoundFileMagic) || binary.LittleEndian.Uint16(header[28:30]) != 0xfffe {
		return nil, errors.New("compound document header is invalid")
	}
	sectorShift := binary.LittleEndian.Uint16(header[30:32])
	majorVersion := binary.LittleEndian.Uint16(header[26:28])
	if (majorVersion != 3 || sectorShift != 9) && (majorVersion != 4 || sectorShift != 12) {
		return nil, errors.New("compound document sector size is unsupported")
	}
	sectorSize := int64(1 << sectorShift)
	sectorCount := size/sectorSize - 1
	if sectorCount <= 0 || size%sectorSize != 0 {
		return nil, errors.New("compound document size is invalid")
	}
	fatEntriesPerSector := sectorSize / 4
	maxFATSectors := int((sectorCount + fatEntriesPerSector - 1) / fatEntriesPerSector)
	numFAT := int(binary.LittleEndian.Uint32(header[44:48]))
	firstDirectory := binary.LittleEndian.Uint32(header[48:52])
	firstDIFAT := binary.LittleEndian.Uint32(header[68:72])
	numDIFAT := int(binary.LittleEndian.Uint32(header[72:76]))
	if numFAT <= 0 || numFAT > maxFATSectors || numDIFAT > maxDIFATSectors {
		return nil, errors.New("compound document allocation table exceeds limits")
	}
	fatSectors := make([]uint32, 0, numFAT)
	for offset := 76; offset < 512 && len(fatSectors) < numFAT; offset += 4 {
		sector := binary.LittleEndian.Uint32(header[offset : offset+4])
		if sector != freeSector {
			fatSectors = append(fatSectors, sector)
		}
	}
	seenDIFAT := map[uint32]bool{}
	for i, sector := 0, firstDIFAT; i < numDIFAT; i++ {
		if int64(sector) >= sectorCount || seenDIFAT[sector] {
			return nil, errors.New("compound document DIFAT chain is invalid")
		}
		seenDIFAT[sector] = true
		data, readErr := readCompoundSector(reader, sector, sectorSize, size)
		if readErr != nil {
			return nil, readErr
		}
		for offset := 0; offset < len(data)-4 && len(fatSectors) < numFAT; offset += 4 {
			fatID := binary.LittleEndian.Uint32(data[offset : offset+4])
			if fatID != freeSector {
				fatSectors = append(fatSectors, fatID)
			}
		}
		sector = binary.LittleEndian.Uint32(data[len(data)-4:])
		if i == numDIFAT-1 && sector != endOfChain {
			return nil, errors.New("compound document DIFAT chain does not terminate")
		}
	}
	if len(fatSectors) != numFAT {
		return nil, errors.New("compound document FAT sector count is invalid")
	}
	fat := make([]uint32, 0, int(sectorCount))
	seenFAT := map[uint32]bool{}
	for _, sector := range fatSectors {
		if int64(sector) >= sectorCount || seenFAT[sector] {
			return nil, errors.New("compound document FAT sector list is invalid")
		}
		seenFAT[sector] = true
		data, readErr := readCompoundSector(reader, sector, sectorSize, size)
		if readErr != nil {
			return nil, readErr
		}
		for offset := 0; offset < len(data); offset += 4 {
			fat = append(fat, binary.LittleEndian.Uint32(data[offset:offset+4]))
		}
	}
	if len(fat) < int(sectorCount) {
		return nil, errors.New("compound document FAT is truncated")
	}
	for _, sector := range fatSectors {
		if fat[sector] != fatSector {
			return nil, errors.New("compound document FAT sector is not self-marked")
		}
	}
	for sector := range seenDIFAT {
		if fat[sector] != difatSector {
			return nil, errors.New("compound document DIFAT sector is not self-marked")
		}
	}

	var directory bytes.Buffer
	seenDirectory := map[uint32]bool{}
	for sector := firstDirectory; sector != endOfChain; {
		if int64(sector) >= sectorCount || seenDirectory[sector] || int64(directory.Len())+sectorSize > maxDirectoryBytes {
			return nil, errors.New("compound document directory chain exceeds limits")
		}
		seenDirectory[sector] = true
		data, readErr := readCompoundSector(reader, sector, sectorSize, size)
		if readErr != nil {
			return nil, readErr
		}
		_, _ = directory.Write(data)
		next := fat[sector]
		if next == freeSector || next == fatSector || next == difatSector {
			return nil, errors.New("compound document directory chain is invalid")
		}
		sector = next
	}
	entries := make([]compoundDirectoryEntry, 0, directory.Len()/128)
	rootIndex := -1
	data := directory.Bytes()
	for offset := 0; offset+128 <= len(data); offset += 128 {
		entry := data[offset : offset+128]
		entryType := entry[66]
		if entryType == 0 {
			entries = append(entries, compoundDirectoryEntry{})
			continue
		}
		if entryType != 1 && entryType != 2 && entryType != 5 {
			return nil, errors.New("compound document directory entry type is invalid")
		}
		nameLength := int(binary.LittleEndian.Uint16(entry[64:66]))
		if nameLength < 2 || nameLength > 64 || nameLength%2 != 0 {
			return nil, errors.New("compound document directory name is invalid")
		}
		name, decodeErr := decodeUTF16LE(entry[:nameLength-2])
		if decodeErr != nil || name == "" {
			return nil, errors.New("compound document directory name is invalid")
		}
		entries = append(entries, compoundDirectoryEntry{
			name: name, entryType: entryType,
			left: binary.LittleEndian.Uint32(entry[68:72]), right: binary.LittleEndian.Uint32(entry[72:76]),
			child: binary.LittleEndian.Uint32(entry[76:80]),
		})
		if entryType == 5 {
			if rootIndex != -1 {
				return nil, errors.New("compound document has multiple root entries")
			}
			rootIndex = len(entries) - 1
		}
	}
	if rootIndex < 0 {
		return nil, errors.New("compound document has no root entry")
	}
	names := map[string]bool{}
	seen := map[uint32]bool{}
	stack := []uint32{entries[rootIndex].child}
	for len(stack) > 0 {
		index := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if index == compoundNoStream {
			continue
		}
		if uint64(index) >= uint64(len(entries)) || seen[index] || entries[index].entryType == 0 || entries[index].entryType == 5 {
			return nil, errors.New("compound document root directory tree is invalid")
		}
		seen[index] = true
		entry := entries[index]
		if entry.entryType == 2 {
			names[entry.name] = true
		}
		stack = append(stack, entry.left, entry.right)
	}
	return names, nil
}

func readCompoundSector(reader io.ReaderAt, sector uint32, sectorSize, size int64) ([]byte, error) {
	offset := (int64(sector) + 1) * sectorSize
	if offset < sectorSize || offset > size-sectorSize {
		return nil, errors.New("compound document sector is out of range")
	}
	data := make([]byte, sectorSize)
	if _, err := reader.ReadAt(data, offset); err != nil {
		return nil, fmt.Errorf("read compound document sector: %w", err)
	}
	return data, nil
}

func decodeUTF16LE(data []byte) (string, error) {
	if len(data)%2 != 0 {
		return "", errors.New("odd UTF-16 length")
	}
	runes := make([]rune, 0, len(data)/2)
	for i := 0; i < len(data); i += 2 {
		value := binary.LittleEndian.Uint16(data[i : i+2])
		if value == 0 || value >= 0xd800 && value <= 0xdfff {
			return "", errors.New("unsupported UTF-16 directory name")
		}
		runes = append(runes, rune(value))
	}
	return string(runes), nil
}

func detectZIPFormat(reader io.ReaderAt, size int64) (CandidateFormat, error) {
	if err := validateZIPEndRecord(reader, size); err != nil {
		return CandidateFormat{}, err
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return CandidateFormat{}, fmt.Errorf("open document ZIP container: %w", err)
	}
	if len(archive.File) > maxZIPEntries {
		return CandidateFormat{}, errors.New("document ZIP container has too many entries")
	}
	names := make(map[string]bool, len(archive.File))
	var expanded uint64
	var mimeValue string
	var contentTypes []byte
	for _, entry := range archive.File {
		if err := validateZIPName(entry.Name); err != nil {
			return CandidateFormat{}, err
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return CandidateFormat{}, errors.New("document ZIP container contains a symlink")
		}
		if entry.Flags&1 != 0 || (entry.Method != zip.Store && entry.Method != zip.Deflate) {
			return CandidateFormat{}, errors.New("document ZIP container uses unsupported encryption or compression")
		}
		if entry.UncompressedSize64 > maxZIPSingleExpandedByte || expanded > maxZIPExpandedBytes-entry.UncompressedSize64 {
			return CandidateFormat{}, errors.New("document ZIP container exceeds expanded-byte limits")
		}
		expanded += entry.UncompressedSize64
		if names[entry.Name] {
			return CandidateFormat{}, errors.New("document ZIP container has duplicate entry names")
		}
		names[entry.Name] = true
		if err := verifyZIPEntry(entry); err != nil {
			return CandidateFormat{}, err
		}
		if entry.Name == "mimetype" {
			value, readErr := readZIPEntry(entry, 256)
			if readErr != nil {
				return CandidateFormat{}, readErr
			}
			mimeValue = string(value)
		}
		if entry.Name == ooxmlContentTypesName {
			value, readErr := readZIPEntry(entry, 2<<20)
			if readErr != nil {
				return CandidateFormat{}, readErr
			}
			contentTypes = value
		}
	}

	var id string
	switch {
	case names["word/document.xml"] && hasOOXMLMainType(contentTypes, "/word/document.xml",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"):
		id = "docx"
	case names["ppt/presentation.xml"] && hasOOXMLMainType(contentTypes, "/ppt/presentation.xml",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"):
		id = "pptx"
	case names["xl/workbook.xml"] && hasOOXMLMainType(contentTypes, "/xl/workbook.xml",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"):
		id = "xlsx"
	case mimeValue == "application/vnd.oasis.opendocument.text" && names["META-INF/manifest.xml"]:
		id = "odt"
	case mimeValue == "application/vnd.oasis.opendocument.spreadsheet" && names["META-INF/manifest.xml"]:
		id = "ods"
	case mimeValue == "application/epub+zip" && names["META-INF/container.xml"]:
		id = "epub"
	case hasNumbersMarker(names):
		id = "numbers"
	default:
		return CandidateFormat{}, errors.New("zip container is not a supported document format")
	}
	format, _ := CandidateFormatByID(id)
	return format, nil
}

func hasOOXMLMainType(content []byte, partName, contentType string) bool {
	if len(content) == 0 || !validXMLDocument(content) {
		return false
	}
	var document struct {
		XMLName   xml.Name `xml:"Types"`
		Overrides []struct {
			PartName    string `xml:"PartName,attr"`
			ContentType string `xml:"ContentType,attr"`
		} `xml:"Override"`
	}
	if err := xml.Unmarshal(content, &document); err != nil ||
		document.XMLName.Space != "http://schemas.openxmlformats.org/package/2006/content-types" {
		return false
	}
	found := false
	for _, override := range document.Overrides {
		if override.PartName != partName {
			continue
		}
		if found || override.ContentType != contentType {
			return false
		}
		found = true
	}
	return found
}

func validateZIPEndRecord(reader io.ReaderAt, size int64) error {
	const maxTail = int64(65_557)
	tailSize := min(size, maxTail)
	tail := make([]byte, tailSize)
	if _, err := reader.ReadAt(tail, size-tailSize); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read document ZIP end record: %w", err)
	}
	signature := []byte{'P', 'K', 0x05, 0x06}
	offset := bytes.LastIndex(tail, signature)
	if offset < 0 || len(tail)-offset < 22 {
		return errors.New("document ZIP container has no bounded end record")
	}
	record := tail[offset:]
	entries := binary.LittleEndian.Uint16(record[10:12])
	entriesOnDisk := binary.LittleEndian.Uint16(record[8:10])
	centralSize := binary.LittleEndian.Uint32(record[12:16])
	centralOffset := binary.LittleEndian.Uint32(record[16:20])
	commentSize := int(binary.LittleEndian.Uint16(record[20:22]))
	if binary.LittleEndian.Uint16(record[4:6]) != 0 || binary.LittleEndian.Uint16(record[6:8]) != 0 || entriesOnDisk != entries {
		return errors.New("multi-disk document ZIP containers are unsupported")
	}
	if entries == 0xffff || centralSize == 0xffffffff || centralOffset == 0xffffffff ||
		int(entries) > maxZIPEntries || centralSize > maxZIPCentralDirectory {
		return errors.New("document ZIP central directory exceeds limits")
	}
	if int64(centralOffset)+int64(centralSize) > size {
		return errors.New("document ZIP central directory is out of range")
	}
	if len(record) != 22+commentSize {
		return errors.New("document ZIP end record has invalid comment length")
	}
	return nil
}

func validateZIPName(name string) error {
	if name == "" || strings.ContainsRune(name, 0) || strings.ContainsAny(name, "\\:") || strings.HasPrefix(name, "/") {
		return errors.New("document ZIP container has an unsafe entry name")
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return errors.New("document ZIP container has a traversing entry name")
	}
	return nil
}

func readZIPEntry(entry *zip.File, limit int64) ([]byte, error) {
	if limit < 0 || entry.UncompressedSize64 > maxZIPSingleExpandedByte || int64(entry.UncompressedSize64) > limit {
		return nil, errors.New("document ZIP marker entry exceeds limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open document ZIP marker: %w", err)
	}
	defer func() { _ = reader.Close() }()
	value, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read document ZIP marker: %w", err)
	}
	if int64(len(value)) > limit {
		return nil, errors.New("document ZIP marker entry exceeds limit")
	}
	return value, nil
}

func verifyZIPEntry(entry *zip.File) error {
	if entry.UncompressedSize64 > maxZIPSingleExpandedByte {
		return errors.New("document ZIP entry exceeds verification limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return fmt.Errorf("open document ZIP entry: %w", err)
	}
	expectedSize := int64(entry.UncompressedSize64)
	written, readErr := io.Copy(io.Discard, io.LimitReader(reader, expectedSize+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || written != expectedSize {
		return errors.New("document ZIP entry failed bounded verification")
	}
	return nil
}

func hasNumbersMarker(names map[string]bool) bool {
	for name := range names {
		if strings.HasPrefix(name, "Index/Tables/") && strings.HasSuffix(name, ".iwa") {
			return true
		}
	}
	return false
}

func detectTextFormat(content []byte, mediaType string) (CandidateFormat, error) {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return CandidateFormat{}, errors.New("document text is not safe UTF-8")
	}
	candidate, ok := candidateByMediaType(mediaType)
	if !ok || !isTextCandidate(candidate) {
		return CandidateFormat{}, errors.New("document bytes have no supported signature")
	}
	trimmed := bytes.TrimSpace(content)
	switch candidate.ID {
	case "json":
		if len(trimmed) == 0 || !jsontext.Value(trimmed).IsValid() {
			return CandidateFormat{}, errors.New("declared JSON document is invalid")
		}
	case "jsonl":
		for line := range bytes.SplitSeq(trimmed, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) > 0 && !jsontext.Value(bytes.TrimSpace(line)).IsValid() {
				return CandidateFormat{}, errors.New("declared JSONL document is invalid")
			}
		}
	case "xml":
		if !validXMLDocument(content) {
			return CandidateFormat{}, errors.New("declared XML document is invalid")
		}
	case "csv":
		csvReader := csv.NewReader(bytes.NewReader(content))
		csvReader.FieldsPerRecord = -1
		csvReader.ReuseRecord = true
		records := 0
		for {
			_, readErr := csvReader.Read()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return CandidateFormat{}, errors.New("declared CSV document is invalid")
			}
			records++
		}
		if records == 0 {
			return CandidateFormat{}, errors.New("declared CSV document is invalid")
		}
	case "latex":
		if !bytes.Contains(content, []byte(`\documentclass`)) && !bytes.Contains(content, []byte(`\begin{document}`)) {
			return CandidateFormat{}, errors.New("declared LaTeX document has no document marker")
		}
	case "eml":
		message, err := mail.ReadMessage(bytes.NewReader(content))
		if err != nil || message.Header.Get("From") == "" || message.Header.Get("Date") == "" {
			return CandidateFormat{}, errors.New("declared EML document lacks required message headers")
		}
	case "yaml":
		if len(trimmed) == 0 || (!bytes.HasPrefix(trimmed, []byte("---")) && !bytes.Contains(trimmed, []byte(": "))) {
			return CandidateFormat{}, errors.New("declared YAML document has no structural marker")
		}
	}
	return candidate, nil
}

func validXMLDocument(content []byte) bool {
	decoder := xml.NewDecoder(bytes.NewReader(content))
	depth := 0
	roots := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return roots == 1 && depth == 0
		}
		if err != nil {
			return false
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return false
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 {
				return false
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(value)) != 0 {
				return false
			}
		}
	}
}

func isTextCandidate(candidate CandidateFormat) bool {
	switch candidate.Family {
	case "text", "structured", "source", "mail", "spreadsheet":
		return candidate.ID != "msg" && candidate.ID != "xls" && candidate.ID != "xlsx" && candidate.ID != "ods" && candidate.ID != "numbers"
	default:
		return false
	}
}

func candidateByMediaType(mediaType string) (CandidateFormat, bool) {
	for _, candidate := range candidateFormats {
		if candidate.MediaType == mediaType {
			return candidate, true
		}
	}
	return CandidateFormat{}, false
}

func readPrefix(reader io.ReaderAt, size, limit int64) ([]byte, error) {
	length := min(size, limit)
	buffer := make([]byte, length)
	read, err := reader.ReadAt(buffer, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read document signature: %w", err)
	}
	if int64(read) != length {
		return nil, errors.New("document bytes changed during signature read")
	}
	return buffer, nil
}
