package media

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

const (
	capabilityRecordVersion  = 1
	maxInspectionSourceBytes = int64(1 << 30)
	maxID3v2TagBytes         = 1 << 20
)

// CapabilityReason is a stable capability inspection outcome.
type CapabilityReason string

const (
	CapabilityReasonEligible           CapabilityReason = "eligible"
	CapabilityReasonUnsupported        CapabilityReason = "unsupported_media_type"
	CapabilityReasonUnboundedFamily    CapabilityReason = "unbounded_media_family"
	CapabilityReasonMalformed          CapabilityReason = "malformed_input"
	CapabilityReasonSourceBytes        CapabilityReason = "source_bytes_exceeded"
	CapabilityReasonExpandedBytes      CapabilityReason = "expanded_bytes_exceeded"
	CapabilityReasonEntryBytes         CapabilityReason = "entry_bytes_exceeded"
	CapabilityReasonEntryCount         CapabilityReason = "entry_count_exceeded"
	CapabilityReasonNestedContainer    CapabilityReason = "nested_container"
	CapabilityReasonEncryptedContainer CapabilityReason = "encrypted_container"
	CapabilityReasonExternalReference  CapabilityReason = "external_reference"
	CapabilityReasonSemanticUnits      CapabilityReason = "semantic_units_exceeded"
	CapabilityReasonVisualBounds       CapabilityReason = "visual_bounds_exceeded"
)

// InspectionPolicy binds finite preflight limits and downstream authority to
// one exact source. Every supported family must have an enforceable bound.
type InspectionPolicy struct {
	Filename          string `json:"filename"`
	DeclaredMediaType string `json:"declared_media_type"`
	ExpectedBytes     int64  `json:"expected_bytes"`
	ExpectedSHA256    string `json:"expected_sha256"`

	DescriptorFingerprint string                      `json:"descriptor_fingerprint"`
	ProfileFingerprint    string                      `json:"profile_fingerprint"`
	DisclosureFingerprint string                      `json:"disclosure_fingerprint"`
	InputKind             document.RenditionInputKind `json:"input_kind"`

	MaxSourceBytes   int64 `json:"max_source_bytes"`
	MaxExpandedBytes int64 `json:"max_expanded_bytes"`
	MaxEntryBytes    int64 `json:"max_entry_bytes"`
	MaxEntries       int64 `json:"max_entries"`
	MaxNestingDepth  int64 `json:"max_nesting_depth"`
	MaxTextLines     int64 `json:"max_text_lines"`
	MaxCharacters    int64 `json:"max_characters"`
	MaxRecords       int64 `json:"max_records"`
	MaxPages         int64 `json:"max_pages"`
	MaxSlides        int64 `json:"max_slides"`
	MaxSheets        int64 `json:"max_sheets"`
	MaxCells         int64 `json:"max_cells"`
	MaxSpineItems    int64 `json:"max_spine_items"`
	MaxResources     int64 `json:"max_resources"`
	MaxPixels        int64 `json:"max_pixels"`
	MaxFrames        int64 `json:"max_frames"`
	MaxDurationMS    int64 `json:"max_duration_ms"`
}

// CapabilityMeasurements records only finite, locally verified measurements.
type CapabilityMeasurements struct {
	CompressedBytes int64 `json:"compressed_bytes"`
	ExpandedBytes   int64 `json:"expanded_bytes"`
	Entries         int64 `json:"entries"`
	MaxEntryBytes   int64 `json:"max_entry_bytes"`
	NestingDepth    int64 `json:"nesting_depth"`
	TextLines       int64 `json:"text_lines"`
	Characters      int64 `json:"characters"`
	Records         int64 `json:"records"`
	Pages           int64 `json:"pages"`
	Slides          int64 `json:"slides"`
	Sheets          int64 `json:"sheets"`
	Cells           int64 `json:"cells"`
	SpineItems      int64 `json:"spine_items"`
	Resources       int64 `json:"resources"`
	Pixels          int64 `json:"pixels"`
	Frames          int64 `json:"frames"`
	DurationMS      int64 `json:"duration_ms"`
}

