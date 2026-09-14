package transfer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

type Finding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Path     string `json:"path"`
	Detail   string `json:"detail"`
}

type BoundsReport struct {
	ManifestBytes int64 `json:"manifest_bytes"`
	RecordsBytes  int64 `json:"records_bytes"`
	ExpandedBytes int64 `json:"expanded_bytes"`
	BlobBytes     int64 `json:"blob_bytes"`
	RecordLines   int64 `json:"record_lines"`
	Blobs         int64 `json:"blobs"`
}

type Report struct {
	Format             string              `json:"format"`
	PackageID          string              `json:"package_id"`
	ArchiveID          string              `json:"archive_id"`
	NextCursor         string              `json:"next_cursor"`
	Valid              bool                `json:"valid"`
	Partial            bool                `json:"partial"`
	FindingsTruncated  bool                `json:"findings_truncated"`
	Findings           []Finding           `json:"findings"`
	FindingsTotal      int64               `json:"findings_total"`
	Counts             CountsV1            `json:"counts"`
	Bounds             BoundsReport        `json:"bounds"`
	IntegrityAuthority IntegrityAuthority  `json:"integrity_authority"`
	PackageAuthority   PackageAuthority    `json:"package_authority"`
	LegacyEvidence     *LegacyEvidence     `json:"legacy_evidence,omitzero"`
	FormatLimitations  []CapabilityEntryV1 `json:"format_limitations,omitzero"`
}

func (report *Report) AddFinding(finding Finding) {
	report.FindingsTotal++
	if len(report.Findings) < MaxInlineFindings {
		report.Findings = append(report.Findings, finding)
		return
	}
	report.FindingsTruncated = true
}

func ReconcileCounts(manifest, terminal, observed CountsV1) error {
	if manifest != terminal || terminal != observed {
		return errors.New("transfer: package_integrity_failed: counts")
	}
	return nil
}

func Validate(ctx context.Context, reader PackageReader) (Report, error) {
	validator, err := newPackageValidator(ctx, reader)
	if err != nil {
		return Report{}, err
	}
	runErr := validator.run()
	closeErr := validator.close()
	return finishValidation(validator, runErr, closeErr)
}

func finishValidation(validator *packageValidator, runErr, closeErr error) (Report, error) {
	if closeErr != nil {
		validator.report.AddFinding(Finding{Severity: "error", Code: "package_integrity_failed", Path: "package", Detail: "validation cleanup failed"})
	}
	if runErr != nil {
		if validator.report.FindingsTotal == 0 {
			validator.report.AddFinding(Finding{Severity: "error", Code: "package_integrity_failed", Path: "package", Detail: "package validation failed"})
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			if closeErr != nil {
				return validator.report, errors.Join(runErr, errors.New("transfer: validation cleanup failed"))
			}
			return validator.report, runErr
		}
		return validator.report, errors.New("transfer: package validation failed")
	}
	if closeErr != nil {
		return validator.report, errors.New("transfer: validation cleanup failed")
	}
	validator.report.Valid = true
	return validator.report, nil
}

type packageValidator struct {
	ctx               context.Context
	reader            PackageReader
	report            Report
	index             *validationIndex
	manifest          ManifestV1
	selectionDigest   string
	observed          CountsV1
	terminal          *CompleteV1
	phase             int
	recordsPrefix     hash.Hash
	recordsWhole      hash.Hash
	recordsChecksum   string
	manifestChecksum  string
	manifestBytes     int64
	recordsBytes      int64
	blobCount         int64
	blobBytes         int64
	legacyEvidence    *LegacyEvidence
	legacyBinding     LegacyArchiveBinding
	legacyLimitations []CapabilityEntryV1
}

func newPackageValidator(ctx context.Context, reader PackageReader) (*packageValidator, error) {
	if reader == nil {
		return nil, errors.New("transfer: package reader is required")
	}
	index, err := newValidationIndex()
	if err != nil {
		return nil, err
	}
	validator := &packageValidator{
		ctx: ctx, reader: reader, index: index,
		recordsPrefix: sha256.New(), recordsWhole: sha256.New(),
		report: Report{IntegrityAuthority: IntegritySumsOnly, PackageAuthority: PackageAuthorityFullV1},
	}
	if legacy, ok := reader.(interface {
		legacyAuthority() (LegacyEvidence, LegacyArchiveBinding, []CapabilityEntryV1)
	}); ok {
		evidence, binding, limitations := legacy.legacyAuthority()
		validator.legacyEvidence = &evidence
		validator.legacyBinding = binding
		validator.legacyLimitations = slices.Clone(limitations)
		validator.report.PackageAuthority = PackageAuthorityLegacyCompatibility
		validator.report.LegacyEvidence = &evidence
		validator.report.FormatLimitations = slices.Clone(limitations)
	}
	return validator, nil
}

func (validator *packageValidator) close() error {
	return validator.index.Close()
}

