package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
)

func TestAuthorizeReturnsOnlyValidatedUnlinkedExactBytes(t *testing.T) {
	data := []byte("alpha\nbeta\n")
	record := inspectCapability(t, data)
	directory := t.TempDir()
	var validated bool
	upload, err := Authorize(t.Context(), Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: directory,
		testHook: func(stage authorizeStage, _ string) error {
			if stage == authorizeStageValidated {
				validated = true
			}
			return nil
		},
	}, record, UploadMetadata{Filename: "notes.txt", ProviderMetadata: []byte(`{"mode":"synthetic"}`)})
	require.NoError(t, err)
	require.True(t, validated, "the adapter-facing reader cannot exist before validation completes")
	metadata := upload.Metadata()
	assert.Equal(t, record.SourceSHA256, metadata.SHA256)
	assert.Equal(t, record.Checksum, metadata.CapabilityRecordChecksum)
	assert.Equal(t, sha256Hex([]byte(`{"mode":"synthetic"}`)), metadata.ProviderMetadataChecksum)
	assert.Equal(t, "text", metadata.MediaFamily)

	got, err := io.ReadAll(upload)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	require.NoError(t, upload.Close())
	_, err = upload.Read(make([]byte, 1))
	require.Error(t, err)
	assert.Empty(t, spoolEntries(t, directory), "the named spool must be gone before handoff")
}

func TestAuthorizeRejectsPathReplacementSymlinkAndWriterMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reparse-point cases run in the platform suite")
	}
	data := []byte("authoritative bytes\n")
	record := inspectCapability(t, data)
	tests := []struct {
		name string
		hook func(string) error
	}{
		{
			name: "path replacement",
			hook: func(path string) error {
				if err := os.Rename(path, path+".original"); err != nil {
					return err
				}
				return os.WriteFile(path, data, 0o600)
			},
		},
		{
			name: "symlink replacement",
			hook: func(path string) error {
				if err := os.Rename(path, path+".original"); err != nil {
					return err
				}
				return os.Symlink(filepath.Base(path)+".original", path)
			},
		},
		{
			name: "writer mutation and second hash mismatch",
			hook: func(path string) error {
				return os.WriteFile(path, []byte("mutated bytes differ\n"), 0o600)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			_, err := Authorize(t.Context(), Source{
				Reader: io.NopCloser(bytes.NewReader(data)), Directory: directory,
				testHook: func(stage authorizeStage, path string) error {
					if stage == authorizeStageWritten {
						return tt.hook(path)
					}
					return nil
				},
			}, record, UploadMetadata{Filename: "notes.txt"})
			require.Error(t, err)
			assert.Empty(t, spoolEntries(t, directory))
		})
	}
}

func TestAuthorizeCleanupRemainsBoundToOriginalParentDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory renames with open handles are covered by Windows CI")
	}
	data := []byte("authoritative bytes\n")
	record := inspectCapability(t, data)
	root := t.TempDir()
	base := filepath.Join(root, "spool")
	relocated := filepath.Join(root, "relocated")
	require.NoError(t, os.Mkdir(base, 0o700))
	var replacementSpool string

	upload, err := Authorize(t.Context(), Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: base,
		testHook: func(stage authorizeStage, path string) error {
			if stage != authorizeStageWritten {
				return nil
			}
			if err := os.Rename(base, relocated); err != nil {
				return err
			}
			if err := os.Mkdir(base, 0o700); err != nil {
				return err
			}
			replacementSpool = filepath.Join(base, filepath.Base(filepath.Dir(path)))
			return os.Mkdir(replacementSpool, 0o700)
		},
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.NoError(t, err)
	require.NoError(t, upload.Close())
	_, err = os.Stat(replacementSpool)
	require.NoError(t, err, "cleanup must not remove a same-named directory under a replacement parent")
	_, err = os.Stat(filepath.Join(relocated, filepath.Base(replacementSpool)))
	require.ErrorIs(t, err, os.ErrNotExist, "cleanup must remove the descriptor-held original spool")
}

