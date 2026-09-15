package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransferHashesBindActualBytesAndStableMeaning(t *testing.T) {
	r := RecordV1{
		RecordType:       RecordTypeRecord,
		SourceRef:        "s1",
		RecordRef:        "r1",
		LegacyRef:        "legacy-1",
		Revision:         "observation-1",
		Kind:             KindChatMessage,
		RecordOrigin:     RecordOriginProviderOriginal,
		BodyText:         "orchid-47",
		Raw:              &BlobRefV1{BlobSHA256: strings.Repeat("a", 64), Size: 10, MediaType: "message/rfc822"},
		NormalizedSHA256: strings.Repeat("b", 64),
	}
	raw, digest, err := MarshalRecordV1(r)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(sum[:]), digest)

	semantic, err := NormalizedRecordSHA256(r)
	require.NoError(t, err)
	r.Revision = "later-observation"
	r.LegacyRef = "legacy-2"
	r.Raw = &BlobRefV1{BlobSHA256: strings.Repeat("c", 64), Size: 12, MediaType: "message/rfc822"}
	r.NormalizedSHA256 = strings.Repeat("d", 64)
	other, changed, err := MarshalRecordV1(r)
	require.NoError(t, err)
	require.NotEqual(t, raw, other)
	require.NotEqual(t, digest, changed)
	same, err := NormalizedRecordSHA256(r)
	require.NoError(t, err)
	require.Equal(t, semantic, same)

	_, _, err = DecodeRecordV1([]byte(`{"kind":"chat_message","vibe":true}`))
	require.Error(t, err)
}

func TestManifestOverflowNamesItsBound(t *testing.T) {
	manifest := testPackageManifest()
	for range 16000 {
		manifest.Selection.Sources = append(manifest.Selection.Sources, strings.Repeat("a", 64))
	}
	_, _, err := MarshalManifestV1(manifest)
	require.ErrorContains(t, err, "manifest size limit")
}

func TestTransferCanonicalGoldenCoversEveryLineType(t *testing.T) {
	manifest := ManifestV1{
		Format:         FormatV1,
		PackageID:      "pkg-1",
		ExportSequence: "00000000000000000001",
		Producer:       ProducerV1{Name: "msgvault", Version: "fixture", ContractRevision: ContractRevision},
		Archive:        ArchiveV1{ArchiveID: "archive-1", System: "msgvault", DisplayName: "Fixture"},
		CreatedAt:      "2026-09-12T08:00:00.000000000Z",
		Selection: SelectionV1{
			Sources: []string{"source-1"}, Kinds: []Kind{KindChatMessage},
			Window:            SelectionWindowV1{Start: "2026-01-01T00:00:00Z", End: "2027-01-01T00:00:00Z"},
			PersonFieldPolicy: "identity_only", Excluded: []SelectionExcludedV1{},
		},
		Snapshot: SnapshotV1{
			Consistency: "single_read_transaction", SpineTimezone: "UTC", SpineGeneration: 1,
			HighWatermark:   "2026-09-12T07:59:59.000000000Z",
			ReadStartedAt:   "2026-09-12T07:59:59.000000000Z",
			ReadCompletedAt: "2026-09-12T08:00:00.000000000Z",
		},
		RecordsSHA256: strings.Repeat("0", 64), Counts: CountsV1{},
	}
	person := PersonV1{
		RecordType: RecordTypePerson, PersonRef: "person-1", UIDKind: PersonUIDKindVCard,
		DisplayName: "Ada", SelectedFields: []string{"display_name"}, WithheldFields: []string{},
		ContactPoints: []ContactPointV1{{
			Kind: ContactPointKindEmail, ValueNormalized: "ada@example.test",
			ValueDisplay: "ada@example.test", IsPrimary: false,
		}},
	}
	source := SourceLineV1{
		RecordType: RecordTypeSource, SourceRef: "source-1", SourceType: SourceTypeSlack,
		Route: "slack", Identifier: "workspace-1",
	}
	conversation := ConversationV1{
		RecordType: RecordTypeConversation, SourceRef: "source-1", ConversationRef: "conversation-1",
		Title: "General", ConversationType: "channel",
	}
	record := RecordV1{
		RecordType: RecordTypeRecord, RecordRef: "attachment-1", Kind: KindAttachmentOccurrence,
		SourceRef: "source-1", RecordOrigin: RecordOriginProducerCanonical,
		Attachment: &AttachmentOccurrenceV1{
			PartKey: "part-1", Filename: "note.txt", MediaType: "text/plain", Size: 7,
			ContentSHA256: strings.Repeat("2", 64), Role: AttachmentRoleStandalone,
			Blob: nil, Availability: AvailabilityMetadataOnly, AvailabilityReason: ReasonNotSelected,
		},
	}
	coverage := CoverageV1{
		RecordType: RecordTypeCoverage, SourceRef: "source-1", Route: "slack", Kind: KindChatMessage,
		SelectionDigest: strings.Repeat("1", 64), Reasons: []ReasonCountV1{}, Capabilities: []CapabilityEntryV1{},
	}
	tombstone := TombstoneV1{
		RecordType: RecordTypeTombstone, SourceRef: "source-1", Kind: KindChatMessage,
		RecordRef: "record-0", Reason: "deleted_from_source", ObservedAt: "2026-09-12T08:00:00.000000000Z",
	}
	complete := CompleteV1{
		RecordType: RecordTypeComplete, Counts: CountsV1{}, RecordsSHA256: strings.Repeat("0", 64), Truncated: false,
	}

	encoded := make([][]byte, 0, 8)
	for _, marshal := range []func() ([]byte, string, error){
		func() ([]byte, string, error) { return MarshalManifestV1(manifest) },
		func() ([]byte, string, error) { return MarshalPersonV1(person) },
		func() ([]byte, string, error) { return MarshalSourceLineV1(source) },
		func() ([]byte, string, error) { return MarshalConversationV1(conversation) },
		func() ([]byte, string, error) { return MarshalRecordV1(record) },
		func() ([]byte, string, error) { return MarshalCoverageV1(coverage) },
		func() ([]byte, string, error) { return MarshalTombstoneV1(tombstone) },
		func() ([]byte, string, error) { return MarshalCompleteV1(complete) },
	} {
		raw, digest, err := marshal()
		require.NoError(t, err)
		require.Len(t, digest, 64)
		encoded = append(encoded, raw)
	}

	want, err := os.ReadFile("testdata/msgvault-transfer-v1.golden.jsonl")
	require.NoError(t, err)
	require.Equal(t, string(want), string(append([]byte(strings.Join(byteStrings(encoded), "\n")), '\n')))

	_, _, err = DecodeManifestV1(encoded[0])
	require.NoError(t, err)
	_, _, err = DecodePersonV1(encoded[1])
	require.NoError(t, err)
	_, _, err = DecodeSourceLineV1(encoded[2])
	require.NoError(t, err)
	_, _, err = DecodeConversationV1(encoded[3])
	require.NoError(t, err)
	_, _, err = DecodeRecordV1(encoded[4])
	require.NoError(t, err)
	_, _, err = DecodeCoverageV1(encoded[5])
	require.NoError(t, err)
	_, _, err = DecodeTombstoneV1(encoded[6])
	require.NoError(t, err)
	_, _, err = DecodeCompleteV1(encoded[7])
	require.NoError(t, err)
}