func (validator *packageValidator) run() error {
	if verifier, ok := validator.reader.(interface {
		verifyLegacyEvidence(ctx context.Context) error
	}); ok {
		if err := verifier.verifyLegacyEvidence(validator.ctx); err != nil {
			if ctxErr := contextError(err); ctxErr != nil {
				return ctxErr
			}
			return validator.fail("package_integrity_failed", "legacy evidence", "original legacy bytes do not match their retained digest")
		}
	}
	if err := validator.inventory(); err != nil {
		return err
	}
	if err := validator.loadManifest(); err != nil {
		return err
	}
	if err := validator.verifyChecksums(); err != nil {
		return err
	}
	if err := validator.validateRelation("inventory", true, "package", "package_integrity_failed"); err != nil {
		return err
	}
	if err := validator.validateRelation("inventory_sizes", true, "package", "package_integrity_failed"); err != nil {
		return err
	}
	if err := validator.reader.Records(validator.ctx, validator.visitRecord); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, errAdmissionFinding) {
			return err
		}
		return validator.fail("incomplete_package", "records.jsonl", "record stream framing or read failed")
	}
	if validator.terminal == nil {
		return validator.fail("incomplete_package", "records.jsonl", "terminal record is missing")
	}
	if validator.recordsBytes != validator.report.Bounds.RecordsBytes || hex.EncodeToString(validator.recordsWhole.Sum(nil)) != validator.recordsChecksum {
		return validator.fail("package_integrity_failed", "records.jsonl", "record stream bytes do not match checksums")
	}
	prefixDigest := hex.EncodeToString(validator.recordsPrefix.Sum(nil))
	if prefixDigest != validator.manifest.RecordsSHA256 || prefixDigest != validator.terminal.RecordsSHA256 {
		return validator.fail("package_integrity_failed", "records.jsonl", "record prefix digest does not reconcile")
	}
	if err := ReconcileCounts(validator.manifest.Counts, validator.terminal.Counts, validator.observed); err != nil {
		return validator.fail("package_integrity_failed", "records.jsonl", "declared and observed counts do not reconcile")
	}
	if validator.terminal.Truncated {
		if validator.terminal.Continuation == "" || len(validator.terminal.Continuation) > MaxContinuationBytes {
			return validator.fail("incomplete_package", "records.jsonl", "partial package continuation is invalid")
		}
		validator.report.Partial = true
		validator.report.NextCursor = string(validator.terminal.Continuation)
	} else if validator.terminal.Continuation != "" {
		return validator.fail("package_integrity_failed", "records.jsonl", "complete package carries a continuation")
	}
	if validator.manifest.Snapshot.Consistency == "streamed_watermark" {
		windowEnd, _ := time.Parse(time.RFC3339, validator.manifest.Selection.Window.End)
		highWatermark, _ := time.Parse(time.RFC3339Nano, validator.manifest.Snapshot.HighWatermark)
		if windowEnd.After(highWatermark) && !validator.report.Partial {
			return validator.fail("package_integrity_failed", "transfer.json", "streamed watermark does not cover the selection window")
		}
	}
	if validator.manifest.BlobCount != validator.blobCount || validator.manifest.BlobBytes != validator.blobBytes {
		return validator.fail("package_integrity_failed", "transfer.json", "declared and observed blob inventory does not reconcile")
	}
	for _, relation := range []struct {
		name  string
		exact bool
	}{
		{"sources", true}, {"source_routes", false}, {"people", false}, {"person_uids", false},
		{"conversations", false}, {"records", false}, {"attachments", false},
		{"blobs", true}, {"coverage", false}, {"tombstones", false},
	} {
		if err := validator.validateRelation(relation.name, relation.exact, "records.jsonl", "package_integrity_failed"); err != nil {
			return err
		}
	}
	validator.report.Counts = validator.observed
	return nil
}

func (validator *packageValidator) inventory() error {
	return validator.reader.WalkFiles(validator.ctx, func(file PackageFile) error {
		if file.Size < 0 || file.Size > MaxExpandedBytes-validator.report.Bounds.ExpandedBytes {
			return validator.fail("bound_exceeded", "package", "expanded package exceeds its byte bound")
		}
		validator.report.Bounds.ExpandedBytes += file.Size
		switch {
		case file.Name == "transfer.json":
			validator.report.Bounds.ManifestBytes = file.Size
		case file.Name == "records.jsonl":
			validator.report.Bounds.RecordsBytes = file.Size
		case file.Name == "SHA256SUMS":
			return nil
		case strings.HasPrefix(file.Name, "blobs/"):
			if file.Size > MaxBlobBytes || validator.blobCount >= MaxBlobs || file.Size > MaxExpandedBytes-validator.blobBytes {
				return validator.fail("bound_exceeded", "blobs", "blob inventory exceeds its bound")
			}
			validator.blobCount++
			validator.blobBytes += file.Size
			validator.report.Bounds.Blobs = validator.blobCount
			validator.report.Bounds.BlobBytes = validator.blobBytes
		}
		if err := validator.index.relation("inventory").addDefinition(file.Name); err != nil {
			return err
		}
		return validator.index.relation("inventory_sizes").addDefinition(relationKey(file.Name, strconv.FormatInt(file.Size, 10)))
	})
}

func (validator *packageValidator) loadManifest() error {
	raw, err := readPackageFile(validator.ctx, validator.reader, "transfer.json", MaxManifestBytes)
	if err != nil {
		if ctxErr := contextError(err); ctxErr != nil {
			return ctxErr
		}
		return validator.fail("package_integrity_failed", "transfer.json", "manifest cannot be read")
	}
	if err := CheckStructuredKeys(raw); err != nil {
		return validator.fail(structuredFindingCode(err), "transfer.json", "manifest contains a forbidden or invalid structured key")
	}
	manifest, manifestChecksum, err := DecodeManifestV1(raw)
	if err != nil {
		return validator.fail(decodeFindingCode(err), "transfer.json", "manifest does not match the v1 contract")
	}
	validator.manifest = manifest
	validator.manifestChecksum = manifestChecksum
	validator.manifestBytes = int64(len(raw))
	validator.report.Format = manifest.Format
	validator.report.PackageID = manifest.PackageID
	validator.report.ArchiveID = manifest.Archive.ArchiveID
	var semanticErr *semanticFinding
	if validator.legacyEvidence != nil {
		semanticErr = validateLegacyManifest(manifest, *validator.legacyEvidence, validator.legacyBinding, validator.legacyLimitations)
	} else {
		semanticErr = validateManifest(manifest)
	}
	if semanticErr != nil {
		return validator.fail(semanticErr.code, "transfer.json", semanticErr.detail)
	}
	validator.selectionDigest, err = SelectionDigest(manifest.Selection)
	if err != nil {
		return validator.fail("package_integrity_failed", "transfer.json", "selection cannot be hashed")
	}
	for _, sourceRef := range manifest.Selection.Sources {
		if err := validator.index.relation("sources").addReference(sourceRef); err != nil {
			return err
		}
	}
	return nil
}

