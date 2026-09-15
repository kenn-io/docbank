package transfer

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalLineRejectsTruncationAndCRLF(t *testing.T) {
	for _, input := range []string{"{}", "{}\r\n", strings.Repeat("x", MaxRecordLineBytes) + "\n"} {
		_, err := ReadCanonicalLine(bufio.NewReader(strings.NewReader(input)))
		require.Error(t, err)
	}
	got, err := ReadCanonicalLine(bufio.NewReader(strings.NewReader("{}\n")))
	require.NoError(t, err)
	require.Equal(t, []byte("{}"), got)
}

func TestRecordsRejectsAnUnterminatedFinalLine(t *testing.T) {
	directory := t.TempDir()
	writeTestManifest(t, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "records.jsonl"), []byte("{}\ntruncated"), 0o600))
	reader, err := OpenDirectory(t.Context(), directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })

	err = reader.Records(t.Context(), func(int, []byte) error { return nil })
	require.ErrorIs(t, err, ErrUnterminatedLine)
}

func TestTransferPackagePathRejectsWindowsAliases(t *testing.T) {
	for _, name := range []string{"C:/x", "//host/x", "a\\b", "../x", "blobs/aa/../../x", "x:stream", "CON", "a."} {
		require.Error(t, ValidatePackagePath(name), name)
	}
	require.NoError(t, ValidatePackagePath("transfer.json"))
}

func TestDirectoryReaderConfinesFilesAndStreamsRecords(t *testing.T) {
	directory := t.TempDir()
	manifest := testPackageManifest()
	manifestRaw, manifestSHA, err := MarshalManifestV1(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "transfer.json"), manifestRaw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "records.jsonl"), []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "SHA256SUMS"), []byte{}, 0o600))

	reader, err := OpenDirectory(t.Context(), directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	require.Equal(t, manifest, reader.Manifest())
	require.Equal(t, manifestSHA, reader.ManifestSHA256())

	var lines [][]byte
	require.NoError(t, reader.Records(t.Context(), func(number int, line []byte) error {
		require.Equal(t, len(lines)+1, number)
		lines = append(lines, slices.Clone(line))
		return nil
	}))
	require.Equal(t, [][]byte{[]byte("{}")}, lines)

	var files []PackageFile
	require.NoError(t, reader.WalkFiles(t.Context(), func(file PackageFile) error {
		files = append(files, file)
		return nil
	}))
	slices.SortFunc(files, func(left, right PackageFile) int { return strings.Compare(left.Name, right.Name) })
	require.Equal(t, []PackageFile{
		{Name: "SHA256SUMS", Size: 0},
		{Name: "records.jsonl", Size: 3},
		{Name: "transfer.json", Size: int64(len(manifestRaw))},
	}, files)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceledReader, err := OpenDirectory(ctx, directory)
	if canceledReader != nil {
		require.NoError(t, canceledReader.Close())
	}
	require.ErrorIs(t, err, context.Canceled)
}

func TestDirectoryReaderRejectsUnknownAndLinkedEntries(t *testing.T) {
	for _, setup := range []func(string){
		func(directory string) {
			require.NoError(t, os.WriteFile(filepath.Join(directory, "surprise"), []byte("x"), 0o600))
		},
		func(directory string) {
			require.NoError(t, os.Symlink("transfer.json", filepath.Join(directory, "records.jsonl")))
		},
	} {
		directory := t.TempDir()
		writeTestManifest(t, directory)
		setup(directory)
		_, err := OpenDirectory(t.Context(), directory)
		require.Error(t, err)
	}
}

