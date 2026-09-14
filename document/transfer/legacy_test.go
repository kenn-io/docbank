package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fiveLineLegacyFixture = `{"record_type":"manifest","schema":"msgvault-message-export/1","msgvault_version":"v0.1.0","window":{"start":"2025-01-01T00:00:00Z","end":"2025-02-01T00:00:00Z"},"filters":{"person_id":42,"message_types":["sms"],"sources":[{"source_type":"synctech_sms","identifier":"phone|primary"}]}}
{"record_type":"source","source_type":"synctech_sms","identifier":"phone|primary","display_name":"Synthetic phone","last_successful_sync_at":null}
{"record_type":"conversation","source_type":"synctech_sms","source_identifier":"phone|primary","id":"thread-1","title":"Synthetic thread","conversation_type":"direct_chat","parent_id":null}
{"record_type":"message","source_type":"synctech_sms","source_identifier":"phone|primary","id":"message-1","conversation_id":"thread-1","message_type":"sms","subject":"","text":"synthetic body","author":null,"occurred_at":"2025-01-02T03:04:05.1200-07:00","deleted_from_source":false}
{"record_type":"complete","counts":{"sources":1,"conversations":1,"messages":1}}
`

func TestLegacyIdentityDoesNotConcatenateUnescapedDelimiters(t *testing.T) {
	a, err := LegacyRecordRef("chat", "account|part", "message")
	require.NoError(t, err)
	b, err := LegacyRecordRef("chat", "account", "part|message")
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

func TestLegacyReaderRequiresArchiveBinding(t *testing.T) {
	_, err := ReadLegacyExport(t.Context(), bytes.NewBufferString(fiveLineLegacyFixture))
	require.ErrorIs(t, err, ErrLegacyArchiveRequired)
}

func TestLegacyEvidenceCopyPreservesMidstreamCancellation(t *testing.T) {
	reader, err := ReadLegacyExportWithArchive(t.Context(),
		bytes.NewBufferString(fiveLineLegacyFixture), LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	implementation, ok := reader.(*legacyPackageReader)
	require.True(t, ok)
	evidence := bytes.Repeat([]byte{'x'}, 128*1024)
	require.NoError(t, os.WriteFile(implementation.evidencePath, evidence, 0o600))
	digest := sha256.Sum256(evidence)
	implementation.evidence.Bytes = int64(len(evidence))
	implementation.evidence.SHA256 = hex.EncodeToString(digest[:])
	ctx := &cancelAfterChecksContext{Context: t.Context(), cancelAfter: 3, failure: context.Canceled}
	err = implementation.verifyLegacyEvidence(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.checks, 4)
}

func TestLegacyAssemblyCopyPreservesMidstreamCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "legacy")
	require.NoError(t, os.Mkdir(root, 0o700))
	packageDirectory := filepath.Join(root, "package")
	require.NoError(t, os.Mkdir(packageDirectory, 0o700))
	ctx := &cancelAfterChecksContext{Context: t.Context(), cancelAfter: 6, failure: context.Canceled}
	normalizer, err := newLegacyNormalizer(ctx, root, packageDirectory,
		LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, normalizer.close()) })
	_, err = normalizer.spools[RecordTypeSource].Write(bytes.Repeat([]byte{'x'}, 128*1024))
	require.NoError(t, err)
	err = normalizer.writePackage()
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.checks, 7)
	info, err := os.Stat(filepath.Join(packageDirectory, "records.jsonl"))
	require.NoError(t, err)
	assert.Equal(t, int64(64*1024), info.Size())
}