func (validator *packageValidator) verifyChecksums() error {
	raw, err := readPackageFile(validator.ctx, validator.reader, "SHA256SUMS", MaxChecksumBytes)
	if err != nil {
		if ctxErr := contextError(err); ctxErr != nil {
			return ctxErr
		}
		return validator.fail("package_integrity_failed", "SHA256SUMS", "checksum inventory cannot be read")
	}
	buffered := bufio.NewReader(strings.NewReader(string(raw)))
	previous := ""
	for {
		line, lineErr := ReadCanonicalLine(buffered)
		if errors.Is(lineErr, io.EOF) {
			break
		}
		if lineErr != nil || len(line) < 67 || string(line[64:66]) != "  " {
			return validator.fail("package_integrity_failed", "SHA256SUMS", "checksum framing is invalid")
		}
		digest, name := string(line[:64]), string(line[66:])
		if !canonical.IsSHA256Hex(digest) || name == "SHA256SUMS" || ValidatePackagePath(name) != nil || previous >= name {
			return validator.fail("package_integrity_failed", "SHA256SUMS", "checksum inventory is invalid")
		}
		previous = name
		if err := validator.index.relation("inventory").addReference(name); err != nil {
			return err
		}
		observed, observedSize, hashErr := hashPackageFile(validator.ctx, validator.reader, name, fileReadLimit(name))
		if ctxErr := contextError(hashErr); ctxErr != nil {
			return ctxErr
		}
		if hashErr != nil || observed != digest {
			return validator.fail("package_integrity_failed", name, "file bytes do not match the checksum inventory")
		}
		if name == "transfer.json" && (digest != validator.manifestChecksum || observedSize != validator.manifestBytes) {
			return validator.fail("package_integrity_failed", "transfer.json", "decoded manifest bytes do not match the checksum inventory")
		}
		if strings.HasPrefix(name, "blobs/") {
			pathDigest := name[strings.LastIndexByte(name, '/')+1:]
			if observed != pathDigest {
				return validator.fail("package_integrity_failed", "blobs", "blob content does not match its content-addressed name")
			}
			if err := validator.index.relation("blobs").addDefinition(relationKey(pathDigest, strconv.FormatInt(observedSize, 10))); err != nil {
				return err
			}
		}
		if err := validator.index.relation("inventory_sizes").addReference(relationKey(name, strconv.FormatInt(observedSize, 10))); err != nil {
			return err
		}
		if name == "records.jsonl" {
			validator.recordsChecksum = digest
		}
	}
	if previous == "" || validator.recordsChecksum == "" {
		return validator.fail("package_integrity_failed", "SHA256SUMS", "checksum inventory is incomplete")
	}
	return nil
}

func hashPackageFile(ctx context.Context, reader PackageReader, name string, limit int64) (string, int64, error) {
	file, declaredSize, err := reader.OpenFile(ctx, name)
	if err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return "", total, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			if int64(read) > limit-total {
				_ = file.Close()
				return "", total, errors.New("transfer: package file limit")
			}
			total += int64(read)
			_, _ = hasher.Write(buffer[:read])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = file.Close()
			return "", total, fmt.Errorf("transfer: read package file: %w", readErr)
		}
	}
	if err := file.Close(); err != nil {
		return "", total, fmt.Errorf("transfer: close package file: %w", err)
	}
	if total != declaredSize {
		return "", total, errors.New("transfer: package file size changed")
	}
	return hex.EncodeToString(hasher.Sum(nil)), total, nil
}

func fileReadLimit(name string) int64 {
	switch name {
	case "transfer.json":
		return MaxManifestBytes
	case "SHA256SUMS":
		return MaxChecksumBytes
	default:
		if strings.HasPrefix(name, "blobs/") {
			return MaxBlobBytes
		}
		return MaxExpandedBytes
	}
}

func (validator *packageValidator) visitRecord(number int, raw []byte) error {
	if int64(len(raw)+1) > MaxExpandedBytes-validator.recordsBytes {
		return validator.fail("bound_exceeded", "records.jsonl", "record stream exceeds its byte bound")
	}
	validator.recordsBytes += int64(len(raw) + 1)
	validator.report.Bounds.RecordLines = int64(number)
	_, _ = validator.recordsWhole.Write(raw)
	_, _ = validator.recordsWhole.Write([]byte{'\n'})
	if err := CheckStructuredKeys(raw); err != nil {
		return validator.fail(structuredFindingCode(err), recordPath(number), "record contains a forbidden or invalid structured key")
	}
	if validator.terminal != nil {
		return validator.fail("incomplete_package", recordPath(number), "data follows the terminal record")
	}
	header, err := decodeRecordHeader(raw)
	if err != nil {
		return validator.fail("package_integrity_failed", recordPath(number), "record header is invalid")
	}
	if !ValidRecordType(header.RecordType) {
		return validator.fail("unsupported_record_kind", recordPath(number), "record type is unsupported")
	}
	phase := recordPhase(header.RecordType)
	if phase < validator.phase {
		return validator.fail("package_integrity_failed", recordPath(number), "record phase order is invalid")
	}
	validator.phase = phase
	if header.RecordType != RecordTypeComplete {
		_, _ = validator.recordsPrefix.Write(raw)
		_, _ = validator.recordsPrefix.Write([]byte{'\n'})
	}
	switch header.RecordType {
	case RecordTypePerson:
		value, _, decodeErr := DecodePersonV1(raw)
		if decodeErr != nil {
			return validator.decodeFailure(number, decodeErr)
		}
		if semanticErr := validator.validatePerson(value); semanticErr != nil {
			return validator.semanticFailure(number, semanticErr)
		}
		validator.observed.Person++
	case RecordTypeSource:
		value, _, decodeErr := DecodeSourceLineV1(raw)
		if decodeErr != nil {
			return validator.decodeFailure(number, decodeErr)
		}
		if semanticErr := validator.validateSource(value); semanticErr != nil {
			return validator.semanticFailure(number, semanticErr)
		}
		validator.observed.Source++
	case RecordTypeConversation:
		value, _, decodeErr := DecodeConversationV1(raw)
		if decodeErr != nil {
			return validator.decodeFailure(number, decodeErr)
		}
		if semanticErr := validator.validateConversation(value); semanticErr != nil {
			return validator.semanticFailure(number, semanticErr)
		}
		validator.observed.Conversation++
	case RecordTypeRecord:
		value, _, decodeErr := DecodeRecordV1(raw)
		if decodeErr != nil {
			return validator.decodeFailure(number, decodeErr)
		}
		if semanticErr := validator.validateRecord(value); semanticErr != nil {
			return validator.semanticFailure(number, semanticErr)
		}
		if value.Kind == KindAttachmentOccurrence {
			validator.observed.AttachmentOccurrence++
		} else {
			validator.observed.Record++
		}
	case RecordTypeCoverage:
		value, _, decodeErr := DecodeCoverageV1(raw)
		if decodeErr != nil {
			return validator.decodeFailure(number, decodeErr)
		}
		if semanticErr := validator.validateCoverage(value); semanticErr != nil {
			return validator.semanticFailure(number, semanticErr)
		}
		validator.observed.Coverage++
	case RecordTypeTombstone:
		value, _, decodeErr := DecodeTombstoneV1(raw)
		if decodeErr != nil {
			return validator.decodeFailure(number, decodeErr)
		}
		if semanticErr := validator.validateTombstone(value); semanticErr != nil {
			return validator.semanticFailure(number, semanticErr)
		}
		validator.observed.Tombstone++
	case RecordTypeComplete:
		value, _, decodeErr := DecodeCompleteV1(raw)
		if decodeErr != nil {
			return validator.decodeFailure(number, decodeErr)
		}
		if value.RecordType != RecordTypeComplete {
			return validator.fail("unsupported_record_kind", recordPath(number), "terminal record type is invalid")
		}
		validator.terminal = &value
	}
	return nil
}