func TestZipReaderRejectsDuplicateAndOverBoundCentralDirectories(t *testing.T) {
	var duplicate bytes.Buffer
	writer := zip.NewWriter(&duplicate)
	for range 2 {
		entry, err := writer.Create("transfer.json")
		require.NoError(t, err)
		_, err = entry.Write([]byte("{}"))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	_, err := OpenZip(t.Context(), bytes.NewReader(duplicate.Bytes()), int64(duplicate.Len()))
	require.Error(t, err)

	// An entry count over the package bound must fail from the fixed EOCD
	// preflight, before archive/zip is allowed to parse or allocate entries.
	zip64 := make([]byte, 56+20+22)
	binary.LittleEndian.PutUint32(zip64, 0x06064b50)
	binary.LittleEndian.PutUint64(zip64[4:], 44)
	binary.LittleEndian.PutUint64(zip64[24:], uint64(MaxZipEntries+1))
	binary.LittleEndian.PutUint64(zip64[32:], uint64(MaxZipEntries+1))
	locator := zip64[56:]
	binary.LittleEndian.PutUint32(locator, 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:], 0)
	binary.LittleEndian.PutUint32(locator[16:], 1)
	eocd := zip64[76:]
	binary.LittleEndian.PutUint32(eocd, 0x06054b50)
	binary.LittleEndian.PutUint16(eocd[8:], 0xffff)
	binary.LittleEndian.PutUint16(eocd[10:], 0xffff)
	binary.LittleEndian.PutUint32(eocd[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(eocd[16:], 0xffffffff)
	_, err = OpenZip(t.Context(), bytes.NewReader(zip64), int64(len(zip64)))
	require.ErrorContains(t, err, "ZIP entry limit")
}

func TestZipPreflightCountsActualCentralDirectoryHeaders(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range []string{"transfer.json", "records.jsonl", "SHA256SUMS"} {
		entry, err := writer.Create(name)
		require.NoError(t, err)
		_, err = entry.Write([]byte("x"))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	raw := archive.Bytes()
	eocd := bytes.LastIndex(raw, []byte{'P', 'K', 5, 6})
	require.NotEqual(t, -1, eocd)
	binary.LittleEndian.PutUint16(raw[eocd+8:], 1)
	binary.LittleEndian.PutUint16(raw[eocd+10:], 1)

	err := preflightZipDirectoryWithLimits(t.Context(), bytes.NewReader(raw), int64(len(raw)), 2, MaxZipDirectoryBytes)
	require.Error(t, err)
	_, err = OpenZip(t.Context(), bytes.NewReader(raw), int64(len(raw)))
	require.Error(t, err)
}

func TestZipPreflightAndArchiveParserSelectTheSameDirectoryEnd(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range []string{"transfer.json", "records.jsonl", "SHA256SUMS"} {
		_, err := writer.Create(name)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	raw := append([]byte(nil), archive.Bytes()...)
	end := len(raw) - 22
	originalEnd := append([]byte(nil), raw[end:]...)
	directoryStart := int(binary.LittleEndian.Uint32(originalEnd[16:]))
	offset := directoryStart
	for range 2 {
		offset += 46 + int(binary.LittleEndian.Uint16(raw[offset+28:])) + int(binary.LittleEndian.Uint16(raw[offset+30:])) + int(binary.LittleEndian.Uint16(raw[offset+32:]))
	}
	binary.LittleEndian.PutUint16(raw[end+8:], 1)
	binary.LittleEndian.PutUint16(raw[end+10:], 1)
	binary.LittleEndian.PutUint32(raw[end+12:], uint32(end-offset)) // #nosec G115 -- the synthetic ZIP is tiny.
	binary.LittleEndian.PutUint32(raw[end+16:], uint32(offset))     // #nosec G115 -- the synthetic ZIP is tiny.
	binary.LittleEndian.PutUint16(raw[end+20:], 23)
	raw = append(raw, originalEnd...)
	raw = append(raw, 'x')

	selectedEnd, selectedOffset, err := locateZipDirectoryEnd(t.Context(), bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	require.Equal(t, int64(end+22), selectedOffset)
	require.Equal(t, uint16(3), binary.LittleEndian.Uint16(selectedEnd[10:]))

	err = preflightZipDirectoryWithLimits(t.Context(), bytes.NewReader(raw), int64(len(raw)), 2, MaxZipDirectoryBytes)
	require.Error(t, err)
}

func TestZipPreflightUsesSmallForcedHeaderAndByteBounds(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range []string{"transfer.json", "records.jsonl", "SHA256SUMS"} {
		_, err := writer.Create(name)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	require.Error(t, preflightZipDirectoryWithLimits(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()), 2, MaxZipDirectoryBytes))
	require.Error(t, preflightZipDirectoryWithLimits(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()), MaxZipEntries, 64))
}

func TestDirectoryEnumerationAppliesSmallBoundsByBatch(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"blobs", "blobs/00", "blobs/01", "blobs/02"} {
		require.NoError(t, os.Mkdir(filepath.Join(directory, filepath.FromSlash(name)), 0o700))
	}
	root, err := os.OpenRoot(directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	err = walkDirectoryWithLimits(t.Context(), root, 3, 1, func(PackageFile) error { return nil })
	require.ErrorContains(t, err, "entry limit")
}

func TestReaderConstructorsRedactManifestKeysAndValues(t *testing.T) {
	privateManifest := []byte(`{"private-secret-key":"private-value"}`)
	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "transfer.json"), privateManifest, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "records.jsonl"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "SHA256SUMS"), nil, 0o600))
	_, err := OpenDirectory(t.Context(), directory)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-secret-key")
	require.NotContains(t, err.Error(), "private-value")
	require.NotContains(t, err.Error(), directory)

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, file := range []struct {
		name string
		body []byte
	}{{"transfer.json", privateManifest}, {"records.jsonl", nil}, {"SHA256SUMS", nil}} {
		entry, createErr := writer.Create(file.name)
		require.NoError(t, createErr)
		_, createErr = entry.Write(file.body)
		require.NoError(t, createErr)
	}
	require.NoError(t, writer.Close())
	_, err = OpenZip(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-secret-key")
	require.NotContains(t, err.Error(), "private-value")
}

func TestReaderConstructorsRedactAttackerControlledEntryNames(t *testing.T) {
	const privateName = "private-customer-name"
	directory := t.TempDir()
	writeTestManifest(t, directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "records.jsonl"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, privateName), nil, 0o600))
	_, err := OpenDirectory(t.Context(), directory)
	require.Error(t, err)
	require.NotContains(t, err.Error(), privateName)
	require.NotContains(t, err.Error(), directory)

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, name := range []string{"transfer.json", "records.jsonl", "SHA256SUMS", privateName} {
		entry, createErr := writer.Create(name)
		require.NoError(t, createErr)
		if name == "transfer.json" {
			raw, _, marshalErr := MarshalManifestV1(testPackageManifest())
			require.NoError(t, marshalErr)
			_, createErr = entry.Write(raw)
			require.NoError(t, createErr)
		}
	}
	require.NoError(t, writer.Close())
	_, err = OpenZip(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.Error(t, err)
	require.NotContains(t, err.Error(), privateName)
}

