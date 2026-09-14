package transfer_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/transfer"
	"go.kenn.io/docbank/document/transfer/transfertest"
)

type misreportedSizeReader struct {
	transfer.PackageReader
}

type substitutedManifestReader struct {
	transfer.PackageReader

	raw   []byte
	first bool
}

func (reader *substitutedManifestReader) OpenFile(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	if name == "transfer.json" && !reader.first {
		reader.first = true
		return io.NopCloser(strings.NewReader(string(reader.raw))), int64(len(reader.raw)), nil
	}
	return reader.PackageReader.OpenFile(ctx, name)
}

type canceledFileReader struct {
	transfer.PackageReader

	name string
}

func (reader canceledFileReader) OpenFile(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	if name == reader.name {
		return nil, 0, context.Canceled
	}
	return reader.PackageReader.OpenFile(ctx, name)
}

func (reader misreportedSizeReader) WalkFiles(ctx context.Context, visit func(transfer.PackageFile) error) error {
	return reader.PackageReader.WalkFiles(ctx, func(file transfer.PackageFile) error {
		if file.Name == "transfer.json" {
			file.Size--
		}
		return visit(file)
	})
}

func TestValidateAcceptsACompleteSemanticallyLinkedPackage(t *testing.T) {
	for _, test := range []struct {
		name   string
		zipped bool
	}{{"directory", false}, {"zip", true}} {
		t.Run(test.name, func(t *testing.T) {
			path := buildValidPackage(t, test.zipped)
			reader := openPackage(t, path, test.zipped)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })

			report, err := transfer.Validate(t.Context(), reader)
			require.NoError(t, err)
			require.True(t, report.Valid)
			require.False(t, report.Partial)
			require.Equal(t, transfer.IntegritySumsOnly, report.IntegrityAuthority)
			require.Equal(t, transfer.CountsV1{
				Person: 1, Source: 1, Conversation: 1, Record: 2,
				AttachmentOccurrence: 1, Coverage: 1, Tombstone: 1,
			}, report.Counts)
			require.Zero(t, report.FindingsTotal)
		})
	}
}

func TestValidateAcceptsAnExplicitlyTruncatedPackageAsPartial(t *testing.T) {
	blob := []byte("synthetic attachment")
	spec := validPackageSpec(t, sha256String(blob), blob)
	spec.Lines = append(spec.Lines, transfer.CompleteV1{
		RecordType: transfer.RecordTypeComplete,
		Truncated:  true, Continuation: transfer.ContinuationV1("opaque-page-2"),
	})
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.NoError(t, err)
	require.True(t, report.Valid)
	require.True(t, report.Partial)
	require.Equal(t, "opaque-page-2", report.NextCursor)
}