// CapabilityRecord is a checksum-sealed decision over exact source bytes.
// Mutating any exported field invalidates Checksum.
type CapabilityRecord struct {
	Version      int                    `json:"version"`
	Eligible     bool                   `json:"eligible"`
	Reason       CapabilityReason       `json:"reason"`
	MediaFamily  string                 `json:"media_family"`
	MediaType    string                 `json:"media_type"`
	Format       string                 `json:"format"`
	SourceBytes  int64                  `json:"source_bytes"`
	SourceSHA256 string                 `json:"source_sha256"`
	Measurements CapabilityMeasurements `json:"measurements"`

	PolicyFingerprint     string                      `json:"policy_fingerprint"`
	DescriptorFingerprint string                      `json:"descriptor_fingerprint"`
	ProfileFingerprint    string                      `json:"profile_fingerprint"`
	DisclosureFingerprint string                      `json:"disclosure_fingerprint"`
	InputKind             document.RenditionInputKind `json:"input_kind"`
	Policy                InspectionPolicy            `json:"policy"`
	Checksum              string                      `json:"checksum"`

	localAuthority bool
}

type capabilityRecordIdentity CapabilityRecord

// InspectCapability reads one bounded source, verifies its declared identity, and
// returns a checksum-sealed finite capability decision.
func InspectCapability(reader io.Reader, policy InspectionPolicy) (CapabilityRecord, error) {
	if reader == nil {
		return CapabilityRecord{}, errors.New("media: inspection source is required")
	}
	if err := validateInspectionPolicy(policy); err != nil {
		return CapabilityRecord{}, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, policy.MaxSourceBytes+1))
	if err != nil {
		return CapabilityRecord{}, fmt.Errorf("media: read inspection source: %w", err)
	}
	if int64(len(data)) > policy.MaxSourceBytes {
		return sealCapabilityRecord(policy, data, CapabilityRecord{
			Eligible: false, Reason: CapabilityReasonSourceBytes,
		})
	}
	digest := sha256.Sum256(data)
	digestHex := hex.EncodeToString(digest[:])
	if int64(len(data)) != policy.ExpectedBytes {
		return CapabilityRecord{}, fmt.Errorf("media: source byte length %d does not match declared %d",
			len(data), policy.ExpectedBytes)
	}
	if digestHex != policy.ExpectedSHA256 {
		return CapabilityRecord{}, errors.New("media: source SHA-256 does not match declaration")
	}

	record := CapabilityRecord{Eligible: false, Reason: CapabilityReasonUnsupported}
	baseType, _, _ := mime.ParseMediaType(policy.DeclaredMediaType)
	ext := strings.ToLower(filepath.Ext(policy.Filename))
	switch {
	case isTextFamily(ext, baseType):
		record = inspectText(data, ext, baseType, policy)
	case isZIPFamily(ext, baseType, data):
		record = inspectZIP(data, ext, baseType, policy)
	case strings.HasPrefix(baseType, "image/") || strings.HasPrefix(baseType, "video/"):
		record = inspectVisualCapability(data, baseType, policy)
	case baseType == "application/pdf" || ext == ".pdf":
		record = inspectPDF(data, policy)
	case baseType == "audio/wav" || baseType == "audio/x-wav" || ext == ".wav":
		record = inspectWAV(data, policy)
	case baseType == "audio/mpeg" || ext == ".mp3":
		record = inspectMP3(data, policy)
	case strings.HasPrefix(baseType, "audio/"):
		record = CapabilityRecord{Eligible: false, Reason: CapabilityReasonUnboundedFamily,
			MediaFamily: "audio", MediaType: baseType, Format: strings.TrimPrefix(ext, ".")}
	case baseType == "application/msword" || baseType == "application/vnd.ms-powerpoint" ||
		baseType == "application/vnd.ms-excel":
		record = CapabilityRecord{Eligible: false, Reason: CapabilityReasonUnboundedFamily,
			MediaFamily: "document", MediaType: baseType, Format: strings.TrimPrefix(ext, ".")}
	}
	if record.Eligible && requiresDocumentFormatDetection(record.MediaFamily) {
		detected, detectErr := formatdetect.DetectFormat(
			bytes.NewReader(data), int64(len(data)), baseType)
		if detectErr != nil || !filenameAllowsFormat(ext, detected.ID) {
			record.Eligible = false
			record.Reason = CapabilityReasonMalformed
		} else {
			record.MediaFamily = detected.Family
			record.MediaType = detected.MediaType
			record.Format = detected.ID
		}
	}
	if record.Eligible && (record.MediaFamily == string(KindImage) ||
		record.MediaFamily == string(KindVideo)) {
		quickTimeIdentity := record.MediaFamily == string(KindVideo) &&
			baseType == "video/quicktime" && ext == ".mov"
		if !quickTimeIdentity && (baseType != record.MediaType || !filenameAllowsVisualFormat(ext, record.Format)) {
			record.Eligible = false
			record.Reason = CapabilityReasonMalformed
		}
	}
	if record.Eligible && record.MediaFamily == "audio" {
		validIdentity := record.Format == "wav" && ext == ".wav" &&
			(baseType == "audio/wav" || baseType == "audio/x-wav") ||
			record.Format == "mp3" && ext == ".mp3" && baseType == "audio/mpeg"
		if !validIdentity {
			record.Eligible = false
			record.Reason = CapabilityReasonMalformed
		}
	}
	return sealCapabilityRecord(policy, data, record)
}

