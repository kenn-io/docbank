package transfer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	LegacyExportFormat        = "msgvault-message-export/1"
	LegacyCompatibilityFormat = "msgvault-transfer-legacy/1"
	legacyAdapterName         = "docbank-legacy-adapter"
	legacyAdapterVersion      = "1"
)

// ErrLegacyArchiveRequired reports that legacy input cannot supply its own
// archive identity.
var ErrLegacyArchiveRequired = errors.New("transfer: a registered archive ID is required for legacy input")

// PackageAuthority describes whether a package carries native producer
// authority or a reduced legacy compatibility representation.
type PackageAuthority string

const (
	PackageAuthorityFullV1              PackageAuthority = "full_v1"
	PackageAuthorityLegacyCompatibility PackageAuthority = "legacy_compatibility"
)

// LegacyArchiveBinding supplies the registered archive identity absent from a
// msgvault-message-export/1 stream. This package validates the ID shape but
// cannot verify registration in a Docbank vault.
type LegacyArchiveBinding struct {
	ArchiveID   string
	DisplayName string
}

// LegacyEvidence identifies the exact input bytes retained by the adapter.
type LegacyEvidence struct {
	Format string `json:"format"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// LegacyEvidenceReader exposes both a normalized compatibility package and
// the exact legacy bytes from which it was derived.
type LegacyEvidenceReader interface {
	PackageReader
	LegacyEvidence() LegacyEvidence
	LegacyLimitations() []CapabilityEntryV1
	OpenLegacyEvidence(ctx context.Context) (io.ReadCloser, error)
}

func legacyFormatLimitations() []CapabilityEntryV1 {
	return []CapabilityEntryV1{
		{Capability: CapabilityRaw, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoRaw},
		{Capability: CapabilityAttachmentBytes, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoAttachments},
		{Capability: CapabilityPeople, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoPersonUID},
		{Capability: CapabilityHistory, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoHistory},
	}
}

// ReadLegacyExport is retained as a fail-closed entry point. Legacy input has
// no archive identity, so callers must use ReadLegacyExportWithArchive.
func ReadLegacyExport(context.Context, io.Reader) (PackageReader, error) {
	return nil, ErrLegacyArchiveRequired
}

// ReadLegacyExportWithArchive validates and normalizes a legacy export while
// binding it to an explicitly supplied archive identity.
func ReadLegacyExportWithArchive(
	ctx context.Context,
	input io.Reader,
	binding LegacyArchiveBinding,
) (result LegacyEvidenceReader, resultErr error) {
	if input == nil {
		return nil, errors.New("transfer: legacy input is required")
	}
	if !validArchiveID(binding.ArchiveID) || len(binding.DisplayName) > MaxNameBytes {
		return nil, ErrLegacyArchiveRequired
	}
	root, err := os.MkdirTemp("", "docbank-transfer-legacy-")
	if err != nil {
		return nil, errors.New("transfer: create legacy compatibility spool")
	}
	cleanup := true
	defer func() {
		if cleanup {
			if err := os.RemoveAll(root); err != nil {
				resultErr = errors.Join(resultErr, errors.New("transfer: remove legacy compatibility spool"))
			}
		}
	}()

	packageDirectory := filepath.Join(root, "package")
	if err := os.Mkdir(packageDirectory, 0o700); err != nil {
		return nil, errors.New("transfer: create legacy compatibility package")
	}
	normalizer, err := newLegacyNormalizer(ctx, root, packageDirectory, binding)
	if err != nil {
		return nil, err
	}
	if err := normalizer.run(input); err != nil {
		return nil, errors.Join(err, normalizer.close())
	}
	if err := normalizer.close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reader, err := OpenDirectory(packageDirectory)
	if err != nil {
		return nil, errors.New("transfer: open normalized legacy package")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, reader.Close())
	}
	result = &legacyPackageReader{
		PackageReader: reader,
		root:          root,
		evidencePath:  normalizer.evidencePath,
		evidence:      normalizer.evidence,
		binding:       binding,
		limitations:   legacyFormatLimitations(),
	}
	cleanup = false
	return result, nil
}

// LegacyRecordRef derives the stable compatibility identity for one exact
// legacy source tuple.
func LegacyRecordRef(sourceType, identifier, id string) (string, error) {
	if sourceType == "" || identifier == "" || id == "" ||
		len(sourceType) > MaxSourceTypeRawBytes || len(identifier) > 4096 || len(id) > MaxRecordRefBytes {
		return "", errors.New("transfer: invalid legacy record identity")
	}
	raw, err := canonical.Marshal([]string{LegacyExportFormat, sourceType, identifier, id})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type legacyPackageReader struct {
	PackageReader

	root         string
	evidencePath string
	evidence     LegacyEvidence
	binding      LegacyArchiveBinding
	limitations  []CapabilityEntryV1
	closeOnce    sync.Once
	closeErr     error
}

func (reader *legacyPackageReader) LegacyEvidence() LegacyEvidence { return reader.evidence }

func (reader *legacyPackageReader) LegacyLimitations() []CapabilityEntryV1 {
	return slices.Clone(reader.limitations)
}

func (reader *legacyPackageReader) OpenLegacyEvidence(ctx context.Context) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Clean(reader.evidencePath))
	if err != nil {
		return nil, errors.New("transfer: open legacy evidence")
	}
	return file, nil
}

func (reader *legacyPackageReader) Close() error {
	reader.closeOnce.Do(func() {
		reader.closeErr = reader.PackageReader.Close()
		if err := os.RemoveAll(reader.root); err != nil {
			reader.closeErr = errors.Join(reader.closeErr, errors.New("transfer: remove legacy compatibility spool"))
		}
	})
	return reader.closeErr
}

func (reader *legacyPackageReader) legacyAuthority() (LegacyEvidence, LegacyArchiveBinding, []CapabilityEntryV1) {
	return reader.evidence, reader.binding, slices.Clone(reader.limitations)
}

func (reader *legacyPackageReader) verifyLegacyEvidence(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(filepath.Clean(reader.evidencePath))
	if err != nil {
		return errors.New("transfer: legacy evidence is unavailable")
	}
	verifyErr := verifyLegacyEvidenceStream(ctx, reader.evidence, io.LimitReader(file, MaxExpandedBytes+1))
	closeErr := file.Close()
	if verifyErr != nil {
		if closeErr != nil {
			return errors.Join(verifyErr, errors.New("transfer: close legacy evidence"))
		}
		return verifyErr
	}
	if closeErr != nil {
		return errors.New("transfer: close legacy evidence")
	}
	return nil
}

func verifyLegacyEvidenceStream(ctx context.Context, evidence LegacyEvidence, source io.Reader) error {
	hasher := sha256.New()
	written, err := copyLegacyContext(ctx, hasher, source)
	if err != nil {
		if contextError(err) != nil {
			return err
		}
		return errors.New("transfer: read legacy evidence")
	}
	if written != evidence.Bytes || written > MaxExpandedBytes ||
		hex.EncodeToString(hasher.Sum(nil)) != evidence.SHA256 {
		return errors.New("transfer: legacy evidence does not match its digest")
	}
	return nil
}

func copyLegacyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		read, readErr := source.Read(buffer)
		if read < 0 || read > len(buffer) {
			return written, errors.New("transfer: invalid legacy spool read")
		}
		if read > 0 {
			if err := ctx.Err(); err != nil {
				return written, err
			}
			output, writeErr := destination.Write(buffer[:read])
			written += int64(output)
			if writeErr != nil {
				return written, writeErr
			}
			if output != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func copyLegacySpool(ctx context.Context, destination io.Writer, source io.Reader) error {
	if _, err := copyLegacyContext(ctx, destination, source); err != nil {
		if contextError(err) != nil {
			return err
		}
		return errors.New("transfer: assemble normalized record stream")
	}
	return nil
}

type legacyNormalizer struct {
	ctx              context.Context
	root             string
	packageDirectory string
	binding          LegacyArchiveBinding
	evidencePath     string
	evidenceFile     *os.File
	evidence         LegacyEvidence
	spools           map[RecordType]*os.File
	associations     *validationIndex
	manifest         legacyManifest
	observed         legacyCounts
	terminalCounts   legacyCounts
	counts           CountsV1
	sourceRefs       []string
	sourceRefSet     map[string]struct{}
	kinds            []Kind
	kindSet          map[Kind]struct{}
	lineNumber       int64
	normalizedBytes  int64
	complete         bool
}

func newLegacyNormalizer(ctx context.Context, root, packageDirectory string, binding LegacyArchiveBinding) (*legacyNormalizer, error) {
	normalizer := &legacyNormalizer{
		ctx: ctx, root: root, packageDirectory: packageDirectory, binding: binding,
		evidencePath: filepath.Join(root, "msgvault-message-export.jsonl"),
		spools:       make(map[RecordType]*os.File),
		sourceRefSet: make(map[string]struct{}), kindSet: make(map[Kind]struct{}),
	}
	var err error
	normalizer.evidenceFile, err = os.OpenFile(normalizer.evidencePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errors.New("transfer: create legacy evidence spool")
	}
	for _, recordType := range []RecordType{RecordTypeSource, RecordTypeConversation, RecordTypeRecord} {
		file, createErr := os.OpenFile(filepath.Join(root, string(recordType)+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			_ = normalizer.close()
			return nil, errors.New("transfer: create legacy normalization spool")
		}
		normalizer.spools[recordType] = file
	}
	normalizer.associations, err = newValidationIndex()
	if err != nil {
		_ = normalizer.close()
		return nil, err
	}
	return normalizer, nil
}

func (normalizer *legacyNormalizer) run(input io.Reader) error {
	buffered := bufio.NewReader(input)
	hasher := sha256.New()
	for {
		if err := normalizer.ctx.Err(); err != nil {
			return err
		}
		raw, err := ReadCanonicalLine(buffered)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("transfer: invalid legacy JSONL framing")
		}
		normalizer.lineNumber++
		if normalizer.lineNumber > MaxRecordLines {
			return errors.New("transfer: legacy line limit")
		}
		lineBytes := int64(len(raw) + 1)
		if normalizer.evidence.Bytes > MaxExpandedBytes-lineBytes {
			return errors.New("transfer: legacy export byte limit")
		}
		if _, err := normalizer.evidenceFile.Write(raw); err != nil {
			return errors.New("transfer: write legacy evidence spool")
		}
		if _, err := normalizer.evidenceFile.Write([]byte{'\n'}); err != nil {
			return errors.New("transfer: write legacy evidence spool")
		}
		_, _ = hasher.Write(raw)
		_, _ = hasher.Write([]byte{'\n'})
		normalizer.evidence.Bytes += lineBytes
		if err := normalizer.visit(raw); err != nil {
			return fmt.Errorf("transfer: invalid legacy record at line %d: %w", normalizer.lineNumber, err)
		}
	}
	if !normalizer.complete {
		return errors.New("transfer: legacy completion record is missing")
	}
	if normalizer.observed != normalizer.terminalCounts {
		return errors.New("transfer: legacy counts do not reconcile")
	}
	result, err := normalizer.associations.relation("legacy_message_kinds").validate(normalizer.ctx, false)
	if err != nil {
		return err
	}
	if result.Duplicate || result.Missing {
		return errors.New("transfer: legacy message and conversation kinds do not reconcile")
	}
	normalizer.evidence.Format = LegacyExportFormat
	normalizer.evidence.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return normalizer.writePackage()
}

func (normalizer *legacyNormalizer) visit(raw []byte) error {
	if normalizer.complete {
		return errors.New("data follows completion")
	}
	var header struct {
		RecordType string `json:"record_type"`
	}
	if err := json.Unmarshal(raw, &header); err != nil || header.RecordType == "" {
		return errors.New("record header is invalid")
	}
	if normalizer.lineNumber == 1 && header.RecordType != "manifest" {
		return errors.New("manifest is missing")
	}
	switch header.RecordType {
	case "manifest":
		if normalizer.lineNumber != 1 {
			return errors.New("manifest is not first")
		}
		if err := decodeLegacyRecord(raw, []string{"record_type", "schema", "msgvault_version", "window", "filters"}, nil, &normalizer.manifest); err != nil {
			return err
		}
		if err := requireLegacyObjectKeys(raw, "window", []string{"start", "end"}); err != nil {
			return err
		}
		filterKeys := []string{"message_types", "sources"}
		if normalizer.manifest.Filters.PersonID != nil {
			filterKeys = append(filterKeys, "person_id")
		}
		if err := requireLegacyObjectKeys(raw, "filters", filterKeys); err != nil {
			return err
		}
		if err := validateLegacySourceSelectorShapes(normalizer.ctx, raw); err != nil {
			return err
		}
		return validateLegacyManifestInput(normalizer.manifest)
	case "source":
		if normalizer.lineNumber == 1 {
			return errors.New("manifest is missing")
		}
		var source legacySource
		if err := decodeLegacyRecord(raw, []string{"record_type", "source_type", "identifier", "display_name", "last_successful_sync_at"}, []string{"last_successful_sync_at"}, &source); err != nil {
			return err
		}
		return normalizer.addSource(source)
	case "conversation":
		var conversation legacyConversation
		if err := decodeLegacyRecord(raw, []string{"record_type", "source_type", "source_identifier", "id", "title", "conversation_type", "parent_id"}, []string{"parent_id"}, &conversation); err != nil {
			return err
		}
		return normalizer.addConversation(conversation)
	case "message":
		var message legacyMessage
		if err := decodeLegacyRecord(raw, []string{"record_type", "source_type", "source_identifier", "id", "conversation_id", "message_type", "subject", "text", "author", "occurred_at", "deleted_from_source"}, []string{"author"}, &message); err != nil {
			return err
		}
		if message.Author != nil {
			if err := requireLegacyObjectKeys(raw, "author", []string{"display_name", "address"}); err != nil {
				return err
			}
		}
		return normalizer.addMessage(message)
	case "complete":
		var complete legacyComplete
		if err := decodeLegacyRecord(raw, []string{"record_type", "counts"}, nil, &complete); err != nil {
			return err
		}
		if err := requireLegacyObjectKeys(raw, "counts", []string{"sources", "conversations", "messages"}); err != nil {
			return err
		}
		if complete.RecordType != "complete" || complete.Counts.hasInvalidValue() {
			return errors.New("completion is invalid")
		}
		normalizer.complete = true
		normalizer.terminalCounts = complete.Counts
		return nil
	default:
		return errors.New("record type is unsupported")
	}
}

func decodeLegacyRecord[T any](raw []byte, exactKeys, nullableKeys []string, value *T) error {
	if err := CheckStructuredKeys(raw); err != nil {
		return errors.New("structured fields are invalid")
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(raw, &object); err != nil {
		return errors.New("JSON object is invalid")
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	expected := slices.Clone(exactKeys)
	slices.Sort(expected)
	if !slices.Equal(keys, expected) {
		return errors.New("fields do not match the legacy contract")
	}
	for key, rawValue := range object {
		if bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) && !slices.Contains(nullableKeys, key) {
			return errors.New("null value does not match the legacy contract")
		}
	}
	if err := json.Unmarshal(raw, value, json.RejectUnknownMembers(true)); err != nil {
		return errors.New("value does not match the legacy contract")
	}
	return nil
}

func requireLegacyObjectKeys(raw []byte, field string, exactKeys []string) error {
	var outer map[string]jsontext.Value
	if err := json.Unmarshal(raw, &outer); err != nil {
		return errors.New("JSON object is invalid")
	}
	return requireLegacyValueKeys(outer[field], exactKeys)
}

func validateLegacySourceSelectorShapes(ctx context.Context, raw []byte) error {
	var outer map[string]jsontext.Value
	if err := json.Unmarshal(raw, &outer); err != nil {
		return errors.New("JSON object is invalid")
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(outer["filters"], &object); err != nil {
		return errors.New("nested legacy object is invalid")
	}
	var values []jsontext.Value
	if err := json.Unmarshal(object["sources"], &values); err != nil {
		return errors.New("nested legacy array is invalid")
	}
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := requireLegacyValueKeys(value, []string{"source_type", "identifier"}); err != nil {
			return err
		}
	}
	return nil
}

func requireLegacyValueKeys(raw []byte, exactKeys []string) error {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return errors.New("nested legacy object is invalid")
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	expected := slices.Clone(exactKeys)
	slices.Sort(expected)
	if !slices.Equal(keys, expected) {
		return errors.New("nested fields do not match the legacy contract")
	}
	for _, rawValue := range object {
		if bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
			return errors.New("null value does not match the nested legacy contract")
		}
	}
	return nil
}

type legacyManifest struct {
	RecordType      string        `json:"record_type"`
	Schema          string        `json:"schema"`
	MsgvaultVersion string        `json:"msgvault_version"`
	Window          legacyWindow  `json:"window"`
	Filters         legacyFilters `json:"filters"`
}

type legacyWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type legacyFilters struct {
	PersonID     *int64                 `json:"person_id"`
	MessageTypes []string               `json:"message_types"`
	Sources      []legacySourceSelector `json:"sources"`
}

type legacySourceSelector struct {
	SourceType string `json:"source_type"`
	Identifier string `json:"identifier"`
}

type legacySource struct {
	RecordType           string  `json:"record_type"`
	SourceType           string  `json:"source_type"`
	Identifier           string  `json:"identifier"`
	DisplayName          string  `json:"display_name"`
	LastSuccessfulSyncAt *string `json:"last_successful_sync_at"`
}

type legacyConversation struct {
	RecordType       string  `json:"record_type"`
	SourceType       string  `json:"source_type"`
	SourceIdentifier string  `json:"source_identifier"`
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	ConversationType string  `json:"conversation_type"`
	ParentID         *string `json:"parent_id"`
}

type legacyAuthor struct {
	DisplayName string `json:"display_name"`
	Address     string `json:"address"`
}

type legacyMessage struct {
	RecordType        string        `json:"record_type"`
	SourceType        string        `json:"source_type"`
	SourceIdentifier  string        `json:"source_identifier"`
	ID                string        `json:"id"`
	ConversationID    string        `json:"conversation_id"`
	MessageType       string        `json:"message_type"`
	Subject           string        `json:"subject"`
	Text              string        `json:"text"`
	Author            *legacyAuthor `json:"author"`
	OccurredAt        string        `json:"occurred_at"`
	DeletedFromSource bool          `json:"deleted_from_source"`
}

type legacyComplete struct {
	RecordType string       `json:"record_type"`
	Counts     legacyCounts `json:"counts"`
}

type legacyCounts struct {
	Sources       int64 `json:"sources"`
	Conversations int64 `json:"conversations"`
	Messages      int64 `json:"messages"`
}

func (counts legacyCounts) hasInvalidValue() bool {
	return counts.Sources < 0 || counts.Conversations < 0 || counts.Messages < 0 ||
		counts.Sources > MaxRecordLines-counts.Conversations ||
		counts.Sources+counts.Conversations > MaxRecordLines-counts.Messages
}

func validateLegacyManifestInput(manifest legacyManifest) error {
	if manifest.RecordType != "manifest" || manifest.Schema != LegacyExportFormat ||
		manifest.MsgvaultVersion == "" || len(manifest.MsgvaultVersion) > 128 {
		return errors.New("manifest identity is invalid")
	}
	start, startErr := time.Parse(time.RFC3339, manifest.Window.Start)
	end, endErr := time.Parse(time.RFC3339, manifest.Window.End)
	if startErr != nil || endErr != nil || !start.Before(end) {
		return errors.New("manifest window is invalid")
	}
	if manifest.Filters.MessageTypes == nil || manifest.Filters.Sources == nil {
		return errors.New("manifest filters are invalid")
	}
	if manifest.Filters.PersonID != nil && *manifest.Filters.PersonID <= 0 {
		return errors.New("manifest person filter is invalid")
	}
	for _, messageType := range manifest.Filters.MessageTypes {
		if messageType == "" || len(messageType) > MaxSourceTypeRawBytes {
			return errors.New("manifest message filter is invalid")
		}
	}
	for _, source := range manifest.Filters.Sources {
		if source.SourceType == "" || len(source.SourceType) > MaxSourceTypeRawBytes ||
			source.Identifier == "" || len(source.Identifier) > 4096 {
			return errors.New("manifest source filter is invalid")
		}
	}
	return nil
}

func (normalizer *legacyNormalizer) addSource(source legacySource) error {
	if source.RecordType != "source" || source.SourceType == "" || source.Identifier == "" ||
		len(source.SourceType) > MaxSourceTypeRawBytes || len(source.Identifier) > 4096 ||
		len(source.DisplayName) > MaxNameBytes {
		return errors.New("source is invalid")
	}
	if len(normalizer.manifest.Filters.Sources) > 0 && !slices.Contains(normalizer.manifest.Filters.Sources,
		legacySourceSelector{SourceType: source.SourceType, Identifier: source.Identifier}) {
		return errors.New("source does not match manifest filters")
	}
	if source.LastSuccessfulSyncAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *source.LastSuccessfulSyncAt); err != nil {
			return errors.New("source sync time is invalid")
		}
	}
	sourceRef, err := legacySourceRef(source.SourceType, source.Identifier)
	if err != nil {
		return err
	}
	if _, exists := normalizer.sourceRefSet[sourceRef]; exists {
		return errors.New("source identity is duplicated")
	}
	if len(normalizer.sourceRefs) >= MaxManifestBytes/64 {
		return errors.New("legacy source selection exceeds manifest bound")
	}
	normalizedType, route, rawType, err := normalizeLegacySourceType(source.SourceType)
	if err != nil {
		return err
	}
	line := SourceLineV1{
		RecordType: RecordTypeSource, SourceRef: sourceRef, SourceType: normalizedType,
		Route: route, Identifier: source.Identifier, SourceTypeRaw: rawType, DisplayName: source.DisplayName,
	}
	if err := normalizer.writeLine(RecordTypeSource, line); err != nil {
		return err
	}
	normalizer.sourceRefSet[sourceRef] = struct{}{}
	normalizer.sourceRefs = append(normalizer.sourceRefs, sourceRef)
	normalizer.observed.Sources++
	normalizer.counts.Source++
	return nil
}

func (normalizer *legacyNormalizer) addConversation(conversation legacyConversation) error {
	if conversation.RecordType != "conversation" || conversation.SourceType == "" ||
		conversation.SourceIdentifier == "" || conversation.ID == "" ||
		len(conversation.Title) > MaxBodyTextBytes || !slices.Contains(conversationTypes, conversation.ConversationType) {
		return errors.New("conversation is invalid")
	}
	sourceRef, err := legacySourceRef(conversation.SourceType, conversation.SourceIdentifier)
	if err != nil {
		return err
	}
	conversationRef, err := legacyScopedRef("conversation", conversation.SourceType, conversation.SourceIdentifier, conversation.ID)
	if err != nil {
		return err
	}
	parentRef := ""
	if conversation.ParentID != nil {
		if *conversation.ParentID == "" {
			return errors.New("conversation parent is invalid")
		}
		parentRef, err = legacyScopedRef("conversation", conversation.SourceType, conversation.SourceIdentifier, *conversation.ParentID)
		if err != nil {
			return err
		}
	}
	line := ConversationV1{
		RecordType: RecordTypeConversation, SourceRef: sourceRef, ConversationRef: conversationRef,
		Title: conversation.Title, ConversationType: conversation.ConversationType,
		ParentConversationRef: parentRef,
	}
	if err := normalizer.writeLine(RecordTypeConversation, line); err != nil {
		return err
	}
	for _, kind := range legacyConversationKinds(conversation.ConversationType) {
		if err := normalizer.associations.relation("legacy_message_kinds").addDefinition(
			relationKey(sourceRef, conversationRef, string(kind))); err != nil {
			return err
		}
	}
	normalizer.observed.Conversations++
	normalizer.counts.Conversation++
	return nil
}

func (normalizer *legacyNormalizer) addMessage(message legacyMessage) error {
	if message.RecordType != "message" || message.SourceType == "" || message.SourceIdentifier == "" ||
		message.ID == "" || message.ConversationID == "" || len(message.Subject) > MaxBodyTextBytes ||
		len(message.Text) > MaxBodyTextBytes {
		return errors.New("message is invalid")
	}
	if len(normalizer.manifest.Filters.MessageTypes) > 0 && !slices.Contains(normalizer.manifest.Filters.MessageTypes, message.MessageType) {
		return errors.New("message type does not match manifest filters")
	}
	kind, err := legacyMessageKind(message.MessageType)
	if err != nil {
		return err
	}
	sourceRef, err := legacySourceRef(message.SourceType, message.SourceIdentifier)
	if err != nil {
		return err
	}
	conversationRef, err := legacyScopedRef("conversation", message.SourceType, message.SourceIdentifier, message.ConversationID)
	if err != nil {
		return err
	}
	recordRef, err := LegacyRecordRef(message.SourceType, message.SourceIdentifier, message.ID)
	if err != nil {
		return err
	}
	date, err := legacyOrderingDate(message.OccurredAt)
	if err != nil {
		return err
	}
	record := RecordV1{
		RecordType: RecordTypeRecord, RecordRef: recordRef, LegacyRef: recordRef,
		Kind: kind, SourceRef: sourceRef, ConversationRef: conversationRef,
		Dates: []DateV1{date}, Subject: message.Subject, BodyText: message.Text,
		BodyMediaType: "text/plain", RecordOrigin: RecordOriginProducerCanonical,
		History:           &HistoryV1{Edits: HistoryStateUnsupported, Reactions: HistoryStateUnsupported, Deletions: HistoryStateUnsupported},
		DeletedFromSource: message.DeletedFromSource,
	}
	if message.Author != nil {
		if len(message.Author.DisplayName) > MaxNameBytes || len(message.Author.Address) > MaxContactPointBytes {
			return errors.New("message author is invalid")
		}
		participant := ParticipantV1{
			DisplayName: message.Author.DisplayName, Address: message.Author.Address,
			EnvelopeRole: EnvelopeRoleFrom, ScopeDirection: ScopeDirectionFromPerson,
		}
		if participant.Address != "" {
			participant.AddressKind = legacyAddressKind(kind, message.MessageType)
		}
		record.Participants = []ParticipantV1{participant}
	}
	record.NormalizedSHA256, err = NormalizedRecordSHA256(record)
	if err != nil {
		return err
	}
	if err := normalizer.writeLine(RecordTypeRecord, record); err != nil {
		return err
	}
	if err := normalizer.associations.relation("legacy_message_kinds").addReference(
		relationKey(sourceRef, conversationRef, string(kind))); err != nil {
		return err
	}
	if _, exists := normalizer.kindSet[kind]; !exists {
		normalizer.kindSet[kind] = struct{}{}
		normalizer.kinds = append(normalizer.kinds, kind)
	}
	normalizer.observed.Messages++
	normalizer.counts.Record++
	return nil
}

func (normalizer *legacyNormalizer) writeLine(recordType RecordType, value any) error {
	var raw []byte
	var err error
	switch typed := value.(type) {
	case SourceLineV1:
		raw, _, err = MarshalSourceLineV1(typed)
	case ConversationV1:
		raw, _, err = MarshalConversationV1(typed)
	case RecordV1:
		raw, _, err = MarshalRecordV1(typed)
	default:
		return errors.New("transfer: unsupported legacy normalized record")
	}
	if err != nil {
		return err
	}
	line := make([]byte, len(raw)+1)
	copy(line, raw)
	line[len(raw)] = '\n'
	if int64(len(line)) > MaxExpandedBytes-normalizer.normalizedBytes {
		return errors.New("transfer: normalized legacy package exceeds its byte bound")
	}
	if _, err := normalizer.spools[recordType].Write(line); err != nil {
		return errors.New("transfer: write legacy normalization spool")
	}
	normalizer.normalizedBytes += int64(len(line))
	return nil
}

func (normalizer *legacyNormalizer) writePackage() error {
	if err := normalizer.ctx.Err(); err != nil {
		return err
	}
	if err := normalizer.evidenceFile.Close(); err != nil {
		normalizer.evidenceFile = nil
		return errors.New("transfer: close legacy evidence spool")
	}
	normalizer.evidenceFile = nil
	for _, spool := range normalizer.spools {
		if err := normalizer.ctx.Err(); err != nil {
			return err
		}
		if err := spool.Sync(); err != nil {
			return errors.New("transfer: sync legacy normalization spool")
		}
		if _, err := spool.Seek(0, io.SeekStart); err != nil {
			return errors.New("transfer: rewind legacy normalization spool")
		}
	}
	recordsPath := filepath.Join(normalizer.packageDirectory, "records.jsonl")
	records, err := os.OpenFile(recordsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("transfer: create normalized record stream")
	}
	prefixHash := sha256.New()
	wholeHash := sha256.New()
	for _, recordType := range []RecordType{RecordTypeSource, RecordTypeConversation, RecordTypeRecord} {
		if err := copyLegacySpool(normalizer.ctx, io.MultiWriter(records, prefixHash, wholeHash), normalizer.spools[recordType]); err != nil {
			_ = records.Close()
			return err
		}
	}
	if err := normalizer.ctx.Err(); err != nil {
		_ = records.Close()
		return err
	}
	prefixDigest := hex.EncodeToString(prefixHash.Sum(nil))
	completeRaw, _, err := MarshalCompleteV1(CompleteV1{
		RecordType: RecordTypeComplete, Counts: normalizer.counts, RecordsSHA256: prefixDigest,
	})
	if err != nil {
		_ = records.Close()
		return err
	}
	completeLine := make([]byte, len(completeRaw)+1)
	copy(completeLine, completeRaw)
	completeLine[len(completeRaw)] = '\n'
	if _, err := io.MultiWriter(records, wholeHash).Write(completeLine); err != nil {
		_ = records.Close()
		return errors.New("transfer: finish normalized record stream")
	}
	if err := records.Close(); err != nil {
		return errors.New("transfer: close normalized record stream")
	}
	if err := normalizer.ctx.Err(); err != nil {
		return err
	}
	slices.Sort(normalizer.sourceRefs)
	slices.Sort(normalizer.kinds)
	manifest := ManifestV1{
		Format: LegacyCompatibilityFormat, PackageID: legacyPackageID(normalizer.evidence.SHA256),
		Producer: ProducerV1{Name: legacyAdapterName, Version: legacyAdapterVersion, ContractRevision: ContractRevision},
		Archive:  ArchiveV1{ArchiveID: normalizer.binding.ArchiveID, System: "msgvault", DisplayName: normalizer.binding.DisplayName},
		Selection: SelectionV1{
			Sources: normalizer.sourceRefs, Kinds: normalizer.kinds,
			Window:            SelectionWindowV1{Start: normalizer.manifest.Window.Start, End: normalizer.manifest.Window.End},
			PersonFieldPolicy: "identity_only", Excluded: []SelectionExcludedV1{},
		},
		RecordsSHA256: prefixDigest, Counts: normalizer.counts,
	}
	manifestRaw, _, err := MarshalManifestV1(manifest)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(normalizer.packageDirectory, "transfer.json")
	if err := os.WriteFile(manifestPath, manifestRaw, 0o600); err != nil {
		return errors.New("transfer: write normalized manifest")
	}
	sums := fmt.Sprintf("%s  records.jsonl\n%s  transfer.json\n", hex.EncodeToString(wholeHash.Sum(nil)), sha256Hex(manifestRaw))
	if err := os.WriteFile(filepath.Join(normalizer.packageDirectory, "SHA256SUMS"), []byte(sums), 0o600); err != nil {
		return errors.New("transfer: write normalized checksums")
	}
	return normalizer.ctx.Err()
}

func (normalizer *legacyNormalizer) close() error {
	var result error
	if normalizer.evidenceFile != nil {
		result = errors.Join(result, normalizer.evidenceFile.Close())
		normalizer.evidenceFile = nil
	}
	for recordType, spool := range normalizer.spools {
		result = errors.Join(result, spool.Close())
		delete(normalizer.spools, recordType)
	}
	if normalizer.associations != nil {
		result = errors.Join(result, normalizer.associations.Close())
		normalizer.associations = nil
	}
	if result != nil {
		return errors.New("transfer: close legacy normalization spool")
	}
	return nil
}

func legacySourceRef(sourceType, identifier string) (string, error) {
	return legacyScopedRef("source", sourceType, identifier)
}

func legacyScopedRef(parts ...string) (string, error) {
	for _, part := range parts {
		if part == "" || len(part) > 4096 {
			return "", errors.New("transfer: invalid legacy identity")
		}
	}
	raw, err := canonical.Marshal(append([]string{LegacyExportFormat}, parts...))
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func normalizeLegacySourceType(sourceType string) (SourceType, string, string, error) {
	aliases := map[string]SourceType{
		"gcal": SourceTypeGoogleCalendar, "facebook_messenger": SourceTypeMessenger,
		"apple_messages": SourceTypeIMessage, "synctech_sms": SourceTypeSyncTech,
	}
	normalized := SourceType(sourceType)
	if alias, ok := aliases[sourceType]; ok {
		normalized = alias
	}
	if ValidSourceType(normalized) && normalized != SourceTypeOther {
		for _, row := range sourceQualificationRows {
			if row.sourceType == normalized {
				return normalized, row.route, "", nil
			}
		}
	}
	if len(sourceType) > MaxSourceTypeRawBytes || !sourceTypeRawPattern.MatchString(sourceType) {
		return "", "", "", errors.New("transfer: legacy source type is unsupported")
	}
	return SourceTypeOther, sourceType, sourceType, nil
}

func legacyMessageKind(messageType string) (Kind, error) {
	switch messageType {
	case "", "email":
		return KindEmail, nil
	case "sms", "mms", "whatsapp", "teams", "imessage", "slack", "discord", "beeper", "messenger", "fbmessenger", "google_voice_text":
		return KindChatMessage, nil
	case "google_voice_call", "synctech_sms_call", "call_received", "call_placed", "call_missed":
		return KindCall, nil
	case "voicemail", "google_voice_voicemail":
		return KindVoicemail, nil
	case "calendar_event":
		return KindCalendarEvent, nil
	case "meeting_note":
		return KindMeetingNote, nil
	case "meeting_transcript":
		return KindTranscript, nil
	default:
		return "", errors.New("transfer: legacy message type is unsupported")
	}
}

func legacyConversationKinds(conversationType string) []Kind {
	switch conversationType {
	case "email_thread":
		return []Kind{KindEmail}
	case "channel", "thread", "direct_chat", "group_chat":
		return []Kind{KindChatMessage, KindCall, KindVoicemail}
	case "calendar":
		return []Kind{KindCalendarEvent}
	case "meeting":
		return []Kind{KindMeetingNote, KindTranscript}
	case "other":
		return []Kind{KindCall, KindVoicemail}
	default:
		return nil
	}
}

func legacyOrderingDate(raw string) (DateV1, error) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return DateV1{}, errors.New("transfer: legacy ordering timestamp is invalid")
	}
	fractionDigits := 0
	if dot := strings.IndexByte(raw, '.'); dot >= 0 {
		end := len(raw)
		if strings.HasSuffix(raw, "Z") {
			end--
		} else {
			end -= 6
		}
		fractionDigits = end - dot - 1
		if fractionDigits < 1 || fractionDigits > 9 {
			return DateV1{}, errors.New("transfer: legacy ordering timestamp precision is invalid")
		}
	}
	precision := PrecisionSecond
	layout := "2006-01-02T15:04:05"
	if fractionDigits > 0 {
		precision = PrecisionFraction
		layout += "." + strings.Repeat("0", fractionDigits)
	}
	date := DateV1{
		Instant: parsed.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		Civil:   parsed.Format(layout), Precision: precision, FractionDigits: fractionDigits,
		Origin: OriginUnspecified, Raw: raw,
		Diagnostics: []string{string(ReasonLegacyPrecisionUnknown)},
	}
	_, offsetSeconds := parsed.Zone()
	if strings.HasSuffix(raw, "Z") {
		date.Timezone = TimezoneKindUTC
	} else {
		offset := offsetSeconds / 60
		date.Timezone = TimezoneKindOffset
		date.UTCOffsetMinutes = &offset
	}
	return date, nil
}

func legacyAddressKind(kind Kind, messageType string) ContactPointKind {
	switch {
	case kind == KindEmail:
		return ContactPointKindEmail
	case messageType == "sms" || messageType == "mms" || messageType == "synctech_sms_call" || strings.HasPrefix(messageType, "google_voice"):
		return ContactPointKindPhone
	case messageType == "whatsapp":
		return ContactPointKindWhatsApp
	default:
		return ContactPointKindHandle
	}
}

func legacyPackageID(digest string) string {
	raw, _ := hex.DecodeString(digest[:32])
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	hexID := hex.EncodeToString(raw)
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:]
}

func validateLegacyManifest(
	manifest ManifestV1,
	evidence LegacyEvidence,
	binding LegacyArchiveBinding,
	limitations []CapabilityEntryV1,
) *semanticFinding {
	if evidence.Format != LegacyExportFormat || !canonical.IsSHA256Hex(evidence.SHA256) || evidence.Bytes <= 0 || evidence.Bytes > MaxExpandedBytes ||
		manifest.Format != LegacyCompatibilityFormat || manifest.ExportSequence != "" || manifest.CreatedAt != "" ||
		manifest.Snapshot != (SnapshotV1{}) || manifest.Producer != (ProducerV1{Name: legacyAdapterName, Version: legacyAdapterVersion, ContractRevision: ContractRevision}) ||
		manifest.PackageID != legacyPackageID(evidence.SHA256) || manifest.Archive.ArchiveID != binding.ArchiveID ||
		manifest.Archive.DisplayName != binding.DisplayName || manifest.Archive.System != "msgvault" ||
		!validArchiveID(manifest.Archive.ArchiveID) || len(manifest.Archive.DisplayName) > MaxNameBytes ||
		!canonical.IsSHA256Hex(manifest.RecordsSHA256) || !slices.Equal(limitations, legacyFormatLimitations()) {
		return invalidSemantic("package_integrity_failed", "legacy compatibility authority is invalid")
	}
	return validateManifestBody(manifest)
}

func validateLegacyOrderingDate(date DateV1) error {
	if date.Kind != "" || date.Origin != OriginUnspecified || date.Raw == "" ||
		!slices.Equal(date.Diagnostics, []string{string(ReasonLegacyPrecisionUnknown)}) {
		return errors.New("transfer: invalid legacy ordering evidence")
	}
	return validateDate(date, false)
}
