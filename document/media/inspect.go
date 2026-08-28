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
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	webparse "github.com/tdewolff/parse/v2"
	csslexer "github.com/tdewolff/parse/v2/css"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

const (
	capabilityRecordVersion  = 1
	maxInspectionSourceBytes = int64(1 << 30)

	ooxmlWorksheetType = "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"
	ooxmlSlideType     = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml"
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
		record.MediaFamily == string(KindVideo)) &&
		(baseType != record.MediaType || !filenameAllowsVisualFormat(ext, record.Format)) {
		record.Eligible = false
		record.Reason = CapabilityReasonMalformed
	}
	if record.Eligible && record.MediaFamily == "audio" &&
		(ext != ".wav" || baseType != "audio/wav" && baseType != "audio/x-wav") {
		record.Eligible = false
		record.Reason = CapabilityReasonMalformed
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

// UnmarshalJSON decodes a portable record. Local upload authority is not part
// of the representation and is never restored by decoding, including when the
// destination already held it.
func (record *CapabilityRecord) UnmarshalJSON(data []byte) error {
	type portable CapabilityRecord
	var decoded portable
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("media: decode capability record: %w", err)
	}
	*record = CapabilityRecord(decoded)
	return nil
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
		external, err := inspectXML(data, &record.Measurements, xmlMeasureNone, "")
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
	case ".pptx":
		record.MediaFamily = "presentation"
	case ".xlsx", ".ods":
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
	var archiveNames map[string]bool
	if ext == ".epub" {
		archiveNames = make(map[string]bool, len(zr.File))
	}
	if reason := boundZIPEntries(zr.File, policy, archiveNames, &record.Measurements); reason != "" {
		record.Reason = reason
		return record
	}
	var epubPackages map[string]bool
	var declaredTypes contentDeclarations
	if ext == ".epub" {
		var err error
		epubPackages, declaredTypes, err = epubPackageManifest(zr.File, policy.MaxEntryBytes)
		if err != nil {
			record.Reason = CapabilityReasonMalformed
			return record
		}
	} else if isODFExtension(ext) {
		declaredTypes = odfDeclaredTypes(zr.File, policy.MaxEntryBytes)
	} else {
		declaredTypes = ooxmlDeclaredTypes(zr.File, policy.MaxEntryBytes)
	}
	container := zipContainer{
		ext: ext, epubPackages: epubPackages,
		declaredTypes: declaredTypes, archiveNames: archiveNames,
	}
	for _, file := range zr.File {
		reason := container.inspectEntry(file, policy.MaxEntryBytes, &record.Measurements)
		if reason == "" {
			continue
		}
		if reason == CapabilityReasonNestedContainer {
			record.Measurements.NestingDepth = 2
		}
		record.Reason = reason
		return record
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

// inspectXML reports whether a document reaches outside itself. Every
// attribute that is not a namespace declaration or a compact URI is treated as
// a locator, so a resource attribute nobody enumerated still fails closed.
// Static inspection cannot be exhaustive: scripted, entity-split, and
// consumer-specific references remain out of reach. This narrows exposure; the
// provider contract, not this scan, is the boundary.
// boundZIPEntries enforces the per-entry and aggregate byte limits, recording
// what it measured. It returns the reason the container is disqualified, or an
// empty reason when every entry is within bounds.
func boundZIPEntries(
	files []*zip.File, policy InspectionPolicy,
	archiveNames map[string]bool, measurements *CapabilityMeasurements,
) CapabilityReason {
	for _, file := range files {
		if archiveNames != nil {
			archiveNames[file.Name] = true
		}
		if file.Flags&1 != 0 {
			return CapabilityReasonEncryptedContainer
		}
		if file.UncompressedSize64 > math.MaxInt64 {
			return CapabilityReasonEntryBytes
		}
		size := int64(file.UncompressedSize64) // #nosec G115 -- bounded above by MaxInt64
		if size > policy.MaxEntryBytes {
			return CapabilityReasonEntryBytes
		}
		if size > measurements.MaxEntryBytes {
			measurements.MaxEntryBytes = size
		}
		if size > policy.MaxExpandedBytes-measurements.ExpandedBytes {
			return CapabilityReasonExpandedBytes
		}
		measurements.ExpandedBytes += size
		if looksNestedContainer(file.Name) {
			measurements.NestingDepth = 2
			return CapabilityReasonNestedContainer
		}
	}
	return ""
}

// zipContainer is what one entry needs to know about the container holding it:
// which format it is, and what that format declares about its parts.
type zipContainer struct {
	ext           string
	epubPackages  map[string]bool
	declaredTypes contentDeclarations
	archiveNames  map[string]bool
}

// inspectEntry scans one entry for references that leave the document and
// counts it toward its semantic limit. It returns the reason the entry
// disqualifies the container, or an empty reason.
func (container zipContainer) inspectEntry(
	file *zip.File, limit int64, measurements *CapabilityMeasurements,
) CapabilityReason {
	body, err := readZIPEntry(file, limit)
	if err != nil {
		return CapabilityReasonMalformed
	}
	if hasZIPSignature(body) {
		return CapabilityReasonNestedContainer
	}
	isWorksheet, isSlide := container.countsAs(file.Name)
	if container.isMarkup(file.Name) {
		external, err := inspectXML(body, measurements, container.measurement(file.Name, isWorksheet),
			referenceOrigin(file.Name))
		if err != nil {
			return CapabilityReasonMalformed
		}
		if external {
			return CapabilityReasonExternalReference
		}
	}
	if container.isStylesheet(file.Name) {
		external, err := inspectEPUBCSS(body, file.Name, container.archiveNames)
		if err != nil {
			return CapabilityReasonMalformed
		}
		if external {
			return CapabilityReasonExternalReference
		}
	}
	if isSlide {
		measurements.Slides++
	}
	if isWorksheet {
		measurements.Sheets++
	}
	return ""
}

// referenceOrigin is the directory an entry's references resolve from. A
// relationship part describes another part, and its targets resolve from that
// part's directory rather than from the _rels directory holding the file.
func referenceOrigin(name string) string {
	directory := path.Dir(name)
	if path.Base(directory) == "_rels" {
		return path.Dir(directory)
	}
	return directory
}

// countsAs reports the semantic limit an entry counts toward. A part counts
// because of what it is, not where it sits. The package states that, so a
// declared part counts on its declaration alone and an undeclared one falls
// back to its name.
func (container zipContainer) countsAs(name string) (worksheet, slide bool) {
	lowered := strings.ToLower(name)
	worksheet = container.declaredTypes.decides(name, ooxmlWorksheetType,
		strings.HasPrefix(lowered, "xl/worksheets/") && strings.HasSuffix(lowered, ".xml"))
	slide = container.declaredTypes.decides(name, ooxmlSlideType,
		strings.HasPrefix(lowered, "ppt/slides/slide") && strings.HasSuffix(lowered, ".xml"))
	return worksheet, slide
}

// isMarkup reports whether a consumer parses the entry as XML.
func (container zipContainer) isMarkup(name string) bool {
	lowered := strings.ToLower(name)
	return strings.HasSuffix(lowered, ".xml") || strings.HasSuffix(lowered, ".rels") ||
		strings.HasSuffix(lowered, ".opf") || container.declaredTypes.anyXML(name) ||
		container.ext == ".epub" && (strings.HasSuffix(lowered, ".xhtml") ||
			strings.HasSuffix(lowered, ".html") || strings.HasSuffix(lowered, ".htm") ||
			strings.HasSuffix(lowered, ".svg") || container.epubPackages[name])
}

// isStylesheet reports whether a consumer parses the entry as CSS.
func (container zipContainer) isStylesheet(name string) bool {
	return container.ext == ".epub" && (strings.HasSuffix(strings.ToLower(name), ".css") ||
		container.declaredTypes.contains(name, "text/css"))
}

// measurement reports which semantic units the entry's markup carries.
func (container zipContainer) measurement(name string, isWorksheet bool) xmlMeasurement {
	switch {
	case isWorksheet:
		return xmlMeasureOOXMLSheet
	case strings.ToLower(name) == "content.xml" && container.ext == ".ods":
		return xmlMeasureODSSheet
	case container.ext == ".epub" && container.epubPackages[name]:
		return xmlMeasureEPUBPackage
	}
	return xmlMeasureNone
}

func inspectXML(
	data []byte, measurements *CapabilityMeasurements, mode xmlMeasurement, home string,
) (bool, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth, roots := 0, 0
	styleDepth := 0
	var styleText []byte
	bases := make([]xmlScope, 0, 8)
	type odsRow struct {
		depth, repeat, cells int64
	}
	rows := make([]odsRow, 0, 1)
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if roots != 1 || depth != 0 {
					return false, errors.New("XML document must contain exactly one root element")
				}
				return false, nil
			}
			return false, fmt.Errorf("inspect XML token: %w", err)
		}
		switch value := token.(type) {
		case xml.Directive:
			if doctypeReachesOutside(value) {
				return true, nil
			}
		case xml.ProcInst:
			if strings.EqualFold(value.Target, "xml-stylesheet") {
				hrefs, wellFormed := procInstAttributes(value.Inst, "href")
				scope := xmlScope{home: home}
				if len(bases) != 0 {
					scope = bases[len(bases)-1]
				}
				if !wellFormed || len(hrefs) == 0 {
					return true, nil
				}
				for _, href := range hrefs {
					if isExternalXMLURI(href, scope.url, scope.home) {
						return true, nil
					}
				}
			}
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(value)) != 0 {
				return false, errors.New("XML document contains text outside its root element")
			}
			if styleDepth != 0 {
				styleText = append(styleText, value...)
			}
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return false, errors.New("XML document contains multiple root elements")
				}
			}
			inherited := xmlScope{home: home}
			if len(bases) != 0 {
				inherited = bases[len(bases)-1]
			}
			scope, external, attrErr := inspectXMLAttributes(value.Attr, inherited)
			if attrErr != nil {
				return false, attrErr
			}
			if external {
				return true, nil
			}
			bases = append(bases, scope)
			depth++

			name := strings.ToLower(value.Name.Local)
			if styleDepth == 0 && name == "style" && markupStyleNamespace(value.Name.Space) {
				styleDepth = depth
				styleText = styleText[:0]
			}
			switch {
			case mode == xmlMeasureOOXMLSheet && name == "c":
				measurements.Cells = saturatingAdd(measurements.Cells, 1)
			case mode == xmlMeasureODSSheet && name == "table":
				measurements.Sheets = saturatingAdd(measurements.Sheets, 1)
			case mode == xmlMeasureODSSheet && name == "table-row":
				repeat, repeatErr := xmlPositiveRepeat(value.Attr, "number-rows-repeated")
				if repeatErr != nil {
					return false, repeatErr
				}
				rows = append(rows, odsRow{depth: int64(depth), repeat: repeat})
			case mode == xmlMeasureODSSheet && (name == "table-cell" || name == "covered-table-cell"):
				repeat, repeatErr := xmlPositiveRepeat(value.Attr, "number-columns-repeated")
				if repeatErr != nil {
					return false, repeatErr
				}
				if len(rows) == 0 {
					measurements.Cells = saturatingAdd(measurements.Cells, repeat)
				} else {
					last := &rows[len(rows)-1]
					last.cells = saturatingAdd(last.cells, repeat)
				}
			case mode == xmlMeasureEPUBPackage && name == "itemref":
				measurements.SpineItems = saturatingAdd(measurements.SpineItems, 1)
			case mode == xmlMeasureEPUBPackage && name == "item":
				measurements.Resources = saturatingAdd(measurements.Resources, 1)
			}
		case xml.EndElement:
			if styleDepth != 0 && styleDepth == depth && len(bases) != 0 {
				scope := bases[len(bases)-1]
				external, styleErr := cssTextIsExternal(styleText, scope.url, scope.home)
				if styleErr != nil {
					return false, styleErr
				}
				if external {
					return true, nil
				}
				styleDepth = 0
			}
			if mode == xmlMeasureODSSheet && strings.EqualFold(value.Name.Local, "table-row") && len(rows) != 0 {
				row := rows[len(rows)-1]
				rows = rows[:len(rows)-1]
				if row.depth == int64(depth) {
					measurements.Cells = saturatingAddProduct(measurements.Cells, row.cells, row.repeat)
				}
			}
			depth--
			if depth < 0 || len(bases) == 0 {
				return false, errors.New("XML document element depth is invalid")
			}
			bases = bases[:len(bases)-1]
		}
	}
}