func TestValidateRejectsSemanticAndReferenceViolations(t *testing.T) {
	blob := []byte("synthetic attachment")
	blobDigest := sha256String(blob)
	base := validPackageSpec(t, blobDigest, blob)

	tests := []struct {
		name   string
		mutate func(*transfertest.Spec)
	}{
		{"phase order", func(spec *transfertest.Spec) { spec.Lines[0], spec.Lines[1] = spec.Lines[1], spec.Lines[0] }},
		{"missing source", func(spec *transfertest.Spec) {
			record := packageRecord(t, spec)
			record.SourceRef = "missing"
			record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
			spec.Lines[3] = record
		}},
		{"duplicate source", func(spec *transfertest.Spec) {
			spec.Lines = append(spec.Lines[:2], append([]any{spec.Lines[1]}, spec.Lines[2:]...)...)
		}},
		{"unknown enum", func(spec *transfertest.Spec) {
			record := packageRecord(t, spec)
			record.Direction = "sideways"
			record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
			spec.Lines[3] = record
		}},
		{"invalid normalized digest", func(spec *transfertest.Spec) {
			record := packageRecord(t, spec)
			record.NormalizedSHA256 = strings.Repeat("0", 64)
			spec.Lines[3] = record
		}},
		{"missing included blob", func(spec *transfertest.Spec) { spec.Blobs = map[string][]byte{} }},
		{"invalid coverage arithmetic", func(spec *transfertest.Spec) {
			coverage := packageCoverage(t, spec)
			coverage.Selected = 2
			spec.Lines[6] = coverage
		}},
		{"unknown source field", func(spec *transfertest.Spec) {
			record := packageRecord(t, spec)
			record.SourceFields = jsontext.Value(`{"call_sid":"private"}`)
			record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
			spec.Lines[3] = record
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := clonePackageSpec(base)
			test.mutate(&spec)
			path := transfertest.Build(t, spec)
			reader, err := transfer.OpenDirectory(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			report, err := transfer.Validate(t.Context(), reader)
			require.Error(t, err)
			require.False(t, report.Valid)
			require.NotZero(t, report.FindingsTotal)
		})
	}
}

func TestValidateHashesFilesIndependentlyOfReaderManifest(t *testing.T) {
	path := buildValidPackage(t, false)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	recordsPath := path + string(os.PathSeparator) + "records.jsonl"
	file, err := os.OpenFile(recordsPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString("{}\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.False(t, report.Valid)
	require.Equal(t, "package_integrity_failed", report.Findings[0].Code)
}

func TestValidateReconcilesWalkedAndOpenedFileSizes(t *testing.T) {
	path := buildValidPackage(t, false)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), misreportedSizeReader{PackageReader: reader})
	require.Error(t, err)
	require.False(t, report.Valid)
}

func TestValidateBindsBlobNamesAndReferenceSizesToVerifiedBytes(t *testing.T) {
	t.Run("content digest", func(t *testing.T) {
		path := buildValidPackage(t, false)
		blob := []byte("synthetic attachment")
		digest := sha256String(blob)
		changed := []byte("synthetic different!")
		require.Len(t, changed, len(blob))
		require.NoError(t, os.WriteFile(path+"/blobs/"+digest[:2]+"/"+digest, changed, 0o600))
		sums, err := os.ReadFile(path + "/SHA256SUMS")
		require.NoError(t, err)
		sums = []byte(strings.Replace(string(sums), digest+"  blobs/", sha256String(changed)+"  blobs/", 1))
		require.NoError(t, os.WriteFile(path+"/SHA256SUMS", sums, 0o600))

		reader, err := transfer.OpenDirectory(path)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		report, err := transfer.Validate(t.Context(), reader)
		require.Error(t, err)
		require.False(t, report.Valid)
	})

	t.Run("reference size", func(t *testing.T) {
		blob := []byte("synthetic attachment")
		spec := validPackageSpec(t, sha256String(blob), blob)
		record := packageAttachmentRecord(t, &spec)
		record.Attachment.Size = 1
		record.Attachment.Blob.Size = 1
		record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
		spec.Lines[5] = record
		reader, err := transfer.OpenDirectory(transfertest.Build(t, spec))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		report, err := transfer.Validate(t.Context(), reader)
		require.Error(t, err)
		require.False(t, report.Valid)
	})
}

func TestValidateBindsDecodedManifestToVerifiedBytes(t *testing.T) {
	path := buildValidPackage(t, false)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	raw, err := os.ReadFile(path + "/transfer.json")
	require.NoError(t, err)
	raw = []byte(strings.Replace(string(raw), `"archive_id":"archive-1"`, `"archive_id":"archive-2"`, 1))

	report, err := transfer.Validate(t.Context(), &substitutedManifestReader{PackageReader: reader, raw: raw})
	require.Error(t, err)
	require.False(t, report.Valid)
}

func TestValidateRejectsUnterminatedChecksumInventory(t *testing.T) {
	path := buildValidPackage(t, false)
	file, err := os.OpenFile(path+"/SHA256SUMS", os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString("unframed junk")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.False(t, report.Valid)
}

func TestValidatePreservesCancellationFromPackageReads(t *testing.T) {
	for _, name := range []string{"transfer.json", "SHA256SUMS", "records.jsonl"} {
		t.Run(name, func(t *testing.T) {
			reader, err := transfer.OpenDirectory(buildValidPackage(t, false))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			_, err = transfer.Validate(t.Context(), canceledFileReader{PackageReader: reader, name: name})
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestValidateRejectsPersonHintFieldAndAliasConflicts(t *testing.T) {
	blob := []byte("synthetic attachment")
	tests := []struct {
		name   string
		mutate func(*transfertest.Spec)
	}{
		{"oversized local id", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.PersonLocalID = strings.Repeat("x", transfer.MaxRecordRefBytes+1)
			spec.Lines[0] = person
		}},
		{"oversized revision", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.Revision = strings.Repeat("x", transfer.MaxRecordRefBytes+1)
			spec.Lines[0] = person
		}},
		{"oversized identity revision", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.IdentityRevision = strings.Repeat("x", transfer.MaxRecordRefBytes+1)
			spec.Lines[0] = person
		}},
		{"self alias", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.RetiredUIDs = []string{person.PersonRef}
			person.SelectedFields = append(person.SelectedFields, "retired_uids")
			spec.Lines[0] = person
		}},
		{"shared alias", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.RetiredUIDs = []string{"person-old"}
			person.SelectedFields = append(person.SelectedFields, "retired_uids")
			second := person
			second.PersonRef = "person-2"
			spec.Lines = append([]any{person, second}, spec.Lines[1:]...)
		}},
		{"alias is another live identity", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.RetiredUIDs = []string{"person-2"}
			person.SelectedFields = append(person.SelectedFields, "retired_uids")
			second := person
			second.PersonRef = "person-2"
			second.RetiredUIDs = nil
			second.SelectedFields = []string{"display_name"}
			spec.Lines = append([]any{person, second}, spec.Lines[1:]...)
		}},
		{"display name withheld", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.WithheldFields = []string{"display_name"}
			person.SelectedFields = []string{}
			spec.Lines[0] = person
		}},
		{"retired uids withheld", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.RetiredUIDs = []string{"person-old"}
			person.WithheldFields = []string{"retired_uids"}
			spec.Lines[0] = person
		}},
		{"participant refs withheld", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.ParticipantRefs = []string{"participant-1"}
			person.WithheldFields = []string{"participant_refs"}
			spec.Lines[0] = person
		}},
		{"contact points withheld", func(spec *transfertest.Spec) {
			person := packagePerson(t, spec)
			person.ContactPoints = []transfer.ContactPointV1{{Kind: transfer.ContactPointKindEmail, ValueNormalized: "ada@example.test"}}
			person.WithheldFields = []string{"contact_points"}
			spec.Manifest.Selection.PersonFieldPolicy = "identity_and_contact_points"
			spec.Lines[0] = person
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validPackageSpec(t, sha256String(blob), blob)
			test.mutate(&spec)
			reader, err := transfer.OpenDirectory(transfertest.Build(t, spec))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			report, err := transfer.Validate(t.Context(), reader)
			require.Error(t, err)
			require.False(t, report.Valid)
		})
	}
}