// ValidateCapabilityRecord verifies canonical authority fields and checksum.
func ValidateCapabilityRecord(record CapabilityRecord) error {
	if record.Version != capabilityRecordVersion {
		return errors.New("media: capability record version is invalid")
	}
	for subject, value := range map[string]string{
		"source SHA-256": record.SourceSHA256, "policy fingerprint": record.PolicyFingerprint,
		"descriptor fingerprint": record.DescriptorFingerprint,
		"profile fingerprint":    record.ProfileFingerprint,
		"disclosure fingerprint": record.DisclosureFingerprint, "checksum": record.Checksum,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("media: capability record %s is invalid", subject)
		}
	}
	if record.SourceBytes <= 0 {
		return errors.New("media: capability record source bytes are invalid")
	}
	if err := validateInspectionPolicy(record.Policy); err != nil {
		return fmt.Errorf("media: capability record policy is invalid: %w", err)
	}
	policyEncoded, err := json.Marshal(record.Policy, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("media: encode capability record policy: %w", err)
	}
	if sha256Hex(policyEncoded) != record.PolicyFingerprint {
		return errors.New("media: capability record policy fingerprint does not match policy")
	}
	if record.DescriptorFingerprint != record.Policy.DescriptorFingerprint ||
		record.ProfileFingerprint != record.Policy.ProfileFingerprint ||
		record.DisclosureFingerprint != record.Policy.DisclosureFingerprint ||
		record.InputKind != record.Policy.InputKind {
		return errors.New("media: capability record authority does not match policy")
	}
	identity := record
	identity.Checksum = ""
	identity.localAuthority = false
	encoded, err := json.Marshal(capabilityRecordIdentity(identity), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("media: encode capability record: %w", err)
	}
	if sha256Hex(encoded) != record.Checksum {
		return errors.New("media: capability record checksum is invalid")
	}
	if record.Eligible != (record.Reason == CapabilityReasonEligible) {
		return errors.New("media: capability eligibility and reason disagree")
	}
	return nil
}

// InspectionPolicy returns the policy sealed into a locally produced record.
// Records decoded from an external representation deliberately lack this
// authority and cannot authorize an upload.
func (record CapabilityRecord) InspectionPolicy() (InspectionPolicy, bool) {
	return record.Policy, record.localAuthority
}

func sealCapabilityRecord(
	policy InspectionPolicy, data []byte, record CapabilityRecord,
) (CapabilityRecord, error) {
	digest := sha256.Sum256(data)
	policyEncoded, err := json.Marshal(policy, json.Deterministic(true))
	if err != nil {
		return CapabilityRecord{}, fmt.Errorf("media: encode inspection policy: %w", err)
	}
	record.Version = capabilityRecordVersion
	record.SourceBytes = int64(len(data))
	record.SourceSHA256 = hex.EncodeToString(digest[:])
	record.PolicyFingerprint = sha256Hex(policyEncoded)
	record.DescriptorFingerprint = policy.DescriptorFingerprint
	record.ProfileFingerprint = policy.ProfileFingerprint
	record.DisclosureFingerprint = policy.DisclosureFingerprint
	record.InputKind = policy.InputKind
	record.Policy = policy
	record.Checksum = ""
	record.localAuthority = false
	encoded, err := json.Marshal(capabilityRecordIdentity(record), json.Deterministic(true))
	if err != nil {
		return CapabilityRecord{}, fmt.Errorf("media: encode capability record: %w", err)
	}
	record.Checksum = sha256Hex(encoded)
	record.localAuthority = true
	return record, nil
}