func TestAuthorizeRejectsMutatedCapabilityAndSource(t *testing.T) {
	data := []byte("alpha\n")
	record := inspectCapability(t, data)
	record.Measurements.TextLines++
	_, err := Authorize(t.Context(), Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: t.TempDir(),
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.ErrorContains(t, err, "capability")

	record = inspectCapability(t, data)
	_, err = Authorize(t.Context(), Source{
		Reader: io.NopCloser(strings.NewReader("omega\n")), Directory: t.TempDir(),
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.ErrorContains(t, err, "source")
}

func TestAuthorizeRejectsSerializedCapabilityBeforeReadingSource(t *testing.T) {
	data := []byte("alpha\n")
	record := inspectCapability(t, data)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	var decoded media.CapabilityRecord
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	reader := &countingReadCloser{Reader: bytes.NewReader(data)}

	_, err = Authorize(t.Context(), Source{Reader: reader, Directory: t.TempDir()}, decoded,
		UploadMetadata{Filename: "notes.txt"})
	require.ErrorContains(t, err, "local inspection authority")
	assert.Zero(t, reader.reads)
	assert.True(t, reader.didClose)
}

func TestAuthorizeCapsUntrustedSourceAtAuthorizedBytesPlusOne(t *testing.T) {
	authorized := []byte("alpha\n")
	record := inspectCapability(t, authorized)
	reader := &countingReadCloser{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20))}
	_, err := Authorize(t.Context(), Source{Reader: reader, Directory: t.TempDir()}, record,
		UploadMetadata{Filename: "notes.txt"})
	require.ErrorContains(t, err, "source")
	assert.LessOrEqual(t, reader.bytesRead, int(record.SourceBytes)+1)
}

func TestAuthorizeSourceCloseFailureDoesNotLeakAuthorizedUpload(t *testing.T) {
	data := []byte("alpha\n")
	record := inspectCapability(t, data)
	directory := t.TempDir()
	reader := &countingReadCloser{Reader: bytes.NewReader(data), closeErr: errors.New("synthetic close failure")}

	upload, err := Authorize(t.Context(), Source{Reader: reader, Directory: directory}, record,
		UploadMetadata{Filename: "notes.txt"})
	require.ErrorContains(t, err, "close source")
	assert.Nil(t, upload)
	assert.Equal(t, 1, reader.closes)
	assert.Empty(t, spoolEntries(t, directory))
}

func TestAuthorizeCancellationClosesSourceAndCleansSpool(t *testing.T) {
	record := inspectCapability(t, []byte("alpha\n"))
	directory := t.TempDir()
	reader := newBlockingReadCloser()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := Authorize(ctx, Source{Reader: reader, Directory: directory}, record,
			UploadMetadata{Filename: "notes.txt"})
		done <- err
	}()
	reader.waitStarted(t)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.True(t, reader.closed())
	assert.Empty(t, spoolEntries(t, directory))
}

func TestAuthorizeCancellationClosesReturnedUpload(t *testing.T) {
	data := []byte("alpha\n")
	record := inspectCapability(t, data)
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	upload, err := Authorize(ctx, Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: directory,
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.NoError(t, err)
	cancel()
	require.Eventually(t, func() bool {
		_, readErr := upload.Read(make([]byte, 1))
		return readErr != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, upload.Close())
	assert.Empty(t, spoolEntries(t, directory))
}

func TestAuthorizeCancellationAtValidatedHandoffCleansWithoutRacing(t *testing.T) {
	data := []byte("alpha\n")
	record := inspectCapability(t, data)
	directory := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	reader := &countingReadCloser{Reader: bytes.NewReader(data)}

	_, err := Authorize(ctx, Source{
		Reader: reader, Directory: directory,
		testHook: func(stage authorizeStage, _ string) error {
			if stage == authorizeStageValidated {
				cancel()
			}
			return nil
		},
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, reader.didClose)
	assert.Empty(t, spoolEntries(t, directory))
}

func TestRecoverStaleRemovesOnlyOwnedSpoolDirectories(t *testing.T) {
	base := t.TempDir()
	stale := filepath.Join(base, spoolDirectoryPrefix+"stale")
	require.NoError(t, os.Mkdir(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "source"), []byte("stale"), 0o600))
	unrelated := filepath.Join(base, "keep")
	require.NoError(t, os.Mkdir(unrelated, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(unrelated, "source"), []byte("keep"), 0o600))
	spoofedFile := filepath.Join(base, spoolDirectoryPrefix+"file")
	require.NoError(t, os.WriteFile(spoofedFile, []byte("keep"), 0o600))
	spoofedLink := filepath.Join(base, spoolDirectoryPrefix+"link")
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink("keep", spoofedLink))
	}

	recovered, err := RecoverStale(t.Context(), base)
	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	_, err = os.Stat(stale)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(unrelated)
	require.NoError(t, err)
	_, err = os.Lstat(spoofedFile)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		_, err = os.Lstat(spoofedLink)
		require.NoError(t, err)
	}
}

func TestRecoverStaleRejectsSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reparse-root coverage runs in the Windows platform suite")
	}
	target := t.TempDir()
	stale := filepath.Join(target, spoolDirectoryPrefix+"stale")
	require.NoError(t, os.Mkdir(stale, 0o700))
	link := filepath.Join(t.TempDir(), "spool-link")
	require.NoError(t, os.Symlink(target, link))

	_, err := RecoverStale(t.Context(), link)
	require.Error(t, err)
	_, err = os.Stat(stale)
	require.NoError(t, err)
}

func TestAuthorizeRejectsIneligibleRecordBeforeReadingSource(t *testing.T) {
	data := []byte("%PDF-1.7\n")
	policy := inspectionPolicy(data, "report.pdf", "application/pdf")
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.False(t, record.Eligible)
	reader := &countingReadCloser{Reader: bytes.NewReader(data)}
	_, err = Authorize(t.Context(), Source{Reader: reader, Directory: t.TempDir()}, record,
		UploadMetadata{Filename: "report.pdf"})
	require.ErrorContains(t, err, "ineligible")
	assert.Zero(t, reader.reads)
	assert.True(t, reader.didClose)
}

func inspectCapability(t *testing.T, data []byte) media.CapabilityRecord {
	t.Helper()
	record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "notes.txt", "text/plain"))
	require.NoError(t, err)
	require.True(t, record.Eligible)
	return record
}

func inspectionPolicy(data []byte, filename, mediaType string) media.InspectionPolicy {
	return media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType,
		ExpectedBytes: int64(len(data)), ExpectedSHA256: sha256Hex(data),
		DescriptorFingerprint: strings.Repeat("a", 64), ProfileFingerprint: strings.Repeat("b", 64),
		DisclosureFingerprint: strings.Repeat("c", 64), InputKind: document.RenditionInputOriginalFile,
		MaxSourceBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20,
		MaxEntries: 100, MaxNestingDepth: 1, MaxTextLines: 1_000, MaxCharacters: 1 << 20,
		MaxPages: 100, MaxSlides: 100, MaxSheets: 100, MaxCells: 10_000, MaxSpineItems: 1_000, MaxResources: 10_000,
	}
}

func spoolEntries(t *testing.T, base string) []string {
	t.Helper()
	entries, err := os.ReadDir(base)
	require.NoError(t, err)
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name())
	}
	return result
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type countingReadCloser struct {
	io.Reader

	reads     int
	bytesRead int
	didClose  bool
	closes    int
	closeErr  error
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	reader.reads++
	count, err := reader.Reader.Read(buffer)
	reader.bytesRead += count
	return count, err
}

func (reader *countingReadCloser) Close() error {
	reader.didClose = true
	reader.closes++
	return reader.closeErr
}

type blockingReadCloser struct {
	started chan struct{}
	once    sync.Once
	closedC chan struct{}
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{started: make(chan struct{}), closedC: make(chan struct{})}
}

func (reader *blockingReadCloser) Read([]byte) (int, error) {
	reader.once.Do(func() { close(reader.started) })
	<-reader.closedC
	return 0, errors.New("closed")
}

func (reader *blockingReadCloser) Close() error {
	select {
	case <-reader.closedC:
	default:
		close(reader.closedC)
	}
	return nil
}

func (reader *blockingReadCloser) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-reader.started:
	case <-time.After(5 * time.Second):
		t.Fatal("source read did not start")
	}
}

func (reader *blockingReadCloser) closed() bool {
	select {
	case <-reader.closedC:
		return true
	default:
		return false
	}
}