func TestValidateClassifiesUnknownSourceFieldWithoutEchoingIt(t *testing.T) {
	blob := []byte("synthetic attachment")
	spec := validPackageSpec(t, sha256String(blob), blob)
	record := packageRecord(t, &spec)
	record.SourceFields = jsontext.Value(`{"call_sid":"private-value"}`)
	record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
	spec.Lines[3] = record
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.Equal(t, "unknown_field", report.Findings[0].Code)
	for _, finding := range report.Findings {
		require.NotContains(t, finding.Detail, "call_sid")
		require.NotContains(t, finding.Detail, "private-value")
	}
	require.NotContains(t, err.Error(), "call_sid")
	require.NotContains(t, err.Error(), "private-value")
}

func TestValidateClassifiesUnknownKindAsUnsupportedRecordKind(t *testing.T) {
	blob := []byte("synthetic attachment")
	spec := validPackageSpec(t, sha256String(blob), blob)
	coverage := packageCoverage(t, &spec)
	coverage.Kind = "message"
	spec.Lines[6] = coverage
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.Equal(t, "unsupported_record_kind", report.Findings[0].Code)
}

func TestValidateRejectsStructuredSecretsButLeavesBodyStringsOpaque(t *testing.T) {
	blob := []byte("synthetic attachment")
	spec := validPackageSpec(t, sha256String(blob), blob)
	record := packageRecord(t, &spec)
	record.BodyText = `{"token":"ordinary message text"}`
	record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
	spec.Lines[3] = record
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	report, err := transfer.Validate(t.Context(), reader)
	require.NoError(t, err)
	require.True(t, report.Valid)
	require.NoError(t, reader.Close())

	record.SourceFields = jsontext.Value(`{"token":"private-value"}`)
	record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
	spec.Lines[3] = record
	path = transfertest.Build(t, spec)
	reader, err = transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	report, err = transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.Equal(t, "forbidden_field", report.Findings[0].Code)
	require.NotContains(t, report.Findings[0].Detail, "token")
	require.NotContains(t, report.Findings[0].Detail, "private-value")
}