// doctypeReachesOutside reports whether a document type declaration names
// something beyond the document. A bare "<!DOCTYPE html>", which nearly every
// XHTML document carries, names nothing; an external identifier names a DTD to
// fetch, and an internal subset can declare entities of its own.
func doctypeReachesOutside(directive xml.Directive) bool {
	text := string(directive)
	if !strings.Contains(strings.ToUpper(text), "DOCTYPE") {
		return false
	}
	upper := strings.ToUpper(text)
	return strings.Contains(upper, "SYSTEM") || strings.Contains(upper, "PUBLIC") ||
		strings.Contains(text, "[")
}

// procInstAttributes returns every value a processing instruction gives for
// one pseudo-attribute. The syntax looks like markup but is not, so the
// decoder hands the whole instruction over as raw text.
//
// Reading it as ordered pairs matters twice over. Searching for the name alone
// matches text inside an earlier attribute's value, which let a decoy steer
// the scan away from the real reference; and returning only the first match
// would let a repeated attribute hide a second one. The caller checks them
// all, and treats an instruction it cannot parse as unresolvable.
func procInstAttributes(instruction []byte, name string) ([]string, bool) {
	var values []string
	rest := string(instruction)
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			return values, true
		}
		separator := strings.IndexAny(rest, "= \t\r\n")
		if separator <= 0 {
			return values, false
		}
		key := rest[:separator]
		rest = strings.TrimLeft(rest[separator:], " \t\r\n")
		if !strings.HasPrefix(rest, "=") {
			return values, false
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		if rest == "" {
			return values, false
		}
		quote := rest[0]
		if quote != '"' && quote != '\'' {
			return values, false
		}
		closing := strings.IndexByte(rest[1:], quote)
		if closing < 0 {
			return values, false
		}
		if strings.EqualFold(key, name) {
			values = append(values, rest[1:1+closing])
		}
		rest = rest[2+closing:]
	}
}