func TestRequiredNestedZeroValuesStayOnWire(t *testing.T) {
	person := PersonV1{
		RecordType: RecordTypePerson, PersonRef: "person-1", UIDKind: PersonUIDKindVCard,
		DisplayName: "Ada", SelectedFields: []string{}, WithheldFields: []string{},
		ContactPoints: []ContactPointV1{{
			Kind: ContactPointKindEmail, ValueNormalized: "ada@example.test",
			ValueDisplay: "ada@example.test", IsPrimary: false,
		}},
	}
	personRaw, _, err := MarshalPersonV1(person)
	require.NoError(t, err)
	require.JSONEq(t,
		`{"contact_points":[{"is_primary":false,"kind":"email","value_display":"ada@example.test","value_normalized":"ada@example.test"}],"display_name":"Ada","person_ref":"person-1","record_type":"person","selected_fields":[],"uid_kind":"vcard_uid","withheld_fields":[]}`,
		string(personRaw),
	)
	decodedPerson, _, err := DecodePersonV1(personRaw)
	require.NoError(t, err)
	require.Equal(t, person, decodedPerson)

	record := RecordV1{
		RecordType: RecordTypeRecord, RecordRef: "attachment-1", Kind: KindAttachmentOccurrence,
		SourceRef: "source-1", RecordOrigin: RecordOriginProducerCanonical,
		Attachment: &AttachmentOccurrenceV1{
			PartKey: "part-1", Filename: "note.txt", MediaType: "text/plain", Size: 7,
			ContentSHA256: strings.Repeat("2", 64), Role: AttachmentRoleStandalone,
			Availability: AvailabilityMetadataOnly, AvailabilityReason: ReasonNotSelected,
		},
	}
	recordRaw, _, err := MarshalRecordV1(record)
	require.NoError(t, err)
	require.JSONEq(t,
		`{"attachment":{"availability":"metadata_only","availability_reason":"not_selected","blob":null,"content_sha256":"2222222222222222222222222222222222222222222222222222222222222222","filename":"note.txt","media_type":"text/plain","part_key":"part-1","role":"standalone","size":7},"kind":"attachment_occurrence","record_origin":"producer_canonical","record_ref":"attachment-1","record_type":"record","source_ref":"source-1"}`,
		string(recordRaw),
	)
	decodedRecord, _, err := DecodeRecordV1(recordRaw)
	require.NoError(t, err)
	require.Equal(t, record, decodedRecord)
}