func validateInspectionPolicy(policy InspectionPolicy) error {
	if policy.Filename == "" || policy.Filename == "." || policy.Filename == ".." ||
		strings.ContainsAny(policy.Filename, "/\\\x00") ||
		filepath.Base(policy.Filename) != policy.Filename {
		return errors.New("media: inspection filename must be a base name")
	}
	baseType, _, err := mime.ParseMediaType(policy.DeclaredMediaType)
	if err != nil || baseType == "" {
		return errors.New("media: declared media type is invalid")
	}
	if policy.ExpectedBytes <= 0 || policy.ExpectedBytes > maxInspectionSourceBytes ||
		policy.MaxSourceBytes <= 0 || policy.MaxSourceBytes > maxInspectionSourceBytes ||
		policy.ExpectedBytes > policy.MaxSourceBytes {
		return errors.New("media: source byte bounds are invalid")
	}
	for subject, value := range map[string]string{
		"expected SHA-256":       policy.ExpectedSHA256,
		"descriptor fingerprint": policy.DescriptorFingerprint,
		"profile fingerprint":    policy.ProfileFingerprint,
		"disclosure fingerprint": policy.DisclosureFingerprint,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("media: %s is invalid", subject)
		}
	}
	if policy.InputKind != document.RenditionInputOriginalFile &&
		policy.InputKind != document.RenditionInputDerivedUpload {
		return errors.New("media: inspection input kind is invalid")
	}
	limits := []int64{policy.MaxExpandedBytes, policy.MaxEntryBytes, policy.MaxEntries,
		policy.MaxNestingDepth, policy.MaxTextLines, policy.MaxCharacters, policy.MaxPages, policy.MaxSlides,
		policy.MaxSheets, policy.MaxCells, policy.MaxSpineItems, policy.MaxResources}
	for _, limit := range limits {
		if limit <= 0 {
			return errors.New("media: every finite inspection limit must be positive")
		}
	}
	if policy.MaxNestingDepth != 1 {
		return errors.New("media: the registered container inspector requires max nesting depth one")
	}
	if policy.MaxDurationMS < 0 || policy.MaxFrames < 0 || policy.MaxPixels < 0 || policy.MaxRecords < 0 {
		return errors.New("media: optional inspection limits must not be negative")
	}
	return nil
}