func inspectXMLAttributes(attributes []xml.Attr, inherited xmlScope) (xmlScope, bool, error) {
	scope := inherited
	for _, attribute := range attributes {
		if attribute.Name.Space != "http://www.w3.org/XML/1998/namespace" ||
			!strings.EqualFold(attribute.Name.Local, "base") {
			continue
		}
		value := strings.TrimSpace(attribute.Value)
		// A base is a locator like any other, so it is classified by the same
		// rule. Testing it separately let every later rule miss it.
		if isExternalXMLURI(value, scope.url, scope.home) {
			return xmlScope{}, true, nil
		}
		parsed, err := url.Parse(value)
		if err != nil {
			return xmlScope{}, false, fmt.Errorf("inspect XML base URI: %w", err)
		}
		// A base moves where later references resolve from, so containment has
		// to follow it. Checking them against the entry's own directory let a
		// base spend the distance and a reference cross the root after it.
		scope.home = resolveArchiveDir(scope.home, value)
		if scope.url != nil {
			parsed = scope.url.ResolveReference(parsed)
		}
		scope.url = parsed
	}
	for _, attribute := range attributes {
		value := strings.TrimSpace(attribute.Value)
		switch name := strings.ToLower(attribute.Name.Local); {
		case name == "targetmode":
			if strings.EqualFold(value, "External") {
				return xmlScope{}, true, nil
			}
		case name == "style":
			external, err := cssTextIsExternal([]byte(value), scope.url, scope.home)
			if err != nil {
				return xmlScope{}, false, err
			}
			if external {
				return xmlScope{}, true, nil
			}
		case name == "srcset":
			for candidate := range strings.SplitSeq(value, ",") {
				fields := strings.Fields(candidate)
				if len(fields) == 0 || isExternalXMLURI(fields[0], scope.url, scope.home) {
					return xmlScope{}, true, nil
				}
			}
		case nonLocatorXMLAttribute(attribute):
		default:
			if isExternalXMLURI(value, scope.url, scope.home) {
				return xmlScope{}, true, nil
			}
		}
	}
	return scope, false, nil
}

// xmlScope is where references in one element resolve from: the archive
// directory that containment is measured against, and any xml:base in effect.
type xmlScope struct {
	url  *url.URL
	home string
}

// resolveArchiveDir moves an archive directory by a base reference. A rooted
// base names a directory from the container root; only a base carrying a scheme
// names nothing inside the archive, and clears the directory.
func resolveArchiveDir(home, base string) string {
	if home == "" {
		return ""
	}
	candidate := normalizeReferencePath(base)
	if candidate == "" {
		return home
	}
	if strings.Contains(candidate, "://") {
		return ""
	}
	// A rooted base names its directory from the container root, so it replaces
	// the current origin rather than moving it. Reporting no origin instead
	// meant later references were measured against no container at all, which
	// reads every one of them as contained.
	origin := home
	if rooted, found := strings.CutPrefix(candidate, "/"); found {
		origin, candidate = ".", rooted
	}
	// A base names a document, not a directory: references resolve from the
	// directory holding it, so the last segment is dropped. That segment is a
	// name only when there is one. A trailing separator leaves it empty, and a
	// trailing "." or ".." is a step through the tree that must be walked
	// rather than discarded: dropping it from "../.." spent one level instead
	// of two and left the origin a directory deeper than a reader uses.
	resolved := path.Join(origin, candidate)
	if namesDirectory(candidate) {
		return resolved
	}
	return path.Dir(resolved)
}