func TestValidateRejectsPersonLinesOutsideTheSelectedClass(t *testing.T) {
	spec := transfertest.Spec{
		Manifest: transfer.ManifestV1{
			Archive: transfer.ArchiveV1{ArchiveID: "archive-1"},
			Selection: transfer.SelectionV1{
				Sources: []string{"source-1"}, Kinds: []transfer.Kind{},
				PersonFieldPolicy: "identity_only", Excluded: []transfer.SelectionExcludedV1{},
			},
		},
		Lines: []any{
			transfer.PersonV1{RecordType: transfer.RecordTypePerson, PersonRef: "person-1", UIDKind: transfer.PersonUIDKindVCard, DisplayName: "Ada", SelectedFields: []string{"display_name"}, WithheldFields: []string{}},
			transfer.SourceLineV1{RecordType: transfer.RecordTypeSource, SourceRef: "source-1", SourceType: transfer.SourceTypeSlack, Route: "slack", Identifier: "workspace-1"},
		},
		Blobs: map[string][]byte{},
	}
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.False(t, report.Valid)
}

func TestValidateRejectsSourcesOutsideTheExactSelection(t *testing.T) {
	spec := transfertest.Spec{
		Manifest: transfer.ManifestV1{
			Archive: transfer.ArchiveV1{ArchiveID: "archive-1"},
			Selection: transfer.SelectionV1{
				Sources: []string{"source-1"}, Kinds: []transfer.Kind{},
				PersonFieldPolicy: "identity_only", Excluded: []transfer.SelectionExcludedV1{},
			},
		},
		Lines: []any{
			transfer.SourceLineV1{RecordType: transfer.RecordTypeSource, SourceRef: "source-1", SourceType: transfer.SourceTypeSlack, Route: "slack", Identifier: "workspace-1"},
			transfer.SourceLineV1{RecordType: transfer.RecordTypeSource, SourceRef: "source-2", SourceType: transfer.SourceTypeSlack, Route: "slack", Identifier: "workspace-2"},
		},
		Blobs: map[string][]byte{},
	}
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.False(t, report.Valid)
}

func TestValidateRejectsRecordKindsOutsideTheExactSelection(t *testing.T) {
	blob := []byte("synthetic attachment")
	spec := validPackageSpec(t, sha256String(blob), blob)
	record := packageRecord(t, &spec)
	record.Kind = transfer.KindVoicemail
	record.NormalizedSHA256, _ = transfer.NormalizedRecordSHA256(record)
	spec.Lines[3] = record
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.False(t, report.Valid)
}