func inspectText(data []byte, ext, mediaType string, policy InspectionPolicy) CapabilityRecord {
	record := CapabilityRecord{MediaFamily: "text", MediaType: mediaType,
		Format: strings.TrimPrefix(ext, ".")}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	record.Measurements.Characters = int64(utf8.RuneCount(data))
	record.Measurements.TextLines = int64(bytes.Count(data, []byte{'\n'}))
	if len(data) != 0 && data[len(data)-1] != '\n' {
		record.Measurements.TextLines++
	}
	switch mediaType {
	case "text/csv":
		csvReader := csv.NewReader(bytes.NewReader(data))
		csvReader.FieldsPerRecord = -1
		for {
			_, err := csvReader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				record.Reason = CapabilityReasonMalformed
				return record
			}
			record.Measurements.Records++
		}
	case "application/x-ndjson", "application/jsonl":
		for line := range bytes.SplitSeq(data, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) != 0 {
				record.Measurements.Records++
			}
		}
	case "application/json", "application/xml", "message/rfc822":
		record.Measurements.Records = 1
	}
	if mediaType == "application/xml" {
		external, err := inspectXML(data, &record.Measurements, xmlMeasureNone)
		if err != nil {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		if external {
			record.Reason = CapabilityReasonExternalReference
			return record
		}
	}
	if record.Measurements.TextLines > policy.MaxTextLines ||
		record.Measurements.Characters > policy.MaxCharacters ||
		(policy.MaxRecords > 0 && record.Measurements.Records > policy.MaxRecords) {
		record.Reason = CapabilityReasonSemanticUnits
		return record
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record
}

func inspectZIP(data []byte, ext, mediaType string, policy InspectionPolicy) CapabilityRecord {
	record := CapabilityRecord{MediaType: mediaType, Format: strings.TrimPrefix(ext, ".")}
	switch ext {
	case ".pptx", ".pptm", ".odp":
		record.MediaFamily = "presentation"
	case ".xlsx", ".xlsm", ".ods":
		record.MediaFamily = "spreadsheet"
	case ".epub":
		record.MediaFamily = "ebook"
	default:
		record.Reason = CapabilityReasonUnboundedFamily
		return record
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	record.Measurements.CompressedBytes = int64(len(data))
	record.Measurements.Entries = int64(len(zr.File))
	if record.Measurements.Entries > policy.MaxEntries {
		record.Reason = CapabilityReasonEntryCount
		return record
	}
	for _, file := range zr.File {
		if file.Flags&1 != 0 {
			record.Reason = CapabilityReasonEncryptedContainer
			return record
		}
		if file.UncompressedSize64 > math.MaxInt64 {
			record.Reason = CapabilityReasonEntryBytes
			return record
		}
		size := int64(file.UncompressedSize64) // #nosec G115 -- bounded above by MaxInt64
		if size > policy.MaxEntryBytes {
			record.Reason = CapabilityReasonEntryBytes
			return record
		}
		if size > record.Measurements.MaxEntryBytes {
			record.Measurements.MaxEntryBytes = size
		}
		if size > policy.MaxExpandedBytes-record.Measurements.ExpandedBytes {
			record.Reason = CapabilityReasonExpandedBytes
			return record
		}
		record.Measurements.ExpandedBytes += size
		if looksNestedContainer(file.Name) {
			record.Measurements.NestingDepth = 2
			record.Reason = CapabilityReasonNestedContainer
			return record
		}
		body, readErr := readZIPEntry(file, policy.MaxEntryBytes)
		if readErr != nil {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		if len(body) >= 4 && bytes.Equal(body[:4], []byte("PK\x03\x04")) {
			record.Measurements.NestingDepth = 2
			record.Reason = CapabilityReasonNestedContainer
			return record
		}
		name := strings.ToLower(file.Name)
		xmlContent := strings.HasSuffix(name, ".xml") || strings.HasSuffix(name, ".rels") ||
			strings.HasSuffix(name, ".opf") || ext == ".epub" &&
			(strings.HasSuffix(name, ".xhtml") || strings.HasSuffix(name, ".html") ||
				strings.HasSuffix(name, ".htm") || strings.HasSuffix(name, ".svg"))
		if xmlContent {
			mode := xmlMeasureNone
			switch {
			case strings.HasPrefix(name, "xl/worksheets/"):
				mode = xmlMeasureOOXMLSheet
			case name == "content.xml" && ext == ".ods":
				mode = xmlMeasureODSSheet
			case strings.HasSuffix(name, ".opf") && ext == ".epub":
				mode = xmlMeasureEPUBPackage
			}
			external, xmlErr := inspectXML(body, &record.Measurements, mode)
			if xmlErr != nil {
				record.Reason = CapabilityReasonMalformed
				return record
			}
			if external {
				record.Reason = CapabilityReasonExternalReference
				return record
			}
		}
		if ext == ".epub" && strings.HasSuffix(name, ".css") && hasCSSReference(body) {
			record.Reason = CapabilityReasonExternalReference
			return record
		}
		if strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml") {
			record.Measurements.Slides++
		}
		if strings.HasPrefix(name, "xl/worksheets/") && strings.HasSuffix(name, ".xml") {
			record.Measurements.Sheets++
		}
	}
	if record.Measurements.Slides > policy.MaxSlides ||
		record.Measurements.Sheets > policy.MaxSheets || record.Measurements.Cells > policy.MaxCells ||
		record.Measurements.SpineItems > policy.MaxSpineItems ||
		record.Measurements.Resources > policy.MaxResources {
		record.Reason = CapabilityReasonSemanticUnits
		return record
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record
}

type xmlMeasurement uint8

const (
	xmlMeasureNone xmlMeasurement = iota
	xmlMeasureOOXMLSheet
	xmlMeasureODSSheet
	xmlMeasureEPUBPackage
)

func inspectXML(
	data []byte, measurements *CapabilityMeasurements, mode xmlMeasurement,
) (bool, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("inspect XML token: %w", err)
		}
		if directive, ok := token.(xml.Directive); ok &&
			strings.Contains(strings.ToUpper(string(directive)), "DOCTYPE") {
			return true, nil
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch name := strings.ToLower(start.Name.Local); {
		case mode == xmlMeasureOOXMLSheet && name == "c":
			measurements.Cells++
		case mode == xmlMeasureODSSheet && name == "table":
			measurements.Sheets++
		case mode == xmlMeasureODSSheet && name == "table-cell":
			measurements.Cells++
		case mode == xmlMeasureEPUBPackage && name == "itemref":
			measurements.SpineItems++
		case mode == xmlMeasureEPUBPackage && name == "item":
			measurements.Resources++
		}
		for _, attribute := range start.Attr {
			value := strings.TrimSpace(attribute.Value)
			name := strings.ToLower(attribute.Name.Local)
			if name == "targetmode" && strings.EqualFold(value, "External") {
				return true, nil
			}
			if name != "target" && name != "href" && name != "src" && name != "schemalocation" {
				continue
			}
			parsed, parseErr := url.Parse(value)
			if strings.HasPrefix(value, "//") || parseErr == nil && parsed.IsAbs() {
				return true, nil
			}
		}
	}
}

func inspectVisualCapability(data []byte, declaredType string, policy InspectionPolicy) CapabilityRecord {
	metadata, err := DetectBytes(data, declaredType)
	if err != nil {
		return CapabilityRecord{Reason: CapabilityReasonMalformed, MediaType: declaredType}
	}
	if declaredType == "video/quicktime" &&
		(metadata.Kind != KindVideo || metadata.Container != "quicktime") {
		return CapabilityRecord{Reason: CapabilityReasonMalformed, MediaType: declaredType}
	}
	record := CapabilityRecord{MediaFamily: string(metadata.Kind), MediaType: metadata.MediaType,
		Format: string(metadata.Format), Measurements: CapabilityMeasurements{
			Pixels: metadata.Pixels(), Frames: int64(metadata.FrameCount), DurationMS: metadata.DurationMS,
		}}
	if metadata.Kind == KindVideo {
		info, ok := mp4Metadata(data)
		if !ok || !info.sampleAuthority {
			return CapabilityRecord{Reason: CapabilityReasonMalformed, MediaType: declaredType}
		}
		record.Measurements.Frames = info.frameCount
	}
	if policy.MaxPixels <= 0 || record.Measurements.Pixels > policy.MaxPixels ||
		policy.MaxFrames <= 0 || record.Measurements.Frames <= 0 ||
		record.Measurements.Frames > policy.MaxFrames ||
		metadata.Kind == KindVideo && (policy.MaxDurationMS <= 0 || !metadata.DurationKnown ||
			metadata.DurationMS > policy.MaxDurationMS) {
		record.Reason = CapabilityReasonVisualBounds
		return record
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record
}

func inspectPDF(data []byte, policy InspectionPolicy) CapabilityRecord {
	record := CapabilityRecord{MediaFamily: "pdf", MediaType: "application/pdf", Format: "pdf"}
	pages, err := formatdetect.CountPDFPages(data)
	if err != nil {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	record.Measurements.Pages = pages
	if record.Measurements.Pages <= 0 {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	if record.Measurements.Pages > policy.MaxPages {
		record.Reason = CapabilityReasonSemanticUnits
		return record
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record
}

// CountPDFPages resolves the catalog page tree without treating page-like
// bytes in streams, strings, comments, or unrelated objects as authority.
func CountPDFPages(data []byte) (int64, error) {
	return formatdetect.CountPDFPages(data)
}

// PDFInfoFields resolves string fields only from the PDF's final
// cross-reference authority, excluding unrelated objects and stream content.
func PDFInfoFields(data []byte) (map[string]string, error) {
	return formatdetect.PDFInfoFields(data)
}

func inspectWAV(data []byte, policy InspectionPolicy) CapabilityRecord {
	record := CapabilityRecord{MediaFamily: "audio", MediaType: "audio/wav", Format: "wav"}
	if len(data) < 44 || !bytes.Equal(data[:4], []byte("RIFF")) ||
		!bytes.Equal(data[8:12], []byte("WAVE")) ||
		uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	var byteRate, sampleRate, audioBytes uint32
	var audioFormat, channels, blockAlign, bitsPerSample uint16
	var seenFormat, seenAudio bool
	offset := 12
	for offset+8 <= len(data) {
		chunkSize := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		if uint64(chunkSize) > uint64(len(data)) {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		size := int(chunkSize) // #nosec G115 -- bounded by len(data), which is an int
		body := offset + 8
		if size < 0 || body+size > len(data) {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		switch string(data[offset : offset+4]) {
		case "fmt ":
			if seenFormat || size < 16 {
				record.Reason = CapabilityReasonMalformed
				return record
			}
			seenFormat = true
			audioFormat = binary.LittleEndian.Uint16(data[body : body+2])
			channels = binary.LittleEndian.Uint16(data[body+2 : body+4])
			sampleRate = binary.LittleEndian.Uint32(data[body+4 : body+8])
			byteRate = binary.LittleEndian.Uint32(data[body+8 : body+12])
			blockAlign = binary.LittleEndian.Uint16(data[body+12 : body+14])
			bitsPerSample = binary.LittleEndian.Uint16(data[body+14 : body+16])
		case "data":
			if seenAudio {
				record.Reason = CapabilityReasonMalformed
				return record
			}
			seenAudio = true
			audioBytes = chunkSize
		}
		offset = body + size + size%2
	}
	bytesPerSample := uint64(bitsPerSample) / 8
	wantBlockAlign := uint64(channels) * bytesPerSample
	wantByteRate := uint64(sampleRate) * wantBlockAlign
	if offset != len(data) || !seenFormat || !seenAudio ||
		(audioFormat != 1 && audioFormat != 3) || channels == 0 || sampleRate == 0 ||
		bitsPerSample == 0 || bitsPerSample%8 != 0 || wantBlockAlign == 0 ||
		wantBlockAlign > math.MaxUint16 || uint64(blockAlign) != wantBlockAlign ||
		wantByteRate > math.MaxUint32 || uint64(byteRate) != wantByteRate ||
		audioBytes == 0 || uint64(audioBytes)%wantBlockAlign != 0 || policy.MaxDurationMS <= 0 {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	record.Measurements.DurationMS = (int64(audioBytes)*1000 + int64(byteRate) - 1) / int64(byteRate)
	if record.Measurements.DurationMS > policy.MaxDurationMS {
		record.Reason = CapabilityReasonVisualBounds
		return record
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record
}

type mp3FrameHeader struct {
	version         byte
	sampleRate      uint64
	samples, length uint64
}

func inspectMP3(data []byte, policy InspectionPolicy) CapabilityRecord {
	record := CapabilityRecord{MediaFamily: "audio", MediaType: "audio/mpeg", Format: "mp3"}
	if policy.MaxDurationMS <= 0 {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	audioStart, audioEnd, ok := mp3AudioBounds(data)
	if !ok {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	var reference mp3FrameHeader
	var seenFrame bool
	var totalSamples uint64
	for offset := audioStart; offset < audioEnd; {
		frame, ok := parseMP3FrameHeader(data[offset:audioEnd])
		if !ok || seenFrame && (frame.version != reference.version || frame.sampleRate != reference.sampleRate) {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		if !seenFrame {
			reference = frame
			seenFrame = true
		}
		remaining := uint64(audioEnd - offset) //nolint:gosec // offset is below audioEnd by the loop condition
		if frame.length > remaining || math.MaxUint64-totalSamples < frame.samples {
			record.Reason = CapabilityReasonMalformed
			return record
		}
		totalSamples += frame.samples
		offset += int(frame.length) // #nosec G115 -- bounded by the remaining input bytes above.
	}
	if totalSamples == 0 {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	durationMS, ok := mp3DurationMilliseconds(totalSamples, reference.sampleRate)
	if !ok {
		record.Reason = CapabilityReasonMalformed
		return record
	}
	record.Measurements.DurationMS = durationMS
	if durationMS > policy.MaxDurationMS {
		record.Reason = CapabilityReasonVisualBounds
		return record
	}
	record.Eligible, record.Reason = true, CapabilityReasonEligible
	return record
}

func mp3AudioBounds(data []byte) (int, int, bool) {
	start, end := 0, len(data)
	if bytes.HasPrefix(data, []byte("ID3")) {
		if len(data) < 10 {
			return 0, 0, false
		}
		version, revision, flags := data[3], data[4], data[5]
		var allowedFlags byte
		switch version {
		case 2:
			allowedFlags = 0xc0
		case 3:
			allowedFlags = 0xe0
		case 4:
			allowedFlags = 0xf0
		default:
			return 0, 0, false
		}
		if revision == 0xff || flags & ^allowedFlags != 0 {
			return 0, 0, false
		}
		tagSize := 0
		for _, value := range data[6:10] {
			if value&0x80 != 0 {
				return 0, 0, false
			}
			tagSize = tagSize<<7 | int(value)
		}
		if tagSize > maxID3v2TagBytes {
			return 0, 0, false
		}
		footerBytes := 0
		if version == 4 && flags&0x10 != 0 {
			footerBytes = 10
		}
		start = 10 + tagSize + footerBytes
		if start > end {
			return 0, 0, false
		}
		if footerBytes != 0 {
			footer := data[start-footerBytes : start]
			if !bytes.Equal(footer[:3], []byte("3DI")) || footer[3] != version || footer[4] != revision ||
				footer[5] != flags || !bytes.Equal(footer[6:10], data[6:10]) {
				return 0, 0, false
			}
		}
	}
	if end-start >= 128 && bytes.Equal(data[end-128:end-125], []byte("TAG")) {
		end -= 128
	}
	return start, end, start < end
}

func parseMP3FrameHeader(data []byte) (mp3FrameHeader, bool) {
	if len(data) < 4 || data[0] != 0xff || data[1]&0xe0 != 0xe0 {
		return mp3FrameHeader{}, false
	}
	versionID := (data[1] >> 3) & 0x03
	if versionID == 1 || (data[1]>>1)&0x03 != 1 {
		return mp3FrameHeader{}, false
	}
	if data[3]&0x03 == 2 { // emphasis=2 is reserved by MPEG audio.
		return mp3FrameHeader{}, false
	}
	bitrateIndex := (data[2] >> 4) & 0x0f
	sampleRateIndex := (data[2] >> 2) & 0x03
	if bitrateIndex == 0 || bitrateIndex == 15 || sampleRateIndex == 3 {
		return mp3FrameHeader{}, false
	}
	var sampleRate, bitrate, samples, coefficient uint64
	switch versionID {
	case 3: // MPEG-1 Layer III.
		sampleRate = []uint64{44_100, 48_000, 32_000}[sampleRateIndex]
		bitrate = []uint64{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[bitrateIndex-1]
		samples, coefficient = 1_152, 144_000
	case 2: // MPEG-2 Layer III.
		sampleRate = []uint64{22_050, 24_000, 16_000}[sampleRateIndex]
		bitrate = []uint64{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bitrateIndex-1]
		samples, coefficient = 576, 72_000
	case 0: // MPEG-2.5 Layer III.
		sampleRate = []uint64{11_025, 12_000, 8_000}[sampleRateIndex]
		bitrate = []uint64{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[bitrateIndex-1]
		samples, coefficient = 576, 72_000
	default:
		return mp3FrameHeader{}, false
	}
	length := coefficient*bitrate/sampleRate + uint64((data[2]>>1)&1)
	return mp3FrameHeader{version: versionID, sampleRate: sampleRate, samples: samples, length: length}, length >= 4
}

func mp3DurationMilliseconds(samples, sampleRate uint64) (int64, bool) {
	if samples == 0 || sampleRate == 0 || samples > math.MaxUint64/1_000 {
		return 0, false
	}
	milliseconds := (samples*1_000 + sampleRate - 1) / sampleRate
	if milliseconds > math.MaxInt64 {
		return 0, false
	}
	return int64(milliseconds), true
}

func hasCSSReference(data []byte) bool {
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "url") || strings.Contains(lower, "@import")
}

func readZIPEntry(file *zip.File, limit int64) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("open ZIP entry: %w", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(data)) > limit || uint64(len(data)) != file.UncompressedSize64 {
		return nil, errors.New("ZIP entry exceeds bound")
	}
	return data, nil
}

func isTextFamily(ext, mediaType string) bool {
	candidate, ok := formatdetect.CandidateFormatByMediaType(mediaType)
	if ok && slicesContains([]string{
		"txt", "markdown", "rst", "latex", "json", "jsonl", "xml", "yaml",
		"go", "python", "javascript", "eml", "csv",
	}, candidate.ID) {
		return true
	}
	return slicesContains([]string{".txt", ".md", ".csv", ".json", ".jsonl", ".xml", ".yaml", ".yml", ".eml"}, ext)
}

func isZIPFamily(ext, mediaType string, data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], []byte("PK\x03\x04")) ||
		strings.Contains(mediaType, "officedocument") || mediaType == "application/epub+zip" ||
		slicesContains([]string{".pptx", ".pptm", ".xlsx", ".xlsm", ".odp", ".ods", ".epub"}, ext)
}

func looksNestedContainer(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return slicesContains([]string{".zip", ".epub", ".pptx", ".xlsx", ".docx", ".ods", ".odp"}, ext)
}

func requiresDocumentFormatDetection(family string) bool {
	return slicesContains([]string{"text", "structured", "source", "mail", "pdf", "presentation", "spreadsheet", "ebook"}, family)
}

func filenameAllowsFormat(extension, format string) bool {
	allowed := map[string][]string{
		"pdf": {".pdf"}, "pptx": {".pptx"}, "xlsx": {".xlsx"}, "ods": {".ods"},
		"epub": {".epub"}, "csv": {".csv"}, "txt": {".txt"},
		"markdown": {".md", ".markdown"}, "rst": {".rst"}, "latex": {".tex", ".latex"},
		"json": {".json"}, "jsonl": {".jsonl", ".ndjson"}, "xml": {".xml"},
		"yaml": {".yaml", ".yml"}, "go": {".go"}, "python": {".py"},
		"javascript": {".js", ".mjs", ".cjs"}, "eml": {".eml"},
	}
	return slices.Contains(allowed[format], extension)
}

func filenameAllowsVisualFormat(extension, format string) bool {
	allowed := map[string][]string{
		string(FormatJPEG): {".jpg", ".jpeg"}, string(FormatPNG): {".png"},
		string(FormatWebP): {".webp"}, string(FormatGIF): {".gif"}, string(FormatMP4): {".mp4"},
	}
	return slices.Contains(allowed[format], extension)
}

func slicesContains(values []string, candidate string) bool {
	return slices.Contains(values, candidate)
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