func TestRequiredNestedZeroValueOmissionIsRejected(t *testing.T) {
	_, _, err := DecodePersonV1([]byte(
		`{"contact_points":[{"kind":"email","value_display":"ada@example.test","value_normalized":"ada@example.test"}],"display_name":"Ada","person_ref":"person-1","record_type":"person","selected_fields":[],"uid_kind":"vcard_uid","withheld_fields":[]}`,
	))
	require.Error(t, err)

	_, _, err = DecodeRecordV1([]byte(
		`{"attachment":{"availability":"metadata_only","availability_reason":"not_selected","content_sha256":"2222222222222222222222222222222222222222222222222222222222222222","filename":"note.txt","media_type":"text/plain","part_key":"part-1","role":"standalone","size":7},"kind":"attachment_occurrence","record_origin":"producer_canonical","record_ref":"attachment-1","record_type":"record","source_ref":"source-1"}`,
	))
	require.Error(t, err)
}

func TestTransferDecodersRejectNoncanonicalOrAmbiguousJSON(t *testing.T) {
	invalidRecords := [][]byte{
		[]byte(`{"kind":"chat_message","record_origin":"producer_canonical","record_ref":"r1","record_type":"record","record_type":"record","source_ref":"s1"}`),
		[]byte(`{"body_text":"\ud800","kind":"chat_message","record_origin":"producer_canonical","record_ref":"r1","record_type":"record","source_ref":"s1"}`),
		[]byte(` {"kind":"chat_message","record_origin":"producer_canonical","record_ref":"r1","record_type":"record","source_ref":"s1"}`),
		[]byte(`{"kind":"chat_message", "record_origin":"producer_canonical","record_ref":"r1","record_type":"record","source_ref":"s1"}`),
		[]byte(`{"kind":"chat_message","record_origin":"producer_canonical","record_ref":"r1","record_type":"record","source_ref":"s1","vibe":true}`),
	}
	for _, raw := range invalidRecords {
		_, _, err := DecodeRecordV1(raw)
		require.Error(t, err, string(raw))
	}

	for _, raw := range [][]byte{
		[]byte(`{"archive":{"archive_id":"a","display_name":"","system":"msgvault"},"blob_bytes":0,"blob_count":1.0,"counts":{"attachment_occurrence":0,"conversation":0,"coverage":0,"person":0,"record":0,"source":0,"tombstone":0},"created_at":"x","export_sequence":"1","format":"msgvault-transfer/1","package_id":"p","producer":{"contract_revision":1,"name":"msgvault","version":"v"},"records_sha256":"00","selection":{"excluded":[],"include_attachment_bytes":false,"include_person_records":false,"include_raw_source":false,"kinds":[],"person_field_policy":"identity_only","sources":[],"window":{"end":"","start":""}},"snapshot":{"consistency":"x","high_watermark":"","read_completed_at":"","read_started_at":"","spine_generation":0,"spine_timezone":"UTC"}}`),
		[]byte(`{"archive":{"archive_id":"a","display_name":"","system":"msgvault"},"blob_bytes":0,"blob_count":9223372036854775808,"counts":{"attachment_occurrence":0,"conversation":0,"coverage":0,"person":0,"record":0,"source":0,"tombstone":0},"created_at":"x","export_sequence":"1","format":"msgvault-transfer/1","package_id":"p","producer":{"contract_revision":1,"name":"msgvault","version":"v"},"records_sha256":"00","selection":{"excluded":[],"include_attachment_bytes":false,"include_person_records":false,"include_raw_source":false,"kinds":[],"person_field_policy":"identity_only","sources":[],"window":{"end":"","start":""}},"snapshot":{"consistency":"x","high_watermark":"","read_completed_at":"","read_started_at":"","spine_generation":0,"spine_timezone":"UTC"}}`),
	} {
		_, _, err := DecodeManifestV1(raw)
		require.Error(t, err)
	}
}

func TestProducerAndSelectionDigestsBindTheirExactCanonicalInputs(t *testing.T) {
	producer := ProducerV1{Name: "msgvault", Version: "fixture", ContractRevision: 1}
	digest, err := ProducerFingerprint(FormatV1, producer)
	require.NoError(t, err)
	require.Equal(t, "45bee5a5df1c52f6cbe243b4151bf8b8d0588410ccd22f14cd5189551b4e649e", digest)

	selection := SelectionV1{Sources: []string{"s1"}, Kinds: []Kind{KindChatMessage}, Excluded: []SelectionExcludedV1{}}
	first, err := SelectionDigest(selection)
	require.NoError(t, err)
	selection.Sources = []string{"s2"}
	second, err := SelectionDigest(selection)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
}

func TestTransferPackageCanBeImportedByExternalModule(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	require.NoError(t, err)
	temp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(temp, "go.mod"), []byte(
		"module example.test/transfer-consumer\n\ngo 1.27.0\n\nrequire go.kenn.io/docbank v0.0.0\n\nreplace go.kenn.io/docbank => "+repoRoot+"\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(temp, "main.go"), []byte(
		"package main\n\nimport transfer \"go.kenn.io/docbank/document/transfer\"\n\nfunc main() { _, _ = transfer.RecordKey(\"source\", transfer.KindEmail, \"record\") }\n",
	), 0o600))
	command := exec.Command("go", "build", "-tags", "fts5", ".")
	command.Dir = temp
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func byteStrings(values [][]byte) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}
