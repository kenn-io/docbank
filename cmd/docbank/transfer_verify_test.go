package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/transfer"
	"go.kenn.io/docbank/document/transfer/transfertest"
)

func TestTransferVerifyReadsValidLocalPackageWithoutDaemon(t *testing.T) {
	homeFile := filepath.Join(t.TempDir(), "not-a-vault-directory")
	require.NoError(t, os.WriteFile(homeFile, []byte("occupied"), 0o600))
	t.Setenv("DOCBANK_HOME", homeFile)
	path := transfertest.Build(t, transfertest.Spec{
		Manifest: transfer.ManifestV1{
			Archive:   transfer.ArchiveV1{ArchiveID: "archive-1"},
			Selection: transfer.SelectionV1{Sources: []string{"source-1"}},
		},
		Lines: []any{transfer.SourceLineV1{
			RecordType: transfer.RecordTypeSource, SourceRef: "source-1",
			SourceType: transfer.SourceTypeSlack, Route: "slack", Identifier: "workspace-1",
		}},
		Blobs: map[string][]byte{},
	})

	var stdout, stderr bytes.Buffer
	resetFlags(rootCmd)
	code := runProcess([]string{"transfer", "verify", path, "--json"}, &stdout, &stderr)
	assert.Equal(t, exitSuccess, code, stderr.String())
	var report transfer.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.True(t, report.Valid)
	assert.Equal(t, transfer.PackageAuthorityFullV1, report.PackageAuthority)
}

func TestTransferVerifyWritesInvalidReportAndReturnsIntegrityExit(t *testing.T) {
	path := transfertest.Build(t, transfertest.Spec{
		Manifest: transfer.ManifestV1{Archive: transfer.ArchiveV1{ArchiveID: "archive-1"}},
		Blobs:    map[string][]byte{},
	})
	require.NoError(t, os.WriteFile(filepath.Join(path, "records.jsonl"), []byte("{}\n"), 0o600))

	var stdout, stderr bytes.Buffer
	resetFlags(rootCmd)
	code := runProcess([]string{"transfer", "verify", path, "--json"}, &stdout, &stderr)
	assert.Equal(t, exitIntegrity, code)
	var report transfer.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.False(t, report.Valid)
	assert.NotZero(t, report.FindingsTotal)
}

func TestTransferVerifyNormalizesLegacyFileWithExplicitArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(fiveLineLegacyCLIFixture), 0o600))

	var stdout, stderr bytes.Buffer
	resetFlags(rootCmd)
	code := runProcess([]string{"transfer", "verify", path, "--archive-id", "archive_synthetic", "--json"}, &stdout, &stderr)
	assert.Equal(t, exitSuccess, code, stderr.String())
	var report transfer.Report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.True(t, report.Valid)
	assert.Equal(t, transfer.PackageAuthorityLegacyCompatibility, report.PackageAuthority)
	require.NotNil(t, report.LegacyEvidence)
	assert.Equal(t, transfer.LegacyExportFormat, report.LegacyEvidence.Format)
	assert.Equal(t, []transfer.ReasonCode{
		transfer.ReasonFormatV1NoRaw,
		transfer.ReasonFormatV1NoAttachments,
		transfer.ReasonFormatV1NoPersonUID,
		transfer.ReasonFormatV1NoHistory,
	}, limitationReasons(report.FormatLimitations))
}

func limitationReasons(limitations []transfer.CapabilityEntryV1) []transfer.ReasonCode {
	result := make([]transfer.ReasonCode, len(limitations))
	for index, limitation := range limitations {
		result[index] = limitation.Reason
	}
	return result
}

const fiveLineLegacyCLIFixture = `{"record_type":"manifest","schema":"msgvault-message-export/1","msgvault_version":"v0.1.0","window":{"start":"2025-01-01T00:00:00Z","end":"2025-02-01T00:00:00Z"},"filters":{"message_types":["sms"],"sources":[{"source_type":"synctech_sms","identifier":"phone-primary"}]}}
{"record_type":"source","source_type":"synctech_sms","identifier":"phone-primary","display_name":"Synthetic phone","last_successful_sync_at":null}
{"record_type":"conversation","source_type":"synctech_sms","source_identifier":"phone-primary","id":"thread-1","title":"Synthetic thread","conversation_type":"direct_chat","parent_id":null}
{"record_type":"message","source_type":"synctech_sms","source_identifier":"phone-primary","id":"message-1","conversation_id":"thread-1","message_type":"sms","subject":"","text":"synthetic body","author":null,"occurred_at":"2025-01-02T03:04:05.1200-07:00","deleted_from_source":false}
{"record_type":"complete","counts":{"sources":1,"conversations":1,"messages":1}}
`