func (validator *packageValidator) validatePerson(person PersonV1) *semanticFinding {
	if !validator.manifest.Selection.IncludePersonRecords {
		return invalidSemantic("package_integrity_failed", "person records were not selected")
	}
	if person.RecordType != RecordTypePerson || person.PersonRef == "" || len(person.PersonRef) > MaxPersonUIDBytes || person.UIDKind != PersonUIDKindVCard || len(person.DisplayName) > MaxNameBytes || len(person.PersonLocalID) > MaxPersonHintBytes || len(person.Revision) > MaxPersonHintBytes || len(person.IdentityRevision) > MaxPersonHintBytes || len(person.ContactPoints) > MaxContactPoints {
		return invalidSemantic("package_integrity_failed", "person record is invalid")
	}
	if validator.manifest.Selection.PersonFieldPolicy == "identity_only" && len(person.ContactPoints) > 0 {
		return invalidSemantic("package_integrity_failed", "person contact points exceed the selected policy")
	}
	if err := validatePersonFields(person); err != nil {
		return invalidSemantic("unknown_field", "person field declaration is invalid")
	}
	if !uniqueBoundedStrings(person.RetiredUIDs, MaxPersonUIDBytes) || !uniqueBoundedStrings(person.ParticipantRefs, MaxRecordRefBytes) {
		return invalidSemantic("bound_exceeded", "person reference exceeds its bound")
	}
	for _, point := range person.ContactPoints {
		if !ValidContactPointKind(point.Kind) || point.ValueNormalized == "" || len(point.ValueNormalized) > MaxContactPointBytes || len(point.ValueDisplay) > MaxContactPointBytes || len(point.ServiceID) > MaxContactPointBytes || len(point.ScopeKind) > 64 || len(point.ScopeValue) > MaxContactPointBytes || point.ScopeKind == "" != (point.ScopeValue == "") || point.Pref != nil && (*point.Pref < 0 || *point.Pref > 1_000_000) {
			return invalidSemantic("unsupported_enum_value", "person contact point is invalid")
		}
	}
	if err := validator.index.relation("people").addDefinition(person.PersonRef); err != nil {
		return spoolSemantic(err)
	}
	if err := validator.index.relation("person_uids").addDefinition(person.PersonRef); err != nil {
		return spoolSemantic(err)
	}
	for _, retiredUID := range person.RetiredUIDs {
		if err := validator.index.relation("person_uids").addDefinition(retiredUID); err != nil {
			return spoolSemantic(err)
		}
	}
	return nil
}

func (validator *packageValidator) validateSource(source SourceLineV1) *semanticFinding {
	if source.RecordType != RecordTypeSource || source.SourceRef == "" || len(source.SourceRef) > MaxSourceRefBytes || !ValidSourceType(source.SourceType) || source.Route == "" || len(source.Route) > 64 || source.Identifier == "" || len(source.Identifier) > 4096 || len(source.DisplayName) > MaxNameBytes || !validSourceRoute(source.SourceType, source.Route) {
		return invalidSemantic("unsupported_enum_value", "source record is invalid")
	}
	if source.SourceType == SourceTypeOther {
		if source.SourceTypeRaw == "" || len(source.SourceTypeRaw) > MaxSourceTypeRawBytes || !sourceTypeRawPattern.MatchString(source.SourceTypeRaw) {
			return invalidSemantic("unsupported_enum_value", "raw source type is invalid")
		}
	} else if source.SourceTypeRaw != "" {
		return invalidSemantic("unknown_field", "raw source type is only valid for other sources")
	}
	if semanticErr := validator.validateCustodian(source.Custodian); semanticErr != nil {
		return semanticErr
	}
	if err := validator.index.relation("sources").addDefinition(source.SourceRef); err != nil {
		return spoolSemantic(err)
	}
	if err := validator.index.relation("source_routes").addDefinition(relationKey(source.SourceRef, source.Route)); err != nil {
		return spoolSemantic(err)
	}
	return nil
}