func TestLegacyCopyPreservesDeadlineExceeded(t *testing.T) {
	ctx := &cancelAfterChecksContext{Context: t.Context(), cancelAfter: 1, failure: context.DeadlineExceeded}
	_, err := copyLegacyContext(ctx, io.Discard, bytes.NewBufferString("synthetic"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestLegacyReaderPreservesReducedAuthorityAndOriginalEvidence(t *testing.T) {
	reader, err := ReadLegacyExportWithArchive(t.Context(),
		bytes.NewBufferString(fiveLineLegacyFixture),
		LegacyArchiveBinding{ArchiveID: "archive_synthetic", DisplayName: "Synthetic archive"},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	manifest := reader.Manifest()
	assert.Equal(t, LegacyCompatibilityFormat, manifest.Format)
	assert.Empty(t, manifest.ExportSequence)
	assert.Empty(t, manifest.CreatedAt)
	assert.Equal(t, SnapshotV1{}, manifest.Snapshot)
	assert.Equal(t, "archive_synthetic", manifest.Archive.ArchiveID)

	evidence := reader.LegacyEvidence()
	wantHash := sha256.Sum256([]byte(fiveLineLegacyFixture))
	assert.Equal(t, LegacyExportFormat, evidence.Format)
	assert.Equal(t, hex.EncodeToString(wantHash[:]), evidence.SHA256)
	assert.Equal(t, int64(len(fiveLineLegacyFixture)), evidence.Bytes)
	original, err := reader.OpenLegacyEvidence(t.Context())
	require.NoError(t, err)
	originalBytes, err := io.ReadAll(original)
	require.NoError(t, err)
	require.NoError(t, original.Close())
	if !bytes.Equal([]byte(fiveLineLegacyFixture), originalBytes) {
		t.Fatal("retained legacy evidence does not match the input bytes")
	}

	var record RecordV1
	require.NoError(t, reader.Records(t.Context(), func(_ int, raw []byte) error {
		header, decodeErr := decodeRecordHeader(raw)
		if decodeErr == nil && header.RecordType == RecordTypeRecord {
			record, _, decodeErr = DecodeRecordV1(raw)
		}
		return decodeErr
	}))
	assert.Equal(t, KindChatMessage, record.Kind)
	assert.Empty(t, record.Participants, "a null legacy author carries no participant authority")
	require.Len(t, record.Dates, 1)
	date := record.Dates[0]
	assert.Empty(t, date.Kind, "the ordering timestamp is not a sent or received event")
	assert.Equal(t, OriginUnspecified, date.Origin)
	assert.Equal(t, "2025-01-02T03:04:05.1200-07:00", date.Raw)
	assert.Equal(t, PrecisionFraction, date.Precision)
	assert.Equal(t, 4, date.FractionDigits)
	assert.Equal(t, TimezoneKindOffset, date.Timezone)
	assert.Equal(t, -420, *date.UTCOffsetMinutes)
	assert.Equal(t, []string{string(ReasonLegacyPrecisionUnknown)}, date.Diagnostics)

	report, err := Validate(t.Context(), reader)
	require.NoError(t, err)
	assert.True(t, report.Valid)
	assert.Equal(t, PackageAuthorityLegacyCompatibility, report.PackageAuthority)
	require.NotNil(t, report.LegacyEvidence)
	assert.Equal(t, evidence, *report.LegacyEvidence)
	wantLimitations := []CapabilityEntryV1{
		{Capability: CapabilityRaw, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoRaw},
		{Capability: CapabilityAttachmentBytes, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoAttachments},
		{Capability: CapabilityPeople, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoPersonUID},
		{Capability: CapabilityHistory, State: CapabilityStateUnavailable, Reason: ReasonFormatV1NoHistory},
	}
	assert.Equal(t, wantLimitations, reader.LegacyLimitations())
	assert.Equal(t, wantLimitations, report.FormatLimitations)
}

func TestLegacyCompatibilityCannotEnterThroughOrdinaryPackageReader(t *testing.T) {
	reader, err := ReadLegacyExportWithArchive(t.Context(),
		bytes.NewBufferString(fiveLineLegacyFixture), LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	type ordinaryReader struct{ PackageReader }
	report, err := Validate(t.Context(), ordinaryReader{PackageReader: reader})
	require.Error(t, err)
	assert.False(t, report.Valid)
	require.NotEmpty(t, report.Findings)
	assert.Equal(t, "unsupported_record_kind", report.Findings[0].Code)
}

func TestLegacyReaderRejectsCountsUnknownFieldsAndKindMismatch(t *testing.T) {
	tests := map[string]string{
		"counts": bytes.NewBufferString(fiveLineLegacyFixture).String()[:len(fiveLineLegacyFixture)-3] + "2}}\n",
		"unknown field": replaceLegacy(t, fiveLineLegacyFixture,
			`"display_name":"Synthetic phone"`, `"display_name":"Synthetic phone","private_extra":true`),
		"unknown source filter field": replaceLegacy(t, fiveLineLegacyFixture,
			`"identifier":"phone|primary"`, `"identifier":"phone|primary","private_extra":true`),
		"message kind mismatch": replaceLegacy(t, fiveLineLegacyFixture,
			`"message_type":"sms"`, `"message_type":"calendar_event"`),
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ReadLegacyExportWithArchive(t.Context(), bytes.NewBufferString(fixture),
				LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
			require.Error(t, err)
		})
	}
}

func TestLegacyReaderRejectsNullForNonNullableProducerValues(t *testing.T) {
	tests := map[string][2]string{
		"manifest scalar":      {`"msgvault_version":"v0.1.0"`, `"msgvault_version":null`},
		"window scalar":        {`"start":"2025-01-01T00:00:00Z"`, `"start":null`},
		"person filter":        {`"person_id":42`, `"person_id":null`},
		"source filter scalar": {`"identifier":"phone|primary"`, `"identifier":null`},
		"source scalar":        {`"display_name":"Synthetic phone"`, `"display_name":null`},
		"conversation scalar":  {`"title":"Synthetic thread"`, `"title":null`},
		"message subject":      {`"subject":""`, `"subject":null`},
		"message text":         {`"text":"synthetic body"`, `"text":null`},
		"message boolean":      {`"deleted_from_source":false`, `"deleted_from_source":null`},
		"terminal count":       {`"messages":1`, `"messages":null`},
	}
	for name, replacement := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := replaceLegacy(t, fiveLineLegacyFixture, replacement[0], replacement[1])
			_, err := ReadLegacyExportWithArchive(t.Context(), bytes.NewBufferString(fixture),
				LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
			require.Error(t, err)
		})
	}

	withAuthor := replaceLegacy(t, fiveLineLegacyFixture, `"author":null`,
		`"author":{"display_name":"Ada","address":"+15550000001"}`)
	for name, replacement := range map[string][2]string{
		"author display name": {`"display_name":"Ada"`, `"display_name":null`},
		"author address":      {`"address":"+15550000001"`, `"address":null`},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := replaceLegacy(t, withAuthor, replacement[0], replacement[1])
			_, err := ReadLegacyExportWithArchive(t.Context(), bytes.NewBufferString(fixture),
				LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
			require.Error(t, err)
		})
	}
}

func TestLegacySourceSelectorShapeIterationPreservesCancellation(t *testing.T) {
	fixture := replaceLegacy(t, fiveLineLegacyFixture,
		`"sources":[{"source_type":"synctech_sms","identifier":"phone|primary"}]`,
		`"sources":[{"source_type":"synctech_sms","identifier":"phone|primary"},{"source_type":"synctech_sms","identifier":"phone-secondary"}]`)
	temporaryRoot := t.TempDir()
	t.Setenv("TMPDIR", temporaryRoot)
	ctx := &cancelAfterChecksContext{Context: t.Context(), cancelAfter: 2, failure: context.Canceled}
	_, err := ReadLegacyExportWithArchive(ctx, bytes.NewBufferString(fixture),
		LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.checks, 3)
	entries, err := os.ReadDir(temporaryRoot)
	require.NoError(t, err)
	assert.Empty(t, entries, "canceled selector validation must remove owned spools")
}

type cancelAfterChecksContext struct {
	context.Context

	cancelAfter int
	failure     error
	checks      int
}

func (ctx *cancelAfterChecksContext) Err() error {
	ctx.checks++
	if ctx.checks > ctx.cancelAfter {
		return ctx.failure
	}
	return ctx.Context.Err()
}

func TestLegacyReaderMapsSyncTechCallTuple(t *testing.T) {
	fixture := replaceLegacy(t, fiveLineLegacyFixture, `"message_types":["sms"]`,
		`"message_types":["synctech_sms_call"]`)
	fixture = replaceLegacy(t, fixture, `"message_type":"sms"`, `"message_type":"synctech_sms_call"`)
	fixture = replaceLegacy(t, fixture, `"author":null`,
		`"author":{"display_name":"Ada","address":"+15550000001"}`)
	reader, err := ReadLegacyExportWithArchive(t.Context(), bytes.NewBufferString(fixture),
		LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	var record RecordV1
	require.NoError(t, reader.Records(t.Context(), func(_ int, raw []byte) error {
		header, decodeErr := decodeRecordHeader(raw)
		if decodeErr == nil && header.RecordType == RecordTypeRecord {
			record, _, decodeErr = DecodeRecordV1(raw)
		}
		return decodeErr
	}))
	assert.Equal(t, KindCall, record.Kind)
	require.Len(t, record.Participants, 1)
	assert.Equal(t, ContactPointKindPhone, record.Participants[0].AddressKind)
}

func TestLegacyValidationRejectsChangedFormatLimitations(t *testing.T) {
	reader, err := ReadLegacyExportWithArchive(t.Context(),
		bytes.NewBufferString(fiveLineLegacyFixture), LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	implementation, ok := reader.(*legacyPackageReader)
	require.True(t, ok)
	implementation.limitations[0].Reason = ReasonNotSelected

	report, err := Validate(t.Context(), reader)
	require.Error(t, err)
	assert.False(t, report.Valid)
	require.NotEmpty(t, report.Findings)
	assert.Equal(t, "package_integrity_failed", report.Findings[0].Code)
}

func TestLegacyReaderAcceptsInterleavedConversationPagesAndPreservesAuthor(t *testing.T) {
	fixture := `{"record_type":"manifest","schema":"msgvault-message-export/1","msgvault_version":"v0.1.0","window":{"start":"2025-01-01T00:00:00Z","end":"2025-02-01T00:00:00Z"},"filters":{"message_types":["sms","fbmessenger"],"sources":[{"source_type":"synctech_sms","identifier":"phone-primary"}]}}
{"record_type":"source","source_type":"synctech_sms","identifier":"phone-primary","display_name":"Synthetic phone","last_successful_sync_at":null}
{"record_type":"conversation","source_type":"synctech_sms","source_identifier":"phone-primary","id":"thread-1","title":"First","conversation_type":"direct_chat","parent_id":null}
{"record_type":"message","source_type":"synctech_sms","source_identifier":"phone-primary","id":"message-1","conversation_id":"thread-1","message_type":"sms","subject":"","text":"first","author":{"display_name":"Ada","address":"+15550000001"},"occurred_at":"2025-01-02T03:04:05Z","deleted_from_source":false}
{"record_type":"conversation","source_type":"synctech_sms","source_identifier":"phone-primary","id":"thread-2","title":"Second","conversation_type":"direct_chat","parent_id":null}
{"record_type":"message","source_type":"synctech_sms","source_identifier":"phone-primary","id":"message-2","conversation_id":"thread-2","message_type":"fbmessenger","subject":"","text":"second","author":null,"occurred_at":"2025-01-03T03:04:05Z","deleted_from_source":false}
{"record_type":"complete","counts":{"sources":1,"conversations":2,"messages":2}}
`
	reader, err := ReadLegacyExportWithArchive(t.Context(), bytes.NewBufferString(fixture),
		LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	var recordTypes []RecordType
	var first RecordV1
	require.NoError(t, reader.Records(t.Context(), func(_ int, raw []byte) error {
		header, decodeErr := decodeRecordHeader(raw)
		if decodeErr != nil {
			return decodeErr
		}
		recordTypes = append(recordTypes, header.RecordType)
		if header.RecordType == RecordTypeRecord && first.RecordRef == "" {
			first, _, decodeErr = DecodeRecordV1(raw)
		}
		return decodeErr
	}))
	assert.Equal(t, []RecordType{
		RecordTypeSource, RecordTypeConversation, RecordTypeConversation,
		RecordTypeRecord, RecordTypeRecord, RecordTypeComplete,
	}, recordTypes)
	require.Len(t, first.Participants, 1)
	assert.Equal(t, "Ada", first.Participants[0].DisplayName)
	assert.Equal(t, "+15550000001", first.Participants[0].Address)
	assert.Equal(t, ContactPointKindPhone, first.Participants[0].AddressKind)
}

func TestLegacyValidationDetectsChangedOriginalEvidence(t *testing.T) {
	reader, err := ReadLegacyExportWithArchive(t.Context(),
		bytes.NewBufferString(fiveLineLegacyFixture), LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	implementation, ok := reader.(*legacyPackageReader)
	require.True(t, ok)
	require.NoError(t, os.WriteFile(implementation.evidencePath, []byte("changed\n"), 0o600))

	report, err := Validate(t.Context(), reader)
	require.Error(t, err)
	assert.False(t, report.Valid)
	require.NotEmpty(t, report.Findings)
	assert.Equal(t, "package_integrity_failed", report.Findings[0].Code)
}

func TestLegacyReaderCloseRemovesItsSpool(t *testing.T) {
	reader, err := ReadLegacyExportWithArchive(t.Context(),
		bytes.NewBufferString(fiveLineLegacyFixture), LegacyArchiveBinding{ArchiveID: "archive_synthetic"})
	require.NoError(t, err)
	implementation, ok := reader.(*legacyPackageReader)
	require.True(t, ok)
	root := implementation.root
	require.NoError(t, reader.Close())
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func replaceLegacy(t *testing.T, input, old, replacement string) string {
	t.Helper()
	require.Contains(t, input, old)
	return bytes.NewBuffer(bytes.Replace([]byte(input), []byte(old), []byte(replacement), 1)).String()
}