func TestValidateRejectsOversizedPersonParticipantReferences(t *testing.T) {
	spec := transfertest.Spec{
		Manifest: transfer.ManifestV1{
			Archive: transfer.ArchiveV1{ArchiveID: "archive-1"},
			Selection: transfer.SelectionV1{
				Sources: []string{"source-1"}, Kinds: []transfer.Kind{}, IncludePersonRecords: true,
				PersonFieldPolicy: "identity_only", Excluded: []transfer.SelectionExcludedV1{},
			},
		},
		Lines: []any{
			transfer.PersonV1{RecordType: transfer.RecordTypePerson, PersonRef: "person-1", UIDKind: transfer.PersonUIDKindVCard, DisplayName: "Ada", ParticipantRefs: []string{strings.Repeat("x", transfer.MaxRecordRefBytes+1)}, SelectedFields: []string{"display_name", "participant_refs"}, WithheldFields: []string{}},
			transfer.SourceLineV1{RecordType: transfer.RecordTypeSource, SourceRef: "source-1", SourceType: transfer.SourceTypeSlack, Route: "slack", Identifier: "workspace-1"},
		},
		Blobs: map[string][]byte{},
	}
	path := transfertest.Build(t, spec)
	reader, err := transfer.OpenDirectory(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	report, err := transfer.Validate(t.Context(), reader)
	require.Error(t, err)
	require.False(t, report.Valid)
}

func buildValidPackage(t *testing.T, zipped bool) string {
	t.Helper()
	blob := []byte("synthetic attachment")
	blobDigest := sha256String(blob)
	spec := validPackageSpec(t, blobDigest, blob)
	spec.Zip = zipped
	return transfertest.Build(t, spec)
}

func openPackage(t *testing.T, path string, zipped bool) transfer.PackageReader {
	t.Helper()
	if !zipped {
		reader, err := transfer.OpenDirectory(path)
		require.NoError(t, err)
		return reader
	}
	file, err := os.Open(path)
	require.NoError(t, err)
	info, err := file.Stat()
	require.NoError(t, err)
	reader, err := transfer.OpenZip(file, info.Size())
	require.NoError(t, err)
	return reader
}

func validPackageSpec(t *testing.T, blobDigest string, blob []byte) transfertest.Spec {
	t.Helper()
	selection := transfer.SelectionV1{
		Sources:                []string{"source-1"},
		Kinds:                  []transfer.Kind{transfer.KindChatMessage, transfer.KindCall, transfer.KindAttachmentOccurrence},
		Window:                 transfer.SelectionWindowV1{Start: "2024-01-01T00:00:00Z", End: "2025-01-01T00:00:00Z"},
		IncludeAttachmentBytes: true, IncludePersonRecords: true,
		PersonFieldPolicy: "identity_only", Excluded: []transfer.SelectionExcludedV1{},
	}
	selectionDigest, err := transfer.SelectionDigest(selection)
	require.NoError(t, err)
	record := transfer.RecordV1{
		RecordType: transfer.RecordTypeRecord, RecordRef: "record-1", Kind: transfer.KindChatMessage,
		SourceRef: "source-1", ConversationRef: "conversation-1", Direction: transfer.DirectionInbound,
		Participants: []transfer.ParticipantV1{{PersonRef: "person-1", DisplayName: "Ada", Address: "ada@example.test", AddressKind: transfer.ContactPointKindEmail, EnvelopeRole: transfer.EnvelopeRoleFrom, ScopeDirection: transfer.ScopeDirectionFromPerson}},
		BodyText:     "Synthetic message", BodyMediaType: "text/plain",
		RecordOrigin: transfer.RecordOriginProducerCanonical, Attachments: []string{"attachment-1"},
		History: &transfer.HistoryV1{Edits: transfer.HistoryStateAbsent, Reactions: transfer.HistoryStateObserved, Deletions: transfer.HistoryStateAbsent},
		Dates: []transfer.DateV1{{
			Kind: transfer.DateKindMessageSent, Instant: "2024-06-01T12:00:00.000000000Z",
			Precision: transfer.PrecisionSecond, Timezone: transfer.TimezoneKindUTC, Origin: transfer.OriginSentAt,
		}},
	}
	record.NormalizedSHA256, err = transfer.NormalizedRecordSHA256(record)
	require.NoError(t, err)
	attachment := transfer.RecordV1{
		RecordType: transfer.RecordTypeRecord, RecordRef: "attachment-1", Kind: transfer.KindAttachmentOccurrence,
		SourceRef: "source-1", RecordOrigin: transfer.RecordOriginProducerCanonical,
		Attachment: &transfer.AttachmentOccurrenceV1{
			PartKey: "part-1", Filename: "synthetic.txt", MediaType: "text/plain", Size: int64(len(blob)),
			ContentSHA256: blobDigest, Role: transfer.AttachmentRoleStandalone,
			Blob:         &transfer.BlobRefV1{BlobSHA256: blobDigest, Size: int64(len(blob)), MediaType: "text/plain"},
			Availability: transfer.AvailabilityBytesIncluded,
		},
	}
	attachment.NormalizedSHA256, err = transfer.NormalizedRecordSHA256(attachment)
	require.NoError(t, err)
	call := transfer.RecordV1{
		RecordType: transfer.RecordTypeRecord, RecordRef: "call-1", Kind: transfer.KindCall,
		SourceRef: "source-1", ConversationRef: "conversation-1", Direction: transfer.DirectionInbound,
		RecordOrigin: transfer.RecordOriginProducerCanonical,
		SourceFields: jsontext.Value(`{"duration_ms":83000}`),
	}
	call.NormalizedSHA256, err = transfer.NormalizedRecordSHA256(call)
	require.NoError(t, err)
	capabilities := transfer.SourceQualificationMatrix()[5].Capabilities
	return transfertest.Spec{
		Manifest: transfer.ManifestV1{Archive: transfer.ArchiveV1{ArchiveID: "archive-1"}, Selection: selection},
		Lines: []any{
			transfer.PersonV1{RecordType: transfer.RecordTypePerson, PersonRef: "person-1", UIDKind: transfer.PersonUIDKindVCard, DisplayName: "Ada", SelectedFields: []string{"display_name"}, WithheldFields: []string{}},
			transfer.SourceLineV1{RecordType: transfer.RecordTypeSource, SourceRef: "source-1", SourceType: transfer.SourceTypeSlack, Route: "slack", Identifier: "workspace-1"},
			transfer.ConversationV1{RecordType: transfer.RecordTypeConversation, SourceRef: "source-1", ConversationRef: "conversation-1", Title: "Synthetic thread", ConversationType: "channel"},
			record,
			call,
			attachment,
			transfer.CoverageV1{RecordType: transfer.RecordTypeCoverage, SourceRef: "source-1", Route: "slack", Kind: transfer.KindChatMessage, SelectionDigest: selectionDigest, Selected: 1, Emitted: 1, RawAbsent: 1, AttachmentsSelected: 1, AttachmentsBytesIncluded: 1, Reasons: []transfer.ReasonCountV1{}, Capabilities: capabilities},
			transfer.TombstoneV1{RecordType: transfer.RecordTypeTombstone, SourceRef: "source-1", Kind: transfer.KindChatMessage, RecordRef: "record-old", Reason: "deleted_from_source", ObservedAt: "2026-09-12T08:00:00.000000000Z"},
		},
		Blobs: map[string][]byte{blobDigest: blob},
	}
}

func clonePackageSpec(source transfertest.Spec) transfertest.Spec {
	clone := source
	clone.Lines = append([]any(nil), source.Lines...)
	clone.Blobs = make(map[string][]byte, len(source.Blobs))
	for key, value := range source.Blobs {
		clone.Blobs[key] = append([]byte(nil), value...)
	}
	return clone
}

func packageRecord(t *testing.T, spec *transfertest.Spec) transfer.RecordV1 {
	t.Helper()
	record, ok := spec.Lines[3].(transfer.RecordV1)
	require.True(t, ok)
	return record
}

func packageAttachmentRecord(t *testing.T, spec *transfertest.Spec) transfer.RecordV1 {
	t.Helper()
	record, ok := spec.Lines[5].(transfer.RecordV1)
	require.True(t, ok)
	return record
}

func packagePerson(t *testing.T, spec *transfertest.Spec) transfer.PersonV1 {
	t.Helper()
	person, ok := spec.Lines[0].(transfer.PersonV1)
	require.True(t, ok)
	return person
}

func packageCoverage(t *testing.T, spec *transfertest.Spec) transfer.CoverageV1 {
	t.Helper()
	coverage, ok := spec.Lines[6].(transfer.CoverageV1)
	require.True(t, ok)
	return coverage
}

func sha256String(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