func (validator *packageValidator) validateConversation(conversation ConversationV1) *semanticFinding {
	if conversation.RecordType != RecordTypeConversation || conversation.SourceRef == "" || len(conversation.SourceRef) > MaxSourceRefBytes || conversation.ConversationRef == "" || len(conversation.ConversationRef) > MaxRecordRefBytes || len(conversation.Title) > MaxBodyTextBytes || !slices.Contains(conversationTypes, conversation.ConversationType) || len(conversation.ParentConversationRef) > MaxRecordRefBytes || len(conversation.ThreadRootRecordRef) > MaxRecordRefBytes {
		return invalidSemantic("package_integrity_failed", "conversation record is invalid")
	}
	if err := validator.index.relation("sources").addReference(conversation.SourceRef); err != nil {
		return spoolSemantic(err)
	}
	if err := validator.index.relation("conversations").addDefinition(relationKey(conversation.SourceRef, conversation.ConversationRef)); err != nil {
		return spoolSemantic(err)
	}
	return nil
}

func (validator *packageValidator) validateRecord(record RecordV1) *semanticFinding {
	if record.RecordType != RecordTypeRecord || !ValidKind(record.Kind) || record.Kind == KindConversation {
		return invalidSemantic("unsupported_record_kind", "record kind is unsupported")
	}
	if !slices.Contains(validator.manifest.Selection.Kinds, record.Kind) {
		return invalidSemantic("package_integrity_failed", "record kind is outside the exact selection")
	}
	if record.RecordRef == "" || len(record.RecordRef) > MaxRecordRefBytes || record.SourceRef == "" || len(record.SourceRef) > MaxSourceRefBytes || len(record.ConversationRef) > MaxRecordRefBytes || len(record.ParentRecordRef) > MaxRecordRefBytes || len(record.Revision) > MaxRecordRefBytes || len(record.BodyText) > MaxBodyTextBytes || len(record.Dates) > MaxDatesPerRecord || len(record.Participants) > MaxParticipantsPerRecord || len(record.Attachments) > MaxAttachmentsPerRecord {
		return invalidSemantic("bound_exceeded", "record exceeds a v1 bound")
	}
	if !ValidRecordOrigin(record.RecordOrigin) || record.Direction != "" && !ValidDirection(record.Direction) {
		return invalidSemantic("unsupported_enum_value", "record vocabulary is invalid")
	}
	if record.Raw == nil && record.RecordOrigin != RecordOriginProducerCanonical || record.Raw != nil && record.RecordOrigin != RecordOriginProviderOriginal {
		return invalidSemantic("package_integrity_failed", "record origin does not match raw evidence")
	}
	if record.Raw != nil {
		if !validator.manifest.Selection.IncludeRawSource {
			return invalidSemantic("package_integrity_failed", "raw evidence was not selected")
		}
		if semanticErr := validator.validateBlobRef(*record.Raw); semanticErr != nil {
			return semanticErr
		}
	}
	if validator.legacyEvidence != nil {
		if record.LegacyRef == "" || record.LegacyRef != record.RecordRef || record.Direction != "" ||
			record.Raw != nil || len(record.Attachments) != 0 || record.SourceFields != nil ||
			record.History == nil || record.History.Edits != HistoryStateUnsupported ||
			record.History.Reactions != HistoryStateUnsupported || record.History.Deletions != HistoryStateUnsupported ||
			len(record.Dates) != 1 {
			return invalidSemantic("package_integrity_failed", "legacy record authority is invalid")
		}
	}
	seenDates := make([]DateKind, 0, len(record.Dates))
	for _, date := range record.Dates {
		dateErr := ValidateDate(date)
		if validator.legacyEvidence != nil && date.Kind == "" {
			dateErr = validateLegacyOrderingDate(date)
		}
		if slices.Contains(seenDates, date.Kind) || dateErr != nil {
			return invalidSemantic("unsupported_enum_value", "record date is invalid")
		}
		seenDates = append(seenDates, date.Kind)
	}
	for _, participant := range record.Participants {
		if !ValidEnvelopeRole(participant.EnvelopeRole) || !ValidScopeDirection(participant.ScopeDirection) || len(participant.DisplayName) > MaxNameBytes || len(participant.Address) > MaxContactPointBytes || participant.Address == "" != (participant.AddressKind == "") || participant.AddressKind != "" && !ValidContactPointKind(participant.AddressKind) || len(participant.PersonRef) > MaxPersonUIDBytes {
			return invalidSemantic("unsupported_enum_value", "record participant is invalid")
		}
		if participant.PersonRef != "" {
			if validator.legacyEvidence != nil {
				return invalidSemantic("package_integrity_failed", "legacy participant has unsupported person authority")
			}
			if err := validator.index.relation("people").addReference(participant.PersonRef); err != nil {
				return spoolSemantic(err)
			}
		}
	}
	if semanticErr := validator.validateCustodian(record.Custodian); semanticErr != nil {
		return semanticErr
	}
	if record.History != nil && (!ValidHistoryState(record.History.Edits) || !ValidHistoryState(record.History.Reactions) || !ValidHistoryState(record.History.Deletions)) {
		return invalidSemantic("unsupported_enum_value", "record history state is invalid")
	}
	if err := ValidateSourceFields(record.Kind, record.SourceFields); err != nil {
		return invalidSemantic(sourceFieldsFindingCode(err), "record source fields are invalid")
	}
	wantNormalized, err := NormalizedRecordSHA256(record)
	if err != nil || !canonical.IsSHA256Hex(record.NormalizedSHA256) || record.NormalizedSHA256 != wantNormalized {
		return invalidSemantic("package_integrity_failed", "normalized record digest is invalid")
	}
	if record.Kind == KindAttachmentOccurrence {
		if record.Attachment == nil {
			return invalidSemantic("package_integrity_failed", "attachment occurrence metadata is missing")
		}
		if semanticErr := validator.validateAttachment(record.SourceRef, *record.Attachment); semanticErr != nil {
			return semanticErr
		}
		if err := validator.index.relation("attachments").addDefinition(relationKey(record.SourceRef, record.RecordRef)); err != nil {
			return spoolSemantic(err)
		}
	} else if record.Attachment != nil {
		return invalidSemantic("unknown_field", "attachment metadata is invalid for this record kind")
	}
	for _, attachmentRef := range record.Attachments {
		if attachmentRef == "" || len(attachmentRef) > MaxRecordRefBytes {
			return invalidSemantic("bound_exceeded", "attachment reference exceeds its bound")
		}
		if err := validator.index.relation("attachments").addReference(relationKey(record.SourceRef, attachmentRef)); err != nil {
			return spoolSemantic(err)
		}
	}
	if err := validator.index.relation("sources").addReference(record.SourceRef); err != nil {
		return spoolSemantic(err)
	}
	if record.ConversationRef != "" {
		if err := validator.index.relation("conversations").addReference(relationKey(record.SourceRef, record.ConversationRef)); err != nil {
			return spoolSemantic(err)
		}
	}
	if err := validator.index.relation("records").addDefinition(relationKey(record.SourceRef, record.RecordRef)); err != nil {
		return spoolSemantic(err)
	}
	return nil
}