func TestOpenZipClosesOwnedSourceOnConstructorFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		size int64
	}{
		{name: "preflight", body: []byte("not a zip"), size: 9},
		{name: "archive parser", body: append([]byte("PK\x05\x06"), make([]byte, 18)...), size: 22},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &trackingReaderAt{Reader: bytes.NewReader(test.body)}
			_, err := OpenZip(t.Context(), source, test.size)
			require.Error(t, err)
			require.True(t, source.closed)
		})
	}
}

type trackingReaderAt struct {
	*bytes.Reader

	closed bool
	cancel context.CancelFunc
}

func (reader *trackingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	n, err := reader.Reader.ReadAt(p, offset)
	if reader.cancel != nil {
		reader.cancel()
		return n, context.Canceled
	}
	if err != nil {
		return n, fmt.Errorf("read synthetic ZIP: %w", err)
	}
	return n, nil
}

func (reader *trackingReaderAt) Close() error {
	reader.closed = true
	return nil
}

func TestZipReaderReadsWithoutExtractingNames(t *testing.T) {
	manifest := testPackageManifest()
	manifestRaw, manifestSHA, err := MarshalManifestV1(manifest)
	require.NoError(t, err)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, file := range []struct {
		name string
		body []byte
	}{{"transfer.json", manifestRaw}, {"records.jsonl", []byte("{}\n")}, {"SHA256SUMS", nil}} {
		entry, createErr := writer.Create(file.name)
		require.NoError(t, createErr)
		_, writeErr := entry.Write(file.body)
		require.NoError(t, writeErr)
	}
	require.NoError(t, writer.Close())

	reader, err := OpenZip(t.Context(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	require.Equal(t, manifestSHA, reader.ManifestSHA256())
	file, size, err := reader.OpenFile(t.Context(), "records.jsonl")
	require.NoError(t, err)
	body, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, int64(3), size)
	require.Equal(t, []byte("{}\n"), body)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &trackingReaderAt{Reader: bytes.NewReader(archive.Bytes()), cancel: cancel}
	canceledReader, err := OpenZip(ctx, source, int64(archive.Len()))
	if canceledReader != nil {
		require.NoError(t, canceledReader.Close())
	}
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, source.closed)
}

func testPackageManifest() ManifestV1 {
	return ManifestV1{
		Format: FormatV1, PackageID: "00000000-0000-4000-8000-000000000001",
		ExportSequence: "00000000000000000001",
		Producer:       ProducerV1{Name: "msgvault", Version: "fixture", ContractRevision: ContractRevision},
		Archive:        ArchiveV1{ArchiveID: "archive-1", System: "msgvault", DisplayName: "Fixture"},
		CreatedAt:      "2026-09-12T08:00:00.000000000Z",
		Selection:      SelectionV1{Sources: []string{}, Kinds: []Kind{}, Window: SelectionWindowV1{Start: "2026-01-01T00:00:00Z", End: "2027-01-01T00:00:00Z"}, PersonFieldPolicy: "identity_only", Excluded: []SelectionExcludedV1{}},
		Snapshot:       SnapshotV1{Consistency: "single_read_transaction", SpineTimezone: "UTC", HighWatermark: "2026-09-12T07:59:59.000000000Z", ReadStartedAt: "2026-09-12T07:59:59.000000000Z", ReadCompletedAt: "2026-09-12T08:00:00.000000000Z"},
		RecordsSHA256:  strings.Repeat("0", 64),
	}
}

func writeTestManifest(t *testing.T, directory string) {
	t.Helper()
	raw, _, err := MarshalManifestV1(testPackageManifest())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "transfer.json"), raw, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "SHA256SUMS"), nil, 0o600))
}