// namesDirectory reports whether a reference already names a directory rather
// than a document inside one.
func namesDirectory(reference string) bool {
	if strings.HasSuffix(reference, "/") {
		return true
	}
	last := path.Base(reference)
	return last == "." || last == ".."
}

// nonLocatorXMLAttribute reports attributes whose values name a vocabulary
// rather than a resource. Namespace declarations, xml:base, and compact URIs
// such as an OOXML relationship Type or an EPUB dcterms property all carry
// absolute URIs that no consumer fetches. Everything outside this set is
// treated as a locator.
func nonLocatorXMLAttribute(attribute xml.Attr) bool {
	if attribute.Name.Space == "xmlns" || attribute.Name.Local == "xmlns" {
		return true
	}
	if attribute.Name.Space == "http://www.w3.org/XML/1998/namespace" &&
		strings.EqualFold(attribute.Name.Local, "base") {
		return true
	}
	switch strings.ToLower(attribute.Name.Local) {
	// alttext holds the alternative text for a MathML formula, which is
	// commonly TeX and therefore full of backslashes. It names no resource.
	case "about", "alttext", "datatype", "prefix", "property", "rel", "resource",
		"type", "typeof", "vocab":
		return true
	}
	return false
}

// markupStyleNamespace reports whether a <style> element carries CSS. ODF
// names an unrelated <style:style> element in its own namespace.
func markupStyleNamespace(space string) bool {
	switch space {
	case "", "http://www.w3.org/1999/xhtml", "http://www.w3.org/2000/svg":
		return true
	}
	return false
}

func cssTextIsExternal(data []byte, base *url.URL, home string) (bool, error) {
	references, err := cssReferences(data)
	if err != nil {
		return false, fmt.Errorf("inspect embedded CSS: %w", err)
	}
	for _, reference := range references {
		if isExternalXMLURI(reference, base, home) {
			return true, nil
		}
	}
	return false, nil
}

// isExternalXMLURI reports whether a reference leaves the document. A value is
// external when it names a host or uses a scheme that dereferences without
// one. Colon-bearing values that name no host stay local: a spreadsheet range
// such as A1:C3, a settings key such as ooo:view-settings, a compact URI, and
// a urn are all vocabulary, not locators.
func isExternalXMLURI(value string, base *url.URL, home string) bool {
	if namesHostLocation(value) || escapesArchive(value, home) {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return looksExternalText(value)
	}
	if base != nil {
		parsed = base.ResolveReference(parsed)
	}
	return externalURL(parsed)
}

// looksExternalText screens values that are not parseable URLs, such as the
// ODF length "0%". A consumer with a lenient parser could still dereference
// one, so keep a textual check rather than reading a parse failure as local.
func looksExternalText(value string) bool {
	lowered := strings.ToLower(value)
	if strings.Contains(lowered, "://") {
		return true
	}
	scheme, _, found := strings.Cut(lowered, ":")
	return found && dereferenceableScheme(scheme)
}

// namesHostLocation reports whether a value names a place outside the document
// by network authority or by Windows filesystem path. A consumer decodes a
// reference before resolving it, so the percent-encoded spelling names the
// same place as the literal one and is checked alongside it.
func namesHostLocation(value string) bool {
	if namesHostLocationLiterally(value) {
		return true
	}
	// A consumer that reads a backslash as a separator sees "/\host" and
	// "\/host" as the protocol-relative "//host". The mirrored spelling "\/"
	// was already caught as a rooted Windows path while "/\" was not, which
	// left one half of the same idea eligible.
	if strings.HasPrefix(normalizeReferencePath(value), "//") {
		return true
	}
	decoded, err := url.PathUnescape(value)
	return err == nil && decoded != value && namesHostLocationLiterally(decoded)
}

func namesHostLocationLiterally(value string) bool {
	// "//host/path" is protocol-relative; the UNC spelling "\\host\path" is
	// caught as a rooted Windows path.
	return strings.HasPrefix(value, "//") || hasDriveLetter(value) || hasRootedBackslash(value)
}

