// Package transfertest builds deterministic synthetic transfer packages.
package transfertest

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/transfer"
)

type Spec struct {
	Manifest transfer.ManifestV1
	Lines    []any
	Blobs    map[string][]byte
	Zip      bool
}

func Build(tb testing.TB, spec Spec) string {
	tb.Helper()
	t := tb
	directory := filepath.Join(t.TempDir(), "package")
	require.NoError(t, os.MkdirAll(directory, 0o700))

	manifest := withSyntheticDefaults(spec.Manifest)
	counts := transfer.CountsV1{}
	records := make([]byte, 0)
	lines := spec.Lines
	complete := transfer.CompleteV1{RecordType: transfer.RecordTypeComplete}
	if len(lines) > 0 {
		if supplied, ok := lines[len(lines)-1].(transfer.CompleteV1); ok {
			complete = supplied
			lines = lines[:len(lines)-1]
		}
	}
	for _, line := range lines {
		raw, err := marshalLine(line)
		require.NoError(t, err)
		records = append(records, raw...)
		records = append(records, '\n')
		addCount(t, &counts, line)
	}
	recordsHash := digest(records)
	complete.RecordType = transfer.RecordTypeComplete
	complete.Counts = counts
	complete.RecordsSHA256 = recordsHash
	completeRaw, _, err := transfer.MarshalCompleteV1(complete)
	require.NoError(t, err)
	records = append(records, completeRaw...)
	records = append(records, '\n')

	manifest.Counts = counts
	manifest.RecordsSHA256 = recordsHash
	manifest.BlobCount = int64(len(spec.Blobs))
	manifest.BlobBytes = 0
	for blobDigest, body := range spec.Blobs {
		require.Equal(t, digest(body), blobDigest, "blob map key must equal the byte digest")
		manifest.BlobBytes += int64(len(body))
	}
	manifestRaw, _, err := transfer.MarshalManifestV1(manifest)
	require.NoError(t, err)

	files := map[string][]byte{
		"transfer.json": manifestRaw,
		"records.jsonl": records,
	}
	for blobDigest, body := range spec.Blobs {
		files["blobs/"+blobDigest[:2]+"/"+blobDigest] = body
	}
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	var sums strings.Builder
	for _, name := range paths {
		_, _ = fmt.Fprintf(&sums, "%s  %s\n", digest(files[name]), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())

	paths = append(paths, "SHA256SUMS")
	for _, name := range paths {
		target := filepath.Join(directory, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
		require.NoError(t, os.WriteFile(target, files[name], 0o600))
	}
	if !spec.Zip {
		return directory
	}

	archivePath := filepath.Join(t.TempDir(), "package.zip")
	archive, err := os.Create(archivePath)
	require.NoError(t, err)
	zipWriter := zip.NewWriter(archive)
	sort.Strings(paths)
	for _, name := range paths {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		entry, createErr := zipWriter.CreateHeader(header)
		require.NoError(t, createErr)
		_, writeErr := entry.Write(files[name])
		require.NoError(t, writeErr)
	}
	require.NoError(t, zipWriter.Close())
	require.NoError(t, archive.Close())
	return archivePath
}

func withSyntheticDefaults(manifest transfer.ManifestV1) transfer.ManifestV1 {
	if manifest.Format == "" {
		manifest.Format = transfer.FormatV1
	}
	if manifest.PackageID == "" {
		manifest.PackageID = "00000000-0000-4000-8000-000000000001"
	}
	if manifest.ExportSequence == "" {
		manifest.ExportSequence = "00000000000000000001"
	}
	if manifest.Producer.Name == "" {
		manifest.Producer = transfer.ProducerV1{Name: "msgvault", Version: "synthetic-fixture", ContractRevision: transfer.ContractRevision}
	}
	if manifest.Archive.ArchiveID == "" {
		manifest.Archive.ArchiveID = "archive-synthetic-1"
	}
	if manifest.Archive.System == "" {
		manifest.Archive.System = "msgvault"
	}
	if manifest.Archive.DisplayName == "" {
		manifest.Archive.DisplayName = "Synthetic archive"
	}
	if manifest.CreatedAt == "" {
		manifest.CreatedAt = "2026-09-12T08:00:00.000000000Z"
	}
	if manifest.Selection.Sources == nil {
		manifest.Selection.Sources = []string{}
	}
	if manifest.Selection.Kinds == nil {
		manifest.Selection.Kinds = []transfer.Kind{}
	}
	if manifest.Selection.Excluded == nil {
		manifest.Selection.Excluded = []transfer.SelectionExcludedV1{}
	}
	if manifest.Selection.PersonFieldPolicy == "" {
		manifest.Selection.PersonFieldPolicy = "identity_only"
	}
	if manifest.Selection.Window.Start == "" {
		manifest.Selection.Window.Start = "2024-01-01T00:00:00Z"
	}
	if manifest.Selection.Window.End == "" {
		manifest.Selection.Window.End = "2025-01-01T00:00:00Z"
	}
	if manifest.Snapshot.Consistency == "" {
		manifest.Snapshot = transfer.SnapshotV1{
			Consistency: "single_read_transaction", SpineTimezone: "UTC", SpineGeneration: 1,
			HighWatermark:   "2026-09-12T07:59:58.000000000Z",
			ReadStartedAt:   "2026-09-12T07:59:58.000000000Z",
			ReadCompletedAt: "2026-09-12T08:00:00.000000000Z",
		}
	}
	return manifest
}

func marshalLine(line any) ([]byte, error) {
	var raw []byte
	var err error
	switch value := line.(type) {
	case transfer.PersonV1:
		raw, _, err = transfer.MarshalPersonV1(value)
	case transfer.SourceLineV1:
		raw, _, err = transfer.MarshalSourceLineV1(value)
	case transfer.ConversationV1:
		raw, _, err = transfer.MarshalConversationV1(value)
	case transfer.RecordV1:
		raw, _, err = transfer.MarshalRecordV1(value)
	case transfer.CoverageV1:
		raw, _, err = transfer.MarshalCoverageV1(value)
	case transfer.TombstoneV1:
		raw, _, err = transfer.MarshalTombstoneV1(value)
	default:
		return nil, fmt.Errorf("transfertest: unsupported line type %T", line)
	}
	return raw, err
}

func addCount(tb testing.TB, counts *transfer.CountsV1, line any) {
	tb.Helper()
	t := tb
	switch value := line.(type) {
	case transfer.PersonV1:
		counts.Person++
	case transfer.SourceLineV1:
		counts.Source++
	case transfer.ConversationV1:
		counts.Conversation++
	case transfer.RecordV1:
		if value.Kind == transfer.KindAttachmentOccurrence {
			counts.AttachmentOccurrence++
		} else {
			counts.Record++
		}
	case transfer.CoverageV1:
		counts.Coverage++
	case transfer.TombstoneV1:
		counts.Tombstone++
	default:
		require.Fail(t, "unsupported transfer line", "%T", line)
	}
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