func (validator *packageValidator) validateAttachment(sourceRef string, attachment AttachmentOccurrenceV1) *semanticFinding {
	if attachment.PartKey == "" || len(attachment.PartKey) > MaxRecordRefBytes || len(attachment.Filename) > 4096 || len(attachment.MediaType) > 255 || attachment.Size < 0 || attachment.Size > MaxBlobBytes || !canonical.IsSHA256Hex(attachment.ContentSHA256) || !ValidAttachmentRole(attachment.Role) || !ValidAvailability(attachment.Availability) {
		return invalidSemantic("unsupported_enum_value", "attachment occurrence is invalid")
	}
	if attachment.Availability == AvailabilityBytesIncluded {
		if !validator.manifest.Selection.IncludeAttachmentBytes || attachment.AvailabilityReason != "" || attachment.Blob == nil || attachment.Blob.BlobSHA256 != attachment.ContentSHA256 || attachment.Blob.Size != attachment.Size {
			return invalidSemantic("package_integrity_failed", "included attachment bytes are inconsistent")
		}
		if semanticErr := validator.validateBlobRef(*attachment.Blob); semanticErr != nil {
			return semanticErr
		}
	} else if !ValidReasonCode(attachment.AvailabilityReason) || attachment.Blob != nil {
		return invalidSemantic("unsupported_enum_value", "attachment availability reason is invalid")
	}
	_ = sourceRef
	return nil
}

func (validator *packageValidator) validateBlobRef(blob BlobRefV1) *semanticFinding {
	if !canonical.IsSHA256Hex(blob.BlobSHA256) || blob.Size < 0 || blob.Size > MaxBlobBytes || blob.MediaType == "" || len(blob.MediaType) > 255 {
		return invalidSemantic("package_integrity_failed", "blob reference is invalid")
	}
	if err := validator.index.relation("blobs").addReference(relationKey(blob.BlobSHA256, strconv.FormatInt(blob.Size, 10))); err != nil {
		return spoolSemantic(err)
	}
	return nil
}

func (validator *packageValidator) validateCoverage(coverage CoverageV1) *semanticFinding {
	if !ValidKind(coverage.Kind) {
		return invalidSemantic("unsupported_record_kind", "coverage kind is unsupported")
	}
	if !slices.Contains(validator.manifest.Selection.Kinds, coverage.Kind) {
		return invalidSemantic("package_integrity_failed", "coverage kind is outside the exact selection")
	}
	if coverage.RecordType != RecordTypeCoverage || coverage.SourceRef == "" || len(coverage.SourceRef) > MaxSourceRefBytes || coverage.Route == "" || len(coverage.Route) > 64 || coverage.SelectionDigest != validator.selectionDigest || !canonical.IsSHA256Hex(coverage.SelectionDigest) {
		return invalidSemantic("unsupported_enum_value", "coverage identity is invalid")
	}
	values := []int64{coverage.Selected, coverage.Emitted, coverage.Skipped, coverage.RawIncluded, coverage.RawAbsent, coverage.AttachmentsSelected, coverage.AttachmentsBytesIncluded, coverage.AttachmentsOmitted}
	for _, value := range values {
		if value < 0 {
			return invalidSemantic("package_integrity_failed", "coverage count is negative")
		}
	}
	if sum, ok := checkedSum(coverage.Emitted, coverage.Skipped); !ok || coverage.Selected != sum {
		return invalidSemantic("package_integrity_failed", "coverage selected counts do not reconcile")
	}
	if sum, ok := checkedSum(coverage.RawIncluded, coverage.RawAbsent); !ok || coverage.Emitted != sum {
		return invalidSemantic("package_integrity_failed", "coverage raw counts do not reconcile")
	}
	if sum, ok := checkedSum(coverage.AttachmentsBytesIncluded, coverage.AttachmentsOmitted); !ok || coverage.AttachmentsSelected != sum {
		return invalidSemantic("package_integrity_failed", "coverage attachment counts do not reconcile")
	}
	seenReasons := make([]ReasonCode, 0, len(coverage.Reasons))
	for _, reason := range coverage.Reasons {
		if !ValidReasonCode(reason.Code) || reason.Count < 0 || slices.Contains(seenReasons, reason.Code) {
			return invalidSemantic("unsupported_enum_value", "coverage reason is invalid")
		}
		seenReasons = append(seenReasons, reason.Code)
	}
	if err := ValidateCapabilityEntries(coverage.Capabilities); err != nil {
		return invalidSemantic("unsupported_enum_value", "coverage capability is invalid")
	}
	if err := validator.index.relation("sources").addReference(coverage.SourceRef); err != nil {
		return spoolSemantic(err)
	}
	if err := validator.index.relation("source_routes").addReference(relationKey(coverage.SourceRef, coverage.Route)); err != nil {
		return spoolSemantic(err)
	}
	if err := validator.index.relation("coverage").addDefinition(relationKey(coverage.SourceRef, coverage.Route, string(coverage.Kind), coverage.SelectionDigest)); err != nil {
		return spoolSemantic(err)
	}
	return nil
}