// hasRootedBackslash reports whether a value is a Windows path rooted on the
// current drive, such as "\Users\victim\secret.txt". A package resource is
// named by a URI, so it is never spelled with backslashes, and the UNC and
// drive-letter spellings of the same idea are already rejected.
//
// A further separator is required so that a lone leading backslash, which is
// not a path at all, stays local: a TeX macro in an alt-text attribute begins
// that way.
func hasRootedBackslash(value string) bool {
	return strings.HasPrefix(value, `\`) && strings.ContainsAny(value[1:], `\/`)
}

// hasDriveLetter reports whether a value is a Windows drive-absolute path such
// as "C:\secret.txt". url.Parse reads the drive letter as a scheme, and a
// single letter is never a dereferenceable one, so the value would otherwise
// pass as a local reference. A package resource is named relative to the
// container or by an absolute part name, never by drive.
func hasDriveLetter(value string) bool {
	if len(value) < 3 || value[1] != ':' {
		return false
	}
	letter := value[0] | 0x20
	return letter >= 'a' && letter <= 'z' && (value[2] == '\\' || value[2] == '/')
}

// normalizeReferencePath reduces a reference to the path a consumer resolves.
// A query or fragment selects a position inside a resource rather than naming
// another one; a percent escape is another spelling of the character it
// encodes; and a Windows consumer reads a backslash as a separator.
//
// Every place that measures where a reference lands shares this, because
// decoding in one of them and not the other is what let an encoded spelling
// land somewhere the check never looked.
func normalizeReferencePath(value string) string {
	candidate := value
	if index := strings.IndexAny(candidate, "?#"); index >= 0 {
		candidate = candidate[:index]
	}
	// Decoding fails on a value that is not a reference at all, such as the
	// ODF length "0%", which leaves the original text to measure.
	if decoded, err := url.PathUnescape(candidate); err == nil {
		candidate = decoded
	}
	return strings.ReplaceAll(candidate, `\`, "/")
}

// escapesArchive reports whether a reference climbs above the container root.
// Every resource a package names lives inside the container, so a reference
// that resolves past the root names a file on the host instead. home is the
// directory of the entry holding the reference, and is empty for a standalone
// document, where there is no container to leave.
func escapesArchive(value, home string) bool {
	if home == "" {
		return false
	}
	candidate := normalizeReferencePath(value)
	if candidate == "" || path.IsAbs(candidate) {
		return false
	}
	return leavesArchiveRoot(path.Clean(path.Join(home, candidate)))
}

// leavesArchiveRoot reports whether a cleaned path lands outside the container.
// path.Clean spells the root's own parent as ".." with no trailing separator,
// so a prefix test alone reads that one landing place as contained.
func leavesArchiveRoot(resolved string) bool {
	return resolved == ".." || strings.HasPrefix(resolved, "../")
}

func externalURL(parsed *url.URL) bool {
	return parsed.Host != "" || dereferenceableScheme(parsed.Scheme)
}

// dereferenceableScheme reports schemes a consumer resolves on its own. A
// hostless form such as "http:169.254.169.254/latest" still reaches the
// network, so the scheme decides even when no authority was parsed. Values
// such as "A1:C3" and "urn:uuid:..." name nothing to fetch.
func dereferenceableScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https", "ftp", "ftps", "sftp", "ws", "wss", "gopher",
		"file", "data", "jar", "javascript", "vbscript":
		return true
	}
	return false
}

func xmlPositiveRepeat(attributes []xml.Attr, name string) (int64, error) {
	for _, attribute := range attributes {
		if !strings.EqualFold(attribute.Name.Local, name) {
			continue
		}
		repeat, err := strconv.ParseInt(strings.TrimSpace(attribute.Value), 10, 64)
		if err != nil || repeat <= 0 {
			return 0, fmt.Errorf("ODS %s must be a positive integer", name)
		}
		return repeat, nil
	}
	return 1, nil
}

func saturatingAdd(left, right int64) int64 {
	if right > math.MaxInt64-left {
		return math.MaxInt64
	}
	return left + right
}

func saturatingAddProduct(total, left, right int64) int64 {
	if left != 0 && right > math.MaxInt64/left {
		return math.MaxInt64
	}
	return saturatingAdd(total, left*right)
}

func inspectVisualCapability(data []byte, declaredType string, policy InspectionPolicy) CapabilityRecord {
	metadata, err := DetectBytes(data, declaredType)
	if err != nil {
		return CapabilityRecord{Reason: CapabilityReasonMalformed, MediaType: declaredType}
	}
	record := CapabilityRecord{MediaFamily: string(metadata.Kind), MediaType: metadata.MediaType,
		Format: string(metadata.Format), Measurements: CapabilityMeasurements{
			Pixels: metadata.Pixels(), Frames: int64(metadata.FrameCount), DurationMS: metadata.DurationMS,
		}}
	if metadata.Kind == KindVideo {
		info, ok := mp4Metadata(data)
		if !ok {
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
	measurements, err := formatdetect.InspectPDF(data, formatdetect.PDFLimits{
		MaxExpandedBytes: policy.MaxExpandedBytes,
		MaxEntryBytes:    policy.MaxEntryBytes,
		MaxEntries:       policy.MaxEntries,
	})
	if err != nil {
		switch {
		case errors.Is(err, formatdetect.ErrPDFEncrypted):
			record.Reason = CapabilityReasonEncryptedContainer
		case errors.Is(err, formatdetect.ErrPDFExpandedBytes):
			record.Reason = CapabilityReasonExpandedBytes
		case errors.Is(err, formatdetect.ErrPDFEntryBytes):
			record.Reason = CapabilityReasonEntryBytes
		case errors.Is(err, formatdetect.ErrPDFEntryCount):
			record.Reason = CapabilityReasonEntryCount
		case errors.Is(err, formatdetect.ErrPDFUnbounded):
			record.Reason = CapabilityReasonUnboundedFamily
		case errors.Is(err, formatdetect.ErrPDFExternalReference):
			record.Reason = CapabilityReasonExternalReference
		default:
			record.Reason = CapabilityReasonMalformed
		}
		return record
	}
	record.Measurements.Pages = measurements.Pages
	record.Measurements.CompressedBytes = measurements.CompressedBytes
	record.Measurements.ExpandedBytes = measurements.ExpandedBytes
	record.Measurements.Entries = measurements.Entries
	record.Measurements.MaxEntryBytes = measurements.MaxEntryBytes
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

func inspectEPUBCSS(data []byte, stylesheet string, archiveNames map[string]bool) (bool, error) {
	references, err := cssReferences(data)
	if err != nil {
		return false, err
	}
	for _, reference := range references {
		if !cssReferenceInArchive(reference, stylesheet, archiveNames) {
			return true, nil
		}
	}
	return false, nil
}

func cssReferences(data []byte) ([]string, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("CSS is not valid UTF-8")
	}
	lexer := csslexer.NewLexer(webparse.NewInput(bytes.NewReader(data)))
	references := make([]string, 0, 4)
	wantImport := false
	for {
		tokenType, token := lexer.Next()
		switch tokenType {
		case csslexer.ErrorToken:
			if errors.Is(lexer.Err(), io.EOF) {
				return references, nil
			}
			return nil, fmt.Errorf("lex CSS: %w", lexer.Err())
		case csslexer.BadStringToken, csslexer.BadURLToken:
			return nil, errors.New("CSS contains an invalid string or URL")
		case csslexer.WhitespaceToken, csslexer.CommentToken:
			continue
		case csslexer.AtKeywordToken:
			keyword, err := decodeCSSEscapes(token[1:])
			if err != nil {
				return nil, err
			}
			wantImport = strings.EqualFold(keyword, "import")
		case csslexer.URLToken:
			reference, err := cssURLTokenReference(token)
			if err != nil {
				return nil, err
			}
			references = append(references, reference)
			wantImport = false
		case csslexer.FunctionToken:
			name, err := decodeCSSEscapes(token[:len(token)-1])
			if err != nil {
				return nil, err
			}
			isURL := strings.EqualFold(name, "url")
			if wantImport && !isURL {
				return nil, errors.New("CSS import does not name a stylesheet")
			}
			if isURL {
				reference, err := cssURLFunctionReference(lexer)
				if err != nil {
					return nil, err
				}
				references = append(references, reference)
			}
			wantImport = false
		case csslexer.StringToken:
			if wantImport {
				reference, err := cssStringTokenValue(token)
				if err != nil {
					return nil, err
				}
				references = append(references, reference)
			}
			wantImport = false
		default:
			if wantImport {
				return nil, errors.New("CSS import does not name a stylesheet")
			}
		}
	}
}

func cssURLTokenReference(token []byte) (string, error) {
	open := bytes.IndexByte(token, '(')
	if open < 0 || len(token) <= open+1 || token[len(token)-1] != ')' {
		return "", errors.New("CSS URL is incomplete")
	}
	value := bytes.TrimSpace(token[open+1 : len(token)-1])
	if len(value) != 0 && (value[0] == '\'' || value[0] == '"') {
		return cssStringTokenValue(value)
	}
	return decodeCSSEscapes(value)
}

func cssURLFunctionReference(lexer *csslexer.Lexer) (string, error) {
	var value []byte
	quoted := false
	for {
		tokenType, token := lexer.Next()
		switch tokenType {
		case csslexer.ErrorToken:
			return "", errors.New("CSS URL function is incomplete")
		case csslexer.BadStringToken, csslexer.BadURLToken:
			return "", errors.New("CSS URL function is invalid")
		case csslexer.CommentToken:
			continue
		case csslexer.WhitespaceToken:
			value = append(value, token...)
		case csslexer.RightParenthesisToken:
			value = bytes.TrimSpace(value)
			if quoted {
				return cssStringTokenValue(value)
			}
			if bytes.ContainsAny(value, " \t\r\n\f") {
				return "", errors.New("CSS URL contains unescaped whitespace")
			}
			return decodeCSSEscapes(value)
		case csslexer.StringToken:
			if len(bytes.TrimSpace(value)) != 0 {
				return "", errors.New("CSS URL has multiple values")
			}
			quoted = true
			value = append(value, token...)
		default:
			if quoted {
				return "", errors.New("CSS URL has content after its string")
			}
			value = append(value, token...)
		}
	}
}

func cssStringTokenValue(token []byte) (string, error) {
	if len(token) < 2 || token[len(token)-1] != token[0] || (token[0] != '\'' && token[0] != '"') {
		return "", errors.New("CSS string is incomplete")
	}
	return decodeCSSEscapes(token[1 : len(token)-1])
}

func decodeCSSEscapes(data []byte) (string, error) {
	var decoded strings.Builder
	decoded.Grow(len(data))
	for index := 0; index < len(data); {
		if data[index] != '\\' {
			decoded.WriteByte(data[index])
			index++
			continue
		}
		index++
		if index == len(data) {
			return "", errors.New("CSS escape is incomplete")
		}
		if isCSSNewline(data[index]) {
			if data[index] == '\r' && index+1 < len(data) && data[index+1] == '\n' {
				index++
			}
			index++
			continue
		}
		start := index
		for index < len(data) && index-start < 6 && isCSSHex(data[index]) {
			index++
		}
		if start != index {
			value, err := strconv.ParseUint(string(data[start:index]), 16, 32)
			if err != nil {
				return "", fmt.Errorf("decode CSS escape: %w", err)
			}
			character := utf8.RuneError
			if value > 0 && value <= unicode.MaxRune {
				candidate := rune(value) // #nosec G115 -- bounded above by unicode.MaxRune
				if utf8.ValidRune(candidate) {
					character = candidate
				}
			}
			decoded.WriteRune(character)
			if index < len(data) && isCSSWhitespace(data[index]) {
				if data[index] == '\r' && index+1 < len(data) && data[index+1] == '\n' {
					index++
				}
				index++
			}
			continue
		}
		decoded.WriteByte(data[index])
		index++
	}
	return decoded.String(), nil
}

func cssReferenceInArchive(reference, stylesheet string, archiveNames map[string]bool) bool {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || strings.HasPrefix(reference, "//") {
		return false
	}
	referencePath, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil || path.IsAbs(referencePath) || strings.Contains(referencePath, "\\") {
		return false
	}
	if referencePath == "" {
		return archiveNames[stylesheet]
	}
	resolved := path.Clean(path.Join(path.Dir(stylesheet), referencePath))
	if resolved == "." || path.IsAbs(resolved) || leavesArchiveRoot(resolved) {
		return false
	}
	return archiveNames[resolved]
}

func isCSSHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func isCSSWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || isCSSNewline(value)
}

func isCSSNewline(value byte) bool {
	return value == '\r' || value == '\n' || value == '\f'
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

func hasZIPSignature(data []byte) bool {
	return len(data) >= 4 && (bytes.Equal(data[:4], []byte("PK\x03\x04")) ||
		bytes.Equal(data[:4], []byte("PK\x05\x06")))
}

// epubPackageManifest returns the package document path and the media type the
// manifest declares for each resource it names. A reader interprets a resource
// by its declared type, so inspection must follow that declaration rather than
// the entry's filename.
func epubPackageManifest(
	files []*zip.File, limit int64,
) (map[string]bool, contentDeclarations, error) {
	var container *zip.File
	for _, file := range files {
		if file.Name == "META-INF/container.xml" {
			container = file
			break
		}
	}
	if container == nil {
		return nil, nil, errors.New("EPUB container document is missing")
	}
	body, err := readZIPEntry(container, limit)
	if err != nil {
		return nil, nil, err
	}
	var document struct {
		XMLName   xml.Name `xml:"container"`
		Rootfiles struct {
			Items []struct {
				FullPath string `xml:"full-path,attr"`
			} `xml:"rootfile"`
		} `xml:"rootfiles"`
	}
	if err := xml.Unmarshal(body, &document); err != nil ||
		document.XMLName.Space != "urn:oasis:names:tc:opendocument:xmlns:container" ||
		len(document.Rootfiles.Items) == 0 {
		return nil, nil, errors.New("EPUB container document is invalid")
	}
	// A container may declare several renditions. Each one's package document
	// and manifest is reachable content, so inspect all of them.
	packages := make(map[string]bool, len(document.Rootfiles.Items))
	declared := make(contentDeclarations)
	for _, rootfile := range document.Rootfiles.Items {
		packagePath, err := epubArchivePath(rootfile.FullPath, "")
		if err != nil {
			return nil, nil, err
		}
		var packageFile *zip.File
		for _, file := range files {
			if file.Name == packagePath {
				packageFile = file
				break
			}
		}
		if packageFile == nil {
			return nil, nil, errors.New("EPUB package document is missing")
		}
		packages[packagePath] = true
		if err := epubManifestTypes(packageFile, packagePath, limit, declared); err != nil {
			return nil, nil, err
		}
	}
	return packages, declared, nil
}

func epubManifestTypes(
	packageFile *zip.File, packagePath string, limit int64, declared contentDeclarations,
) error {
	body, err := readZIPEntry(packageFile, limit)
	if err != nil {
		return err
	}
	var document struct {
		Base     string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
		Manifest struct {
			Base  string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
			Items []struct {
				HRef      string `xml:"href,attr"`
				MediaType string `xml:"media-type,attr"`
				Base      string `xml:"http://www.w3.org/XML/1998/namespace base,attr"`
			} `xml:"item"`
		} `xml:"manifest"`
	}
	if err := xml.Unmarshal(body, &document); err != nil {
		return errors.New("EPUB package document is invalid")
	}
	base := path.Dir(packagePath)
	for _, item := range document.Manifest.Items {
		// A query or fragment still names the same archive entry, so strip it
		// rather than dropping the declaration and leaving the entry unscanned.
		href := item.HRef
		if index := strings.IndexAny(href, "?#"); index >= 0 {
			href = href[:index]
		}
		if href == "" {
			continue
		}
		declaredType := normalizeDeclaredType(item.MediaType)
		// Readers disagree about whether xml:base applies here, so declare the
		// item at every path one of them may resolve it to. A declaration for a
		// path that holds no entry is inert, while a missing one leaves a
		// reachable resource unscanned.
		for _, directory := range manifestBases(base, document.Base, document.Manifest.Base, item.Base) {
			resource, err := epubArchivePath(href, directory)
			if err != nil {
				// A manifest may name a remote or absolute resource. inspectXML
				// classifies that when it scans the package document itself.
				continue
			}
			declared.add(resource, declaredType)
		}
	}
	return nil
}

// manifestBases returns each directory a manifest item may resolve against.
// The xml:base values arrive outermost first, and a reader may honour any
// prefix of them: the package document's own directory when it honours none,
// the fully shifted directory when it honours all, and a step in between when
// it reads the attribute on some elements and not others. Recording only the
// two ends left an item under a neutral name at an intermediate step declared
// nowhere, and so inspected by its file name rather than as the markup a
// reader loads.
func manifestBases(packageDir string, bases ...string) []string {
	directories := []string{packageDir}
	shifted := packageDir
	for _, base := range bases {
		if strings.TrimSpace(base) == "" {
			continue
		}
		shifted = resolveArchiveDir(shifted, strings.TrimSpace(base))
		if shifted == "" {
			return directories
		}
		if !slices.Contains(directories, shifted) {
			directories = append(directories, shifted)
		}
	}
	return directories
}

// epubArchivePath resolves one EPUB-relative reference to an archive entry
// name, rejecting absolute, remote, and traversing forms.
func epubArchivePath(reference, base string) (string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.IsAbs() || parsed.Host != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("EPUB resource path is invalid")
	}
	resource, err := url.PathUnescape(parsed.EscapedPath())
	if err != nil {
		return "", errors.New("EPUB resource path is invalid")
	}
	// An absolute reference inside a container names the container root, so it
	// must not be joined with the package directory: that silently renamed it
	// to a different entry and left the real one undeclared.
	if rooted, found := strings.CutPrefix(resource, "/"); found {
		resource = rooted
	} else if base != "" && base != "." {
		resource = path.Join(base, resource)
	}
	resource = path.Clean(resource)
	if resource == "." || path.IsAbs(resource) || leavesArchiveRoot(resource) {
		return "", errors.New("EPUB resource path is invalid")
	}
	return resource, nil
}

// contentDeclarations maps an archive entry to the content types its container
// declares for it. An OOXML part has exactly one type. An EPUB container that
// lists several renditions can declare one entry more than once, and each
// declaration decides how some reader interprets those bytes, so inspection
// has to satisfy all of them rather than pick a winner.
type contentDeclarations map[string][]string

func (declarations contentDeclarations) add(name, mediaType string) {
	if slices.Contains(declarations[name], mediaType) {
		return
	}
	declarations[name] = append(declarations[name], mediaType)
}

func (declarations contentDeclarations) contains(name, mediaType string) bool {
	return slices.Contains(declarations[name], mediaType)
}

// decides reports whether an entry is of the given type. A container states the
// type of every part it names, so a declaration settles the question in both
// directions: a part declared as something else is not this type whatever its
// name suggests. Only an entry the container never declared falls back.
func (declarations contentDeclarations) decides(name, mediaType string, fallback bool) bool {
	if _, declared := declarations[name]; declared {
		return declarations.contains(name, mediaType)
	}
	return fallback
}

func (declarations contentDeclarations) anyXML(name string) bool {
	return slices.ContainsFunc(declarations[name], isXMLMediaType)
}

// normalizeDeclaredType reduces a declared content type to its bare media type.
// A parameter such as "; charset=utf-8" does not change how a consumer reads a
// resource, so it must not change whether inspection reads it either.
func normalizeDeclaredType(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	parsed, _, err := mime.ParseMediaType(lowered)
	if err != nil {
		return lowered
	}
	return parsed
}

// isXMLMediaType reports whether a declared media type makes a consumer parse
// the resource as markup. The "+xml" suffix is the structured-syntax suffix
// every OOXML part type carries.
func isXMLMediaType(mediaType string) bool {
	if strings.HasSuffix(mediaType, "+xml") {
		return true
	}
	switch mediaType {
	case "application/xml", "text/xml", "text/html":
		return true
	}
	return false
}

// ooxmlDeclaredTypes maps each OOXML part to the content type the package
// declares for it. A part is markup because [Content_Types].xml says so, not
// because of how it is named.
func ooxmlDeclaredTypes(files []*zip.File, limit int64) contentDeclarations {
	var contentTypes *zip.File
	for _, file := range files {
		if file.Name == "[Content_Types].xml" {
			contentTypes = file
			break
		}
	}
	if contentTypes == nil {
		return nil
	}
	body, err := readZIPEntry(contentTypes, limit)
	if err != nil {
		return nil
	}
	var document struct {
		Defaults []struct {
			Extension   string `xml:"Extension,attr"`
			ContentType string `xml:"ContentType,attr"`
		} `xml:"Default"`
		Overrides []struct {
			PartName    string `xml:"PartName,attr"`
			ContentType string `xml:"ContentType,attr"`
		} `xml:"Override"`
	}
	if err := xml.Unmarshal(body, &document); err != nil {
		// The part is inspected as XML in its own right; a parse failure is
		// classified there rather than silently trusted here.
		return nil
	}
	byExtension := make(map[string]string, len(document.Defaults))
	for _, def := range document.Defaults {
		byExtension["."+strings.ToLower(strings.TrimSpace(def.Extension))] =
			normalizeDeclaredType(def.ContentType)
	}
	declared := make(contentDeclarations, len(files))
	for _, file := range files {
		if declaredType, ok := byExtension[strings.ToLower(path.Ext(file.Name))]; ok {
			declared[file.Name] = []string{declaredType}
		}
	}
	for _, override := range document.Overrides {
		part := strings.TrimPrefix(strings.TrimSpace(override.PartName), "/")
		if part == "" {
			continue
		}
		// A part name is a URI path, so it may be percent-encoded. A consumer
		// decodes it before matching, and an encoded name that matched nothing
		// left its part undeclared and inspected only by its filename.
		declaredType := normalizeDeclaredType(override.ContentType)
		// An Override replaces the Default for that part rather than adding to it.
		declared[part] = []string{declaredType}
		if decoded, err := url.PathUnescape(part); err == nil && decoded != part {
			declared[decoded] = []string{declaredType}
		}
	}
	return declared
}

// odfDeclaredTypes maps each ODF part to the content type META-INF/manifest.xml
// declares for it. ODF states the type of every part it carries, the way OOXML
// states it in [Content_Types].xml and EPUB states it in the package manifest.
// Reading only the other two left an ODF part classified by its file name, so a
// drawing declared as SVG under a name such as "Pictures/image.dat" was stored
// without ever being read as the markup a renderer parses it as.
func odfDeclaredTypes(files []*zip.File, limit int64) contentDeclarations {
	var manifest *zip.File
	for _, file := range files {
		if file.Name == "META-INF/manifest.xml" {
			manifest = file
			break
		}
	}
	if manifest == nil {
		return nil
	}
	body, err := readZIPEntry(manifest, limit)
	if err != nil {
		return nil
	}
	var document struct {
		Entries []struct {
			FullPath  string `xml:"full-path,attr"`
			MediaType string `xml:"media-type,attr"`
		} `xml:"file-entry"`
	}
	if err := xml.Unmarshal(body, &document); err != nil {
		// The part is inspected as XML in its own right; a parse failure is
		// classified there rather than silently trusted here.
		return nil
	}
	declared := make(contentDeclarations, len(document.Entries))
	for _, entry := range document.Entries {
		// A full-path is a URI path, so it may be percent-encoded, and the
		// package root is spelled "/" and names no part.
		part := strings.TrimSpace(entry.FullPath)
		if part == "" || part == "/" {
			continue
		}
		declaredType := normalizeDeclaredType(entry.MediaType)
		declared.add(part, declaredType)
		if decoded, err := url.PathUnescape(part); err == nil && decoded != part {
			declared.add(decoded, declaredType)
		}
	}
	return declared
}

func isTextFamily(ext, mediaType string) bool {
	candidate, ok := formatdetect.CandidateFormatByMediaType(mediaType)
	if ok && slices.Contains([]string{
		"txt", "markdown", "rst", "latex", "json", "jsonl", "xml", "yaml",
		"go", "python", "javascript", "eml", "csv",
	}, candidate.ID) {
		return true
	}
	return slices.Contains([]string{".txt", ".md", ".csv", ".json", ".jsonl", ".xml", ".yaml", ".yml", ".eml"}, ext)
}

func isZIPFamily(ext, mediaType string, data []byte) bool {
	return len(data) >= 4 && bytes.Equal(data[:4], []byte("PK\x03\x04")) ||
		strings.Contains(mediaType, "officedocument") || mediaType == "application/epub+zip" ||
		slices.Contains([]string{".pptx", ".xlsx", ".ods", ".epub"}, ext)
}

// isODFExtension reports whether a container states its part types in
// META-INF/manifest.xml rather than in [Content_Types].xml.
func isODFExtension(ext string) bool {
	return slices.Contains([]string{".ods", ".odp"}, ext)
}

func looksNestedContainer(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return slices.Contains([]string{".zip", ".epub", ".pptx", ".xlsx", ".docx", ".ods", ".odp"}, ext)
}

func requiresDocumentFormatDetection(family string) bool {
	return slices.Contains([]string{"text", "structured", "source", "mail", "pdf", "presentation", "spreadsheet", "ebook"}, family)
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