func (validator *packageValidator) validateTombstone(tombstone TombstoneV1) *semanticFinding {
	if !ValidKind(tombstone.Kind) {
		return invalidSemantic("unsupported_record_kind", "tombstone kind is unsupported")
	}
	if !slices.Contains(validator.manifest.Selection.Kinds, tombstone.Kind) {
		return invalidSemantic("package_integrity_failed", "tombstone kind is outside the exact selection")
	}
	if tombstone.RecordType != RecordTypeTombstone || tombstone.SourceRef == "" || len(tombstone.SourceRef) > MaxSourceRefBytes || tombstone.RecordRef == "" || len(tombstone.RecordRef) > MaxRecordRefBytes || !slices.Contains(tombstoneReasons, tombstone.Reason) || !validUTCNano(tombstone.ObservedAt) {
		return invalidSemantic("unsupported_enum_value", "tombstone record is invalid")
	}
	if err := validator.index.relation("sources").addReference(tombstone.SourceRef); err != nil {
		return spoolSemantic(err)
	}
	if err := validator.index.relation("tombstones").addDefinition(relationKey(tombstone.SourceRef, string(tombstone.Kind), tombstone.RecordRef)); err != nil {
		return spoolSemantic(err)
	}
	return nil
}

func (validator *packageValidator) validateCustodian(custodian *CustodianV1) *semanticFinding {
	if custodian == nil {
		return nil
	}
	if len(custodian.RawLabel) > MaxNameBytes || !ValidCustodianRank(custodian.Rank) || len(custodian.PersonRef) > MaxPersonUIDBytes {
		return invalidSemantic("unsupported_enum_value", "custodian record is invalid")
	}
	if custodian.PersonRef != "" {
		if err := validator.index.relation("people").addReference(custodian.PersonRef); err != nil {
			return spoolSemantic(err)
		}
	}
	return nil
}

func (validator *packageValidator) validateRelation(name string, exact bool, path, code string) error {
	result, err := validator.index.relation(name).validate(validator.ctx, exact)
	if err != nil {
		return err
	}
	if result.Duplicate || result.Missing || result.Unexpected {
		return validator.fail(code, path, "package references or identities do not reconcile")
	}
	return nil
}

func (validator *packageValidator) decodeFailure(number int, err error) error {
	return validator.fail(decodeFindingCode(err), recordPath(number), "record does not match the v1 contract")
}

func (validator *packageValidator) semanticFailure(number int, finding *semanticFinding) error {
	return validator.fail(finding.code, recordPath(number), finding.detail)
}

var errAdmissionFinding = errors.New("transfer: admission finding")

func (validator *packageValidator) fail(code, path, detail string) error {
	validator.report.AddFinding(Finding{Severity: "error", Code: code, Path: path, Detail: detail})
	return errAdmissionFinding
}

type semanticFinding struct {
	code   string
	detail string
	err    error
}

func invalidSemantic(code, detail string) *semanticFinding {
	return &semanticFinding{code: code, detail: detail}
}

func spoolSemantic(err error) *semanticFinding {
	return &semanticFinding{code: "package_integrity_failed", detail: "validation index failed", err: err}
}

func validateManifest(manifest ManifestV1) *semanticFinding {
	if manifest.Format != FormatV1 {
		return invalidSemantic("unsupported_record_kind", "transfer format is unsupported")
	}
	if manifest.Producer.ContractRevision != ContractRevision {
		return invalidSemantic("unsupported_contract_revision", "producer contract revision is unsupported")
	}
	if !validPackageUUID(manifest.PackageID) {
		return invalidSemantic("package_integrity_failed", "package identity is invalid")
	}
	if len(manifest.ExportSequence) != 20 || !decimalPattern.MatchString(manifest.ExportSequence) || manifest.Producer.Name == "" || len(manifest.Producer.Name) > 128 || manifest.Producer.Version == "" || len(manifest.Producer.Version) > 128 || !validArchiveID(manifest.Archive.ArchiveID) || manifest.Archive.System != "msgvault" || len(manifest.Archive.DisplayName) > MaxNameBytes || !validUTCNano(manifest.CreatedAt) || !canonical.IsSHA256Hex(manifest.RecordsSHA256) {
		return invalidSemantic("package_integrity_failed", "manifest identity or digest is invalid")
	}
	if manifest.Snapshot.Consistency != "single_read_transaction" && manifest.Snapshot.Consistency != "streamed_watermark" || manifest.Snapshot.SpineTimezone == "" || len(manifest.Snapshot.SpineTimezone) > 128 || manifest.Snapshot.SpineGeneration < 0 || !validUTCNano(manifest.Snapshot.HighWatermark) || !validUTCNano(manifest.Snapshot.ReadStartedAt) || !validUTCNano(manifest.Snapshot.ReadCompletedAt) {
		return invalidSemantic("package_integrity_failed", "snapshot evidence is invalid")
	}
	started, _ := time.Parse(time.RFC3339Nano, manifest.Snapshot.ReadStartedAt)
	completed, _ := time.Parse(time.RFC3339Nano, manifest.Snapshot.ReadCompletedAt)
	if completed.Before(started) {
		return invalidSemantic("package_integrity_failed", "snapshot timing is invalid")
	}
	return validateManifestBody(manifest)
}

func validateManifestBody(manifest ManifestV1) *semanticFinding {
	if manifest.BlobCount < 0 || manifest.BlobCount > MaxBlobs || manifest.BlobBytes < 0 || manifest.BlobBytes > MaxExpandedBytes || countsInvalid(manifest.Counts) {
		return invalidSemantic("bound_exceeded", "manifest count exceeds a package bound")
	}
	if manifest.Selection.PersonFieldPolicy != "identity_only" && manifest.Selection.PersonFieldPolicy != "identity_and_contact_points" {
		return invalidSemantic("unsupported_enum_value", "person field policy is unsupported")
	}
	start, startErr := time.Parse(time.RFC3339, manifest.Selection.Window.Start)
	end, endErr := time.Parse(time.RFC3339, manifest.Selection.Window.End)
	if startErr != nil || endErr != nil || !start.Before(end) {
		return invalidSemantic("package_integrity_failed", "selection window is invalid")
	}
	if !uniqueBoundedStrings(manifest.Selection.Sources, MaxSourceRefBytes) {
		return invalidSemantic("package_integrity_failed", "selection sources are invalid")
	}
	for _, kind := range manifest.Selection.Kinds {
		if !ValidKind(kind) {
			return invalidSemantic("unsupported_record_kind", "selection kind is unsupported")
		}
	}
	if !uniqueKinds(manifest.Selection.Kinds) {
		return invalidSemantic("package_integrity_failed", "selection kinds are duplicated")
	}
	for _, excluded := range manifest.Selection.Excluded {
		if excluded.Kind != "" && !ValidKind(excluded.Kind) {
			return invalidSemantic("unsupported_record_kind", "excluded selection kind is unsupported")
		}
		if excluded.SourceRef == "" && excluded.Kind == "" || len(excluded.SourceRef) > MaxSourceRefBytes || !ValidReasonCode(excluded.Reason) {
			return invalidSemantic("unsupported_enum_value", "selection exclusion is invalid")
		}
	}
	return nil
}

func validatePersonFields(person PersonV1) error {
	allowed := []string{"display_name", "retired_uids", "contact_points", "participant_refs"}
	selected := slices.Clone(person.SelectedFields)
	withheld := slices.Clone(person.WithheldFields)
	if !allUniqueAllowed(selected, allowed) || !allUniqueAllowed(withheld, allowed) {
		return errors.New("invalid field declaration")
	}
	for _, field := range selected {
		if slices.Contains(withheld, field) {
			return errors.New("field is both selected and withheld")
		}
	}
	supplied := map[string]bool{
		"display_name":     person.DisplayName != "",
		"retired_uids":     len(person.RetiredUIDs) > 0,
		"contact_points":   len(person.ContactPoints) > 0,
		"participant_refs": len(person.ParticipantRefs) > 0,
	}
	for field, present := range supplied {
		if present && (!slices.Contains(selected, field) || slices.Contains(withheld, field)) {
			return errors.New("supplied person field is outside its declaration")
		}
	}
	return nil
}

func contextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func allUniqueAllowed(values, allowed []string) bool {
	seen := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(allowed, value) || slices.Contains(seen, value) {
			return false
		}
		seen = append(seen, value)
	}
	return true
}

func decodeRecordHeader(raw []byte) (struct {
	RecordType RecordType `json:"record_type"`
}, error) {
	var header struct {
		RecordType RecordType `json:"record_type"`
	}
	err := json.Unmarshal(raw, &header)
	return header, err
}

func decodeFindingCode(err error) string {
	if strings.Contains(strings.ToLower(err.Error()), "unknown") {
		return "unknown_field"
	}
	return "package_integrity_failed"
}

func sourceFieldsFindingCode(err error) string {
	if errors.Is(err, errUnknownSourceField) {
		return "unknown_field"
	}
	return "package_integrity_failed"
}

func structuredFindingCode(err error) string {
	switch {
	case errors.Is(err, errForbiddenStructuredKey):
		return "forbidden_field"
	case errors.Is(err, errStructuredDepth):
		return "bound_exceeded"
	default:
		return "package_integrity_failed"
	}
}

func recordPhase(recordType RecordType) int {
	switch recordType {
	case RecordTypePerson:
		return 0
	case RecordTypeSource:
		return 1
	case RecordTypeConversation:
		return 2
	case RecordTypeRecord:
		return 3
	case RecordTypeCoverage:
		return 4
	case RecordTypeTombstone:
		return 5
	case RecordTypeComplete:
		return 6
	default:
		return 7
	}
}

func recordPath(number int) string { return fmt.Sprintf("records.jsonl:%d", number) }

func relationKey(parts ...string) string {
	raw, err := canonical.Marshal(parts)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func countsInvalid(counts CountsV1) bool {
	values := []int64{counts.Person, counts.Source, counts.Conversation, counts.Record, counts.AttachmentOccurrence, counts.Coverage, counts.Tombstone}
	var total int64
	for _, value := range values {
		if value < 0 || value > MaxRecordLines-total {
			return true
		}
		total += value
	}
	return total >= MaxRecordLines
}

func checkedSum(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left > int64(^uint64(0)>>1)-right {
		return 0, false
	}
	return left + right, true
}

func validUTCNano(value string) bool {
	parsed, err := time.Parse("2006-01-02T15:04:05.000000000Z", value)
	return err == nil && parsed.Format("2006-01-02T15:04:05.000000000Z") == value
}

func validArchiveID(value string) bool {
	return value != "" && len(value) <= MaxArchiveIDBytes && archiveIDPattern.MatchString(value)
}

func validPackageUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	return isLowerHex(value[:8]) && isLowerHex(value[9:13]) && isLowerHex(value[14:18]) && isLowerHex(value[19:23]) && isLowerHex(value[24:])
}

func uniqueBoundedStrings(values []string, limit int) bool {
	cloned := slices.Clone(values)
	slices.Sort(cloned)
	for index, value := range cloned {
		if value == "" || len(value) > limit || index > 0 && cloned[index-1] == value {
			return false
		}
	}
	return true
}

func uniqueKinds(values []Kind) bool {
	cloned := slices.Clone(values)
	slices.Sort(cloned)
	for index, value := range cloned {
		if !ValidKind(value) || index > 0 && cloned[index-1] == value {
			return false
		}
	}
	return true
}

func validSourceRoute(sourceType SourceType, route string) bool {
	if sourceType == SourceTypeOther {
		return sourceTypeRawPattern.MatchString(route)
	}
	for _, row := range sourceQualificationRows {
		if row.sourceType == sourceType && row.route == route {
			return true
		}
	}
	return false
}

var (
	decimalPattern       = regexp.MustCompile(`^[0-9]{20}$`)
	archiveIDPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	sourceTypeRawPattern = regexp.MustCompile(`^[a-z0-9_.-]+$`)
	conversationTypes    = []string{"email_thread", "channel", "thread", "direct_chat", "group_chat", "meeting", "calendar", "other"}
	tombstoneReasons     = []string{"deleted_from_source", "deselected", "visibility_revoked"}
)
