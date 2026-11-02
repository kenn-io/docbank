package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

type recordingRestoreCatalog struct {
	calls     int
	records   []PackRecord
	adoptions []Adoption
	err       error
}

type cancelOnErrContext struct {
	context.Context
	cancel   context.CancelFunc
	cancelAt int
	calls    int
}

func (c *cancelOnErrContext) Err() error {
	c.calls++
	if c.calls == c.cancelAt {
		c.cancel()
	}
	return c.Context.Err()
}

func newCancelOnErrContext(cancelAt int) *cancelOnErrContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelOnErrContext{Context: ctx, cancel: cancel, cancelAt: cancelAt}
}

func (c *recordingRestoreCatalog) ReplaceRestoredPacks(
	_ context.Context,
	records []PackRecord,
	adoptions []Adoption,
) error {
	c.calls++
	c.records = append([]PackRecord(nil), records...)
	c.adoptions = append([]Adoption(nil), adoptions...)
	return c.err
}

func TestPrepareImportPublishesBeforeCatalogAuthority(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(err)
	assert.Equal([]Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
	_, err = target.Stat(importPackPath("content", packID))
	require.NoError(err)
	catalog := &recordingRestoreCatalog{}
	assert.Equal(0, catalog.calls)
	assertNoImportStaging(t, target)
}

func TestPrepareImportReusesByteIdenticalDestination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	first, err := PrepareImport(t.Context(), target, "content", input, opts)
	require.NoError(err)

	second, err := PrepareImport(t.Context(), target, "content", input, opts)

	require.NoError(err)
	assert.Equal(first.PackedHashes(), second.PackedHashes())
	assert.Equal(first.Stats(), second.Stats())
}

func TestPrepareImportReuseRequiresDurableDestinationDirectory(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	_, err := PrepareImport(t.Context(), target, "content", input, opts)
	require.NoError(t, err)
	originalSync := syncImportRootDir
	syncErr := errors.New("reused pack directory sync failed")
	finalParent := path.Dir(importPackPath("content", packID))
	syncImportRootDir = func(_ *os.Root, name string) error {
		if name == finalParent {
			return syncErr
		}
		return nil
	}
	t.Cleanup(func() { syncImportRootDir = originalSync })

	prepared, err := PrepareImport(t.Context(), target, "content", input, opts)

	assert.Nil(t, prepared)
	require.ErrorIs(t, err, syncErr)
	assertNoImportStaging(t, target)
}

func TestPrepareImportFallsBackWhenHardLinksUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	originalLink := importRootLink
	originalUnsupported := importLinkUnsupported
	importRootLink = func(*os.Root, string, string) error { return errors.New("hard links unavailable") }
	importLinkUnsupported = func(error) bool { return true }
	t.Cleanup(func() {
		importRootLink = originalLink
		importLinkUnsupported = originalUnsupported
	})
	target := openImportTarget(t)
	contents := [][]byte{[]byte("selected"), []byte("oversized selected sibling")}
	source, packID, entries := buildImportTestPack(t, contents...)
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(contents[0]))

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(err)
	assert.Empty(prepared.PackedHashes())
	assert.Equal(ImportStats{Fallbacks: []ImportFallback{{
		PackID: packID, Reason: FallbackPackPublication,
	}}}, prepared.Stats())
	_, err = target.Stat(importPackPath("content", packID))
	require.ErrorIs(err, os.ErrNotExist)
	assertNoImportStaging(t, target)
}

func TestPrepareImportConcurrentLinkUnsupportedNeverPublishes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	originalLink := importRootLink
	originalUnsupported := importLinkUnsupported
	importRootLink = func(*os.Root, string, string) error { return errors.New("hard links unavailable") }
	importLinkUnsupported = func(error) bool { return true }
	t.Cleanup(func() {
		importRootLink = originalLink
		importLinkUnsupported = originalUnsupported
	})
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	type result struct {
		prepared *PreparedImport
		err      error
	}
	results := make(chan result, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			prepared, err := PrepareImport(t.Context(), target, "content", input, opts)
			results <- result{prepared: prepared, err: err}
		})
	}
	workers.Wait()
	close(results)
	for result := range results {
		require.NoError(result.err)
		assert.Empty(result.prepared.PackedHashes())
		assert.Equal(ImportStats{Fallbacks: []ImportFallback{{
			PackID: packID, Reason: FallbackPackPublication,
		}}}, result.prepared.Stats())
	}
	_, err := target.Stat(importPackPath("content", packID))
	require.ErrorIs(err, os.ErrNotExist)
	assertNoImportStaging(t, target)
}

func TestPrepareImportConcurrentSameIDReusesAtomicWinner(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	hash := hashFromEntry(t, entries[0])
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	type result struct {
		prepared *PreparedImport
		err      error
	}
	results := make(chan result, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			prepared, err := PrepareImport(t.Context(), target, "content", input, opts)
			results <- result{prepared: prepared, err: err}
		})
	}
	workers.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result.err)
		assert.Equal(t, []Hash{hash}, result.prepared.PackedHashes())
	}
	_, err := target.Stat(importPackPath("content", packID))
	require.NoError(t, err)
	assertNoImportStaging(t, target)
}

func TestPrepareImportLinkFallbackRefusesPreexistingDestination(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	_, err := PrepareImport(t.Context(), target, "content", input, opts)
	require.NoError(err)
	final := filepath.Join(target.Name(), filepath.FromSlash(importPackPath("content", packID)))
	require.NoError(os.WriteFile(final, []byte("preexisting collision"), 0o600))
	originalLink := importRootLink
	originalUnsupported := importLinkUnsupported
	importRootLink = func(*os.Root, string, string) error { return errors.New("hard links unavailable") }
	importLinkUnsupported = func(error) bool { return true }
	t.Cleanup(func() {
		importRootLink = originalLink
		importLinkUnsupported = originalUnsupported
	})

	prepared, err := PrepareImport(t.Context(), target, "content", input, opts)

	assert.Nil(prepared)
	require.ErrorContains(err, "collision")
	data, readErr := os.ReadFile(final)
	require.NoError(readErr)
	assert.Equal([]byte("preexisting collision"), data)
}

func TestPrepareImportLinkUnsupportedPlantedFileIsNeverReplaced(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	planted := []byte("planted during publication")
	originalLink := importRootLink
	originalUnsupported := importLinkUnsupported
	importRootLink = func(root *os.Root, _, final string) error {
		require.NoError(root.WriteFile(final, planted, 0o600))
		return errors.New("hard links unavailable")
	}
	importLinkUnsupported = func(error) bool { return true }
	t.Cleanup(func() {
		importRootLink = originalLink
		importLinkUnsupported = originalUnsupported
	})

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	assert.Nil(prepared)
	require.ErrorContains(err, "collision")
	data, readErr := target.ReadFile(importPackPath("content", packID))
	require.NoError(readErr)
	assert.Equal(planted, data)
}

func TestPrepareImportBoundsSourceGrowthAfterPreflight(t *testing.T) {
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	info, err := os.Stat(source)
	require.NoError(err)
	limits := DefaultLimits()
	limits.PackBytes = info.Size()
	originalAfterOpen := importAfterSourceOpen
	importAfterSourceOpen = func(path string) error {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(path, append(data, make([]byte, 1<<20)...), 0o600)
	}
	t.Cleanup(func() { importAfterSourceOpen = originalAfterOpen })

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	require.ErrorContains(err, "source mutation")
	_, statErr := target.Stat(importPackPath("content", packID))
	require.ErrorIs(statErr, os.ErrNotExist)
	assertNoImportStaging(t, target)
}

func TestImportBoundedWriterNeverWritesBeyondLimit(t *testing.T) {
	var destination strings.Builder
	writer := &importBoundedWriter{writer: &destination, remaining: 4}

	n, err := writer.Write([]byte("ten bytes!"))

	assert.Equal(t, 4, n)
	require.ErrorIs(t, err, errImportSourceExceedsLimit)
	assert.Equal(t, "ten ", destination.String())
}

func TestPrepareImportVerifiesEligibleSelectedPayloadOnce(t *testing.T) {
	originalVerify := importVerifySelectedBlob
	var calls int
	importVerifySelectedBlob = func(ctx context.Context, reader *MaintenancePackReader, hash Hash) error {
		calls++
		stream, err := reader.OpenBlob(ctx, hash)
		if err != nil {
			return err
		}
		return errors.Join(stream.Verify(), stream.Close())
	}
	t.Cleanup(func() { importVerifySelectedBlob = originalVerify })
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))

	_, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestPrepareImportAcceptsLegacyZstdWindowWithinLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("legacy bounded window "), 128)
	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1<<20),
		zstd.WithSingleSegment(false))
	require.NoError(err)
	encoded := encoder.EncodeAll(content, nil)
	encoder.Close()

	dir := t.TempDir()
	writer, err := pack.NewWriter(dir, pack.WriterOptions{})
	require.NoError(err)
	id := pack.ComputeBlobID(content)
	_, err = writer.AppendEncoded(id, encoded, uint64(len(content)), true)
	require.NoError(err)
	source := filepath.Join(dir, writer.ID()+PackExt)
	entries, err := writer.Seal(source)
	require.NoError(err)
	reader, err := OpenMaintenancePack(source, DefaultLimits())
	require.NoError(err)
	decoded, err := reader.ReadBlob(hashFromEntry(t, entries[0]))
	require.NoError(err)
	assert.Equal(content, decoded)
	require.NoError(reader.Close())
	target := openImportTarget(t)

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: writer.ID(), SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	require.NoError(err)
	assert.Equal([]Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
}

func TestPrepareImportSurfacesStagingDirectorySyncFailure(t *testing.T) {
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	require.NoError(t, target.MkdirAll(path.Dir(importPackPath("content", packID)), 0o700))
	originalSync := syncImportRootDir
	syncErr := errors.New("staging parent sync failed")
	syncImportRootDir = func(_ *os.Root, name string) error {
		if name == "content/packs" {
			return syncErr
		}
		return nil
	}
	t.Cleanup(func() { syncImportRootDir = originalSync })

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	require.ErrorIs(t, err, syncErr)
	assertNoImportStaging(t, target)
}

func TestPrepareImportPartialFailureLeavesOnlyEarlierVerifiedOrphan(t *testing.T) {
	require := require.New(t)
	target := openImportTarget(t)
	firstSource, firstID, firstEntries := buildImportTestPack(t, []byte("first selected"))
	secondSource, secondID, secondEntries := buildImportTestPack(t, []byte("second selected"))
	second, err := os.OpenFile(secondSource, os.O_RDWR, 0)
	require.NoError(err)
	_, err = second.WriteAt([]byte{0xff}, int64(secondEntries[0].Offset))
	require.NoError(err)
	require.NoError(second.Close())

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{
		{PackID: firstID, SourcePath: firstSource, Selections: importSelections(t, firstEntries)},
		{PackID: secondID, SourcePath: secondSource, Selections: importSelections(t, secondEntries)},
	}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	require.ErrorIs(err, pack.ErrCorrupt)
	_, firstErr := target.Stat(importPackPath("content", firstID))
	require.NoError(firstErr)
	_, secondErr := target.Stat(importPackPath("content", secondID))
	require.ErrorIs(secondErr, os.ErrNotExist)
	assertNoImportStaging(t, target)
}

func TestPrepareImportRefusesPackIDCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
	opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
	_, err := PrepareImport(t.Context(), target, "content", input, opts)
	require.NoError(err)
	final := filepath.Join(target.Name(), filepath.FromSlash(importPackPath("content", packID)))
	require.NoError(os.WriteFile(final, []byte("different bytes"), 0o600))

	prepared, err := PrepareImport(t.Context(), target, "content", input, opts)

	assert.Nil(prepared)
	require.ErrorContains(err, "collision")
	data, readErr := os.ReadFile(final)
	require.NoError(readErr)
	assert.Equal([]byte("different bytes"), data)
}

func TestPrepareImportRejectsOverlappingFullFooterEntries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"), []byte("unselected"))
	data, err := os.ReadFile(source)
	require.NoError(err)
	trailerStart := len(data) - plainPackTrailerSize
	footerLen := int(binary.LittleEndian.Uint32(data[trailerStart:]))
	footerStart := trailerStart - footerLen
	secondOffset := footerStart + 4 + plainPackEntrySize + 32
	binary.LittleEndian.PutUint64(data[secondOffset:], entries[0].Offset)
	digest := sha256.Sum256(data[footerStart : trailerStart+4])
	copy(data[trailerStart+4:trailerStart+36], digest[:])
	require.NoError(os.WriteFile(source, data, 0o600))

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	assert.Nil(prepared)
	require.ErrorIs(err, pack.ErrCorrupt)
	_, statErr := target.Stat(importPackPath("content", packID))
	assert.ErrorIs(statErr, os.ErrNotExist)
}

func TestImportFooterStoredBytesIgnoresZeroLengthSpans(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset uint64
	}{
		{name: "sharing non-empty offset", offset: 100},
		{name: "inside non-empty span", offset: 105},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored, err := importFooterStoredBytes([]pack.Entry{
				{Offset: 100, StoredLen: 10, RawLen: 10},
				{Offset: test.offset, StoredLen: 0, RawLen: 0},
			})

			require.NoError(t, err)
			assert.Equal(t, int64(10), stored)
		})
	}
}

func TestPrepareImportAllowsZeroLengthFooterEntries(t *testing.T) {
	for _, test := range []struct {
		name     string
		contents [][]byte
		mutate   func(*testing.T, string, []pack.Entry)
	}{
		{
			name:     "before following frame",
			contents: [][]byte{{}, []byte("selected content")},
		},
		{
			name:     "within non-empty span",
			contents: [][]byte{[]byte("selected content"), {}},
			mutate: func(t *testing.T, packPath string, entries []pack.Entry) {
				t.Helper()
				entries[1].Offset = entries[0].Offset + 1
				mutateImportFooterEntry(t, packPath, 1, func(entry []byte) {
					binary.LittleEndian.PutUint64(entry[32:], entries[1].Offset)
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := openImportTarget(t)
			packPath, packID, entries := buildImportTestPack(t, test.contents...)
			if test.mutate != nil {
				test.mutate(t, packPath, entries)
			}

			prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries),
			}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

			require.NoError(t, err)
			assert.Len(t, prepared.PackedHashes(), len(entries))
			assert.Equal(t, ImportStats{PackedPacks: 1, PackedBlobs: len(entries)}, prepared.Stats())
		})
	}
}

func TestPreparedImportRejectsOverflowingFullFooterTotals(t *testing.T) {
	_, _, err := importCatalogPlan(preparedImportPack{
		pack:    ImportPack{PackID: pack.NewPackID()},
		entries: []pack.Entry{{Offset: pack.MinEntryOffset, StoredLen: ^uint64(0)}},
	}, time.Now())

	assert.ErrorIs(t, err, pack.ErrCorrupt)
}

func TestPreparedImportCatalogFailureLeavesPublishedOrphan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})
	require.NoError(err)
	catalogErr := errors.New("transaction failed")
	catalog := &recordingRestoreCatalog{err: catalogErr}

	err = prepared.Commit(t.Context(), catalog)

	require.ErrorIs(err, catalogErr)
	require.ErrorContains(err, "catalog")
	assert.Equal(1, catalog.calls)
	_, statErr := target.Stat(importPackPath("content", packID))
	assert.NoError(statErr)
}

func TestPreparedImportRecordsFullFooterTotalsForSelectedSubset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"), []byte("unselected sibling"))
	createdAt := time.Now().UTC().Truncate(time.Second)
	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: createdAt})
	require.NoError(err)
	catalog := &recordingRestoreCatalog{}

	require.NoError(prepared.Commit(t.Context(), catalog))

	require.Len(catalog.records, 1)
	assert.Equal(PackRecord{
		PackID: packID, EntryCount: 2,
		StoredBytes: int64(entries[0].StoredLen + entries[1].StoredLen),
		CreatedAt:   createdAt,
	}, catalog.records[0])
	require.Len(catalog.adoptions, 1)
	assert.Equal(hashFromEntry(t, entries[0]), catalog.adoptions[0].Entry.Hash)
	assert.Equal(entries[0].CRC32C, catalog.adoptions[0].Entry.CRC32C)
	assert.Equal([]string{entries[0].ID.String()}, catalog.adoptions[0].OriginalHashes)
	assert.Equal(1, catalog.calls)
}

func TestPreparedImportCommitValidatesInputsAndAllowsIdempotentRetry(t *testing.T) {
	require := require.New(t)
	var nilPrepared *PreparedImport
	require.ErrorContains(nilPrepared.Commit(t.Context(), &recordingRestoreCatalog{}), "nil")

	target := openImportTarget(t)
	source, packID, entries := buildImportTestPack(t, []byte("selected"))
	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: source, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})
	require.NoError(err)
	require.ErrorContains(prepared.Commit(t.Context(), nil), "nil")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(prepared.Commit(canceled, &recordingRestoreCatalog{}), context.Canceled)
	catalog := &recordingRestoreCatalog{}
	require.NoError(prepared.Commit(t.Context(), catalog))
	require.NoError(prepared.Commit(t.Context(), catalog))
	assert.Equal(t, 2, catalog.calls)
}

func TestPreparedImportRetryAcrossMaintainerOrphanDisposition(t *testing.T) {
	t.Run("adopted orphan is reusable", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(t.Context(), target, "content", input, opts)
		require.NoError(err)
		catalog := newMaintenanceCatalog()
		hash := hashFromEntry(t, entries[0])
		catalog.members[hash] = Reference{Hash: hash, OriginalHashes: []string{hash.String()}}

		stats := runImportMaintainer(t, target, catalog, DefaultLimits())

		assert.Equal(1, stats.PacksAdopted)
		retried, err := PrepareImport(t.Context(), target, "content", input, opts)
		require.NoError(err)
		assert.Equal([]Hash{hash}, retried.PackedHashes())
	})

	t.Run("removed orphan is recopied", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(t.Context(), target, "content", input, opts)
		require.NoError(err)

		stats := runImportMaintainer(t, target, newMaintenanceCatalog(), DefaultLimits())

		assert.Equal(1, stats.PacksRemoved)
		_, err = target.Stat(importPackPath("content", packID))
		require.ErrorIs(err, os.ErrNotExist)
		retried, err := PrepareImport(t.Context(), target, "content", input, opts)
		require.NoError(err)
		assert.Equal([]Hash{hashFromEntry(t, entries[0])}, retried.PackedHashes())
		_, err = target.Stat(importPackPath("content", packID))
		assert.NoError(err)
	})

	t.Run("oversized retained orphan is reusable with compatible target limits", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected content larger than maintenance ceiling"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(t.Context(), target, "content", input, opts)
		require.NoError(err)
		catalog := newMaintenanceCatalog()
		hash := hashFromEntry(t, entries[0])
		catalog.members[hash] = Reference{Hash: hash, OriginalHashes: []string{hash.String()}}
		maintenanceLimits := DefaultLimits()
		maintenanceLimits.BlobBytes = 8

		stats := runImportMaintainer(t, target, catalog, maintenanceLimits)

		assert.Equal(1, stats.PacksDeferredOversized)
		retried, err := PrepareImport(t.Context(), target, "content", input, opts)
		require.NoError(err)
		assert.Equal([]Hash{hash}, retried.PackedHashes())
	})

	t.Run("damaged retained orphan fails current selection verification", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		target := openImportTarget(t)
		source, packID, entries := buildImportTestPack(t, []byte("selected"))
		input := []ImportPack{{PackID: packID, SourcePath: source, Selections: importSelections(t, entries)}}
		opts := ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()}
		_, err := PrepareImport(t.Context(), target, "content", input, opts)
		require.NoError(err)
		final, err := target.OpenFile(importPackPath("content", packID), os.O_RDWR, 0)
		require.NoError(err)
		_, err = final.WriteAt([]byte{0xff}, int64(entries[0].Offset))
		require.NoError(err)
		require.NoError(final.Close())
		catalog := newMaintenanceCatalog()
		hash := hashFromEntry(t, entries[0])
		catalog.members[hash] = Reference{Hash: hash, OriginalHashes: []string{hash.String()}}

		stats := runImportMaintainer(t, target, catalog, DefaultLimits())

		assert.Equal(1, stats.PacksQuarantined)
		_, err = target.Stat(importPackPath("content", packID))
		require.NoError(err)
		prepared, err := PrepareImport(t.Context(), target, "content", input, opts)
		assert.Nil(prepared)
		assert.ErrorContains(err, "collision")
	})
}

func runImportMaintainer(t *testing.T, target *os.Root, catalog *maintenanceCatalog, limits Limits) PackStats {
	t.Helper()
	require := require.New(t)
	t.Helper()
	layout, err := NewLayout(filepath.Join(target.Name(), "content"), LayoutOptions{
		Staging: StagingStoreDirectory, StagingDir: ".staging",
	})
	require.NoError(err)
	maintainer, err := NewMaintainer(catalog, layout, MaintainerOptions{Limits: limits})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(maintainer.Close()) })
	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	return stats
}

func TestPrepareImportUsesConfiguredLimits(t *testing.T) {
	target := openImportTarget(t)
	contents := [][]byte{[]byte("small"), []byte("larger selected content")}
	path, packID, entries := buildImportTestPack(t, contents...)
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(contents[1]))

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.ElementsMatch(t, []Hash{hashFromEntry(t, entries[0]), hashFromEntry(t, entries[1])}, prepared.PackedHashes())
	assert.Equal(t, ImportStats{PackedPacks: 1, PackedBlobs: 2}, prepared.Stats())
}

func TestPrepareImportFallsBackWholePackForContainerLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
	info, err := os.Stat(path)
	require.NoError(err)
	limits := DefaultLimits()
	limits.PackBytes = info.Size() - 1

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(err)
	assert.Empty(prepared.PackedHashes())
	assert.Equal(ImportStats{
		Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackContainerLimit}},
	}, prepared.Stats())
}

func TestPrepareImportRejectsOversizedNonPackInsteadOfFallingBack(t *testing.T) {
	target := openImportTarget(t)
	path := filepath.Join(t.TempDir(), "not-a-pack")
	require.NoError(t, os.WriteFile(path, make([]byte, 1024), 0o600))
	_, packID, entries := buildImportTestPack(t, []byte("selected"))
	limits := DefaultLimits()
	limits.PackBytes = 512

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrBadMagic)
}

func TestPrepareImportRejectsForgedOversizedFooterLength(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected"))
	info, err := os.Stat(path)
	require.NoError(err)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	var forged [4]byte
	binary.LittleEndian.PutUint32(forged[:], uint32(info.Size()+1))
	_, err = f.WriteAt(forged[:], info.Size()-plainPackTrailerSize)
	require.NoError(err)
	require.NoError(f.Close())
	limits := DefaultLimits()
	limits.FooterBytes = 1

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(prepared)
	assert.ErrorIs(err, pack.ErrTruncated)
}

func TestPrepareImportRejectsForgedOversizedFooterCount(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected"))
	data, err := os.ReadFile(path)
	require.NoError(err)
	trailerStart := len(data) - plainPackTrailerSize
	footerLen := int(binary.LittleEndian.Uint32(data[trailerStart:]))
	footerStart := trailerStart - footerLen
	binary.LittleEndian.PutUint32(data[footerStart:], 2)
	require.NoError(os.WriteFile(path, data, 0o600))
	limits := DefaultLimits()
	limits.PackEntries = 1

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(prepared)
	assert.ErrorIs(err, pack.ErrCorrupt)
}

func TestPrepareImportRejectsMetadataMismatchBehindContainerLimit(t *testing.T) {
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected"))
	info, err := os.Stat(path)
	require.NoError(t, err)
	selections := importSelections(t, entries)
	selections[0].Offset++
	limits := DefaultLimits()
	limits.PackBytes = info.Size() - 1

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: selections,
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, pack.ErrCorrupt)
}

func TestPrepareImportFallsBackWholePackForValidFooterLimits(t *testing.T) {
	for _, dimension := range []string{"footer bytes", "entry count"} {
		t.Run(dimension, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			target := openImportTarget(t)
			path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
			limits := DefaultLimits()
			var reason FallbackReason
			switch dimension {
			case "footer bytes":
				info, err := os.Stat(path)
				require.NoError(err)
				data, err := os.ReadFile(path)
				require.NoError(err)
				footerLen := int64(binary.LittleEndian.Uint32(data[info.Size()-plainPackTrailerSize:]))
				limits.FooterBytes = footerLen - 1
				reason = FallbackPackFooterLimit
			case "entry count":
				limits.PackEntries = 1
				reason = FallbackPackEntryCountLimit
			}

			prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
			}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

			require.NoError(err)
			assert.Empty(prepared.PackedHashes())
			assert.Equal(ImportStats{
				Fallbacks: []ImportFallback{{PackID: packID, Reason: reason}},
			}, prepared.Stats())
		})
	}
}

func TestPrepareImportRejectsLimitedVerificationBudgetBeforeScratch(t *testing.T) {
	for _, dimension := range []string{"footer bytes", "entry count"} {
		t.Run(dimension, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			originalFooterBytes := importVerifyMaxFooterBytes
			originalEntries := importVerifyMaxEntries
			t.Cleanup(func() {
				importVerifyMaxFooterBytes = originalFooterBytes
				importVerifyMaxEntries = originalEntries
			})

			target := openImportTarget(t)
			packPath, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
			info, err := os.Stat(packPath)
			require.NoError(err)
			data, err := os.ReadFile(packPath)
			require.NoError(err)
			footerLen := uint64(binary.LittleEndian.Uint32(data[info.Size()-plainPackTrailerSize:]))
			limits := DefaultLimits()
			limits.PackBytes = info.Size() - 1
			var wantDimension LimitDimension
			var wantActual, wantLimit uint64
			switch dimension {
			case "footer bytes":
				importVerifyMaxFooterBytes = footerLen - 1
				wantDimension = LimitPackFooterBytes
				wantActual, wantLimit = footerLen, importVerifyMaxFooterBytes
			case "entry count":
				importVerifyMaxEntries = 1
				wantDimension = LimitPackEntryCount
				wantActual, wantLimit = uint64(len(entries)), importVerifyMaxEntries
			}

			prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries[:1]),
			}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

			assert.Nil(prepared)
			require.ErrorIs(err, ErrBlobTooLarge)
			var limitErr *LimitError
			require.ErrorAs(err, &limitErr)
			assert.Equal(wantDimension, limitErr.Dimension)
			assert.Equal(wantActual, limitErr.Actual)
			assert.Equal(wantLimit, limitErr.Limit)
			assertNoImportVerificationScratch(t, target)
		})
	}
}

func TestPrepareImportLimitedVerificationRejectsTruncatedFooterBeforeBudget(t *testing.T) {
	require := require.New(t)
	target := openImportTarget(t)
	packPath, packID, entries := buildImportTestPack(t, []byte("selected"))
	info, err := os.Stat(packPath)
	require.NoError(err)
	f, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err)
	var forged [4]byte
	binary.LittleEndian.PutUint32(forged[:], uint32(importVerifyMaxFooterBytes+1))
	_, err = f.WriteAt(forged[:], info.Size()-plainPackTrailerSize)
	require.NoError(err)
	require.NoError(f.Close())
	limits := DefaultLimits()
	limits.PackBytes = info.Size() - 1

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	require.ErrorIs(err, pack.ErrTruncated)
	require.NotErrorIs(err, ErrBlobTooLarge)
	assertNoImportVerificationScratch(t, target)
}

func TestPrepareImportLimitedVerificationAllowsSparseOversizedContainer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	packPath, packID, entries := buildImportTestPack(t, []byte("selected"))
	data, err := os.ReadFile(packPath)
	require.NoError(err)
	trailerStart := len(data) - plainPackTrailerSize
	footerLen := int(binary.LittleEndian.Uint32(data[trailerStart:]))
	footerStart := trailerStart - footerLen
	suffix := data[footerStart:]
	const packLimit = int64(1 << 20)
	newSize := packLimit + 1
	f, err := os.OpenFile(packPath, os.O_RDWR, 0)
	require.NoError(err)
	require.NoError(f.Truncate(newSize))
	_, err = f.WriteAt(suffix, newSize-int64(len(suffix)))
	require.NoError(err)
	require.NoError(f.Close())
	limits := DefaultLimits()
	limits.PackBytes = packLimit

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(err)
	assert.Empty(prepared.PackedHashes())
	assert.Equal(ImportStats{
		Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackContainerLimit}},
	}, prepared.Stats())
	assertNoImportVerificationScratch(t, target)
}

func TestPrepareImportFallsBackWholePackForRecognizableUnsupportedEncoding(t *testing.T) {
	for _, test := range []struct {
		name   string
		offset int64
		value  byte
	}{
		{name: "version", offset: 4, value: plainPackVersion + 1},
		{name: "flags", offset: 5, value: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			target := openImportTarget(t)
			path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(err)
			_, err = f.WriteAt([]byte{test.value}, test.offset)
			require.NoError(err)
			require.NoError(f.Close())

			prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
			}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

			require.NoError(err)
			assert.Empty(prepared.PackedHashes())
			assert.Equal(ImportStats{
				Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackEncoding}},
			}, prepared.Stats())
		})
	}
}

func TestPrepareImportFallsBackOnlyOversizedSelectedEntry(t *testing.T) {
	target := openImportTarget(t)
	contents := [][]byte{[]byte("small"), []byte("larger selected content")}
	path, packID, entries := buildImportTestPack(t, contents...)
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(contents[0]))

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(t, err)
	assert.Equal(t, []Hash{hashFromEntry(t, entries[0])}, prepared.PackedHashes())
	assert.Equal(t, ImportStats{
		PackedPacks: 1,
		PackedBlobs: 1,
		Fallbacks: []ImportFallback{{
			PackID: packID,
			Hash:   hashFromEntry(t, entries[1]),
			Reason: FallbackBlobLimit,
		}},
	}, prepared.Stats())
}

func TestPrepareImportLimitFallbackStillVerifiesSelectedPayload(t *testing.T) {
	for _, dimension := range []string{"container bytes", "footer bytes", "entry count"} {
		t.Run(dimension, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			target := openImportTarget(t)
			path, packID, entries := buildImportTestPack(t, []byte("selected content"), []byte("sibling"))
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(err)
			_, err = f.WriteAt([]byte{0xff}, int64(entries[0].Offset))
			require.NoError(err)
			require.NoError(f.Close())
			limits := DefaultLimits()
			switch dimension {
			case "container bytes":
				info, err := os.Stat(path)
				require.NoError(err)
				limits.PackBytes = info.Size() - 1
			case "footer bytes":
				data, err := os.ReadFile(path)
				require.NoError(err)
				trailerStart := len(data) - plainPackTrailerSize
				limits.FooterBytes = int64(binary.LittleEndian.Uint32(data[trailerStart:])) - 1
			case "entry count":
				limits.PackEntries = 1
			}

			prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: path, Selections: importSelections(t, entries[:1]),
			}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

			assert.Nil(prepared)
			assert.ErrorIs(err, pack.ErrCorrupt)
		})
	}
}

func TestPrepareImportLimitFallbackRejectsOverlappingFooterSpans(t *testing.T) {
	originalChunk := importVerifyIDChunkEntries
	importVerifyIDChunkEntries = 1
	t.Cleanup(func() { importVerifyIDChunkEntries = originalChunk })

	for _, dimension := range []string{"container bytes", "footer bytes", "entry count"} {
		t.Run(dimension, func(t *testing.T) {
			require := require.New(t)
			target := openImportTarget(t)
			packPath, packID, entries := buildImportTestPack(t, []byte("selected content"), []byte("overlapping sibling"))
			mutateImportFooterEntry(t, packPath, 1, func(entry []byte) {
				binary.LittleEndian.PutUint64(entry[32:], entries[0].Offset+1)
			})
			limits := DefaultLimits()
			switch dimension {
			case "container bytes":
				info, err := os.Stat(packPath)
				require.NoError(err)
				limits.PackBytes = info.Size() - 1
			case "footer bytes":
				data, err := os.ReadFile(packPath)
				require.NoError(err)
				trailerStart := len(data) - plainPackTrailerSize
				limits.FooterBytes = int64(binary.LittleEndian.Uint32(data[trailerStart:])) - 1
			case "entry count":
				limits.PackEntries = 1
			}

			prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries[:1]),
			}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

			assert.Nil(t, prepared)
			require.ErrorIs(err, pack.ErrCorrupt)
			assertNoImportVerificationScratch(t, target)
		})
	}
}

func TestPrepareImportLimitFallbackAllowsEmptyFooterSpans(t *testing.T) {
	originalChunk := importVerifyIDChunkEntries
	importVerifyIDChunkEntries = 1
	t.Cleanup(func() { importVerifyIDChunkEntries = originalChunk })

	for _, test := range []struct {
		name   string
		offset func(pack.Entry) uint64
	}{
		{name: "equal offset", offset: func(entry pack.Entry) uint64 { return entry.Offset }},
		{name: "at preceding end", offset: func(entry pack.Entry) uint64 { return entry.Offset + entry.StoredLen }},
		{name: "nested", offset: func(entry pack.Entry) uint64 { return entry.Offset + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := openImportTarget(t)
			packPath, packID, entries := buildImportTestPack(t, []byte("selected content"), []byte("empty sibling"))
			mutateImportFooterEntry(t, packPath, 1, func(entry []byte) {
				binary.LittleEndian.PutUint64(entry[32:], test.offset(entries[0]))
				binary.LittleEndian.PutUint64(entry[40:], 0)
			})
			limits := DefaultLimits()
			limits.PackEntries = 1

			prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
				PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries[:1]),
			}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

			require.NoError(t, err)
			assert.Empty(t, prepared.PackedHashes())
			assert.Equal(t, ImportStats{
				Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackEntryCountLimit}},
			}, prepared.Stats())
			assertNoImportVerificationScratch(t, target)
		})
	}
}

func TestPrepareImportLimitFallbackSkipsOversizedSelectedPayload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	content := []byte("oversized selected content")
	path, packID, entries := buildImportTestPack(t, content)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	_, err = f.WriteAt([]byte{0xff}, int64(entries[0].Offset))
	require.NoError(err)
	require.NoError(f.Close())
	info, err := os.Stat(path)
	require.NoError(err)
	limits := DefaultLimits()
	limits.PackBytes = info.Size() - 1
	limits.BlobBytes = int64(len(content) - 1)

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	require.NoError(err)
	assert.Empty(prepared.PackedHashes())
	assert.Equal(ImportStats{
		Fallbacks: []ImportFallback{{PackID: packID, Reason: FallbackPackContainerLimit}},
	}, prepared.Stats())
}

func TestPrepareImportStreamingVerifierCleansScratch(t *testing.T) {
	originalChunk := importVerifyIDChunkEntries
	importVerifyIDChunkEntries = 1
	t.Cleanup(func() { importVerifyIDChunkEntries = originalChunk })

	t.Run("success", func(t *testing.T) {
		target := openImportTarget(t)
		path, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"), []byte("third"))
		limits := DefaultLimits()
		limits.PackEntries = 1

		_, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
			PackID: packID, SourcePath: path, Selections: importSelections(t, entries[:1]),
		}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

		require.NoError(t, err)
		assertNoImportVerificationScratch(t, target)
	})

	t.Run("cross-run duplicate", func(t *testing.T) {
		require := require.New(t)
		target := openImportTarget(t)
		dir := t.TempDir()
		writer, err := pack.NewWriter(dir, pack.WriterOptions{})
		require.NoError(err)
		entry, err := writer.Append([]byte("duplicate"))
		require.NoError(err)
		_, err = writer.Append([]byte("middle"))
		require.NoError(err)
		_, err = writer.Append([]byte("duplicate"))
		require.NoError(err)
		packPath := filepath.Join(dir, writer.ID()+PackExt)
		_, err = writer.Seal(packPath)
		require.NoError(err)
		limits := DefaultLimits()
		limits.PackEntries = 1

		prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
			PackID: writer.ID(), SourcePath: packPath, Selections: importSelections(t, []pack.Entry{entry}),
		}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

		assert.Nil(t, prepared)
		require.ErrorIs(err, pack.ErrCorrupt)
		assertNoImportVerificationScratch(t, target)
	})
}

func TestPrepareImportStreamingSpanVerifierCancelsDuringMerge(t *testing.T) {
	originalChunk := importVerifyIDChunkEntries
	importVerifyIDChunkEntries = 1
	t.Cleanup(func() { importVerifyIDChunkEntries = originalChunk })
	target := openImportTarget(t)
	packPath, packID, entries := buildImportTestPack(t, []byte("first"), []byte("second"))
	limits := DefaultLimits()
	limits.PackEntries = 1
	// PrepareImport has two checkpoints, and the scan and two span spills add
	// three. The sixth check is the span merge itself.
	ctx := newCancelOnErrContext(6)
	t.Cleanup(ctx.cancel)

	prepared, err := PrepareImport(ctx, target, "content", []ImportPack{{
		PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ctx.cancelAt, ctx.calls)
	assertNoImportVerificationScratch(t, target)
}

func TestPrepareImportStreamingSpanVerifierRejectsWithinRunOverlap(t *testing.T) {
	originalChunk := importVerifyIDChunkEntries
	importVerifyIDChunkEntries = 2
	t.Cleanup(func() { importVerifyIDChunkEntries = originalChunk })
	target := openImportTarget(t)
	packPath, packID, entries := buildImportTestPack(t, []byte("selected content"), []byte("overlapping sibling"))
	mutateImportFooterEntry(t, packPath, 1, func(entry []byte) {
		binary.LittleEndian.PutUint64(entry[32:], entries[0].Offset+1)
	})
	limits := DefaultLimits()
	limits.PackEntries = 1

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: packPath, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: limits, CreatedAt: time.Now()})

	assert.Nil(t, prepared)
	require.ErrorIs(t, err, pack.ErrCorrupt)
	assertNoImportVerificationScratch(t, target)
}

func TestPrepareImportRejectsCorruptSourceInsteadOfFallingBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := openImportTarget(t)
	path, packID, entries := buildImportTestPack(t, []byte("selected content"), []byte("unselected content"))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	_, err = f.WriteAt([]byte{0xff}, int64(entries[0].Offset))
	require.NoError(err)
	require.NoError(f.Close())

	prepared, err := PrepareImport(t.Context(), target, "content", []ImportPack{{
		PackID: packID, SourcePath: path, Selections: importSelections(t, entries[:1]),
	}}, ImportOptions{Limits: DefaultLimits(), CreatedAt: time.Now()})

	assert.Nil(prepared)
	assert.ErrorIs(err, pack.ErrCorrupt)
}

func buildImportTestPack(t *testing.T, contents ...[]byte) (string, string, []pack.Entry) {
	t.Helper()
	dir := t.TempDir()
	writer, err := pack.NewWriter(dir, pack.WriterOptions{})
	require.NoError(t, err)
	for _, content := range contents {
		_, err = writer.Append(content)
		require.NoError(t, err)
	}
	path := filepath.Join(dir, writer.ID()+PackExt)
	entries, err := writer.Seal(path)
	require.NoError(t, err)
	return path, writer.ID(), entries
}

func openImportTarget(t *testing.T) *os.Root {
	t.Helper()
	target, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	return target
}

func importSelections(t *testing.T, entries []pack.Entry) []ImportSelection {
	t.Helper()
	selections := make([]ImportSelection, len(entries))
	for i, entry := range entries {
		selections[i] = ImportSelection{
			Hash:      hashFromEntry(t, entry),
			RawLen:    int64(entry.RawLen),
			Offset:    entry.Offset,
			StoredLen: entry.StoredLen,
			Flags:     uint8(entry.Flags),
		}
	}
	return selections
}

func hashFromEntry(t *testing.T, entry pack.Entry) Hash {
	t.Helper()
	hash, err := ParseHash(entry.ID.String())
	require.NoError(t, err)
	return hash
}

func mutateImportFooterEntry(t *testing.T, packPath string, index int, mutate func([]byte)) {
	t.Helper()
	data, err := os.ReadFile(packPath)
	require.NoError(t, err)
	trailerStart := len(data) - plainPackTrailerSize
	footerLen := int(binary.LittleEndian.Uint32(data[trailerStart:]))
	footerStart := trailerStart - footerLen
	entryStart := footerStart + 4 + index*plainPackEntrySize
	mutate(data[entryStart : entryStart+plainPackEntrySize])
	digest := sha256.Sum256(data[footerStart : trailerStart+4])
	copy(data[trailerStart+4:trailerStart+36], digest[:])
	require.NoError(t, os.WriteFile(packPath, data, 0o600))
}

func assertNoImportVerificationScratch(t *testing.T, target *os.Root) {
	t.Helper()
	entries, err := os.ReadDir(target.Name())
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), importVerifyScratchPrefix), entry.Name())
	}
}

func assertNoImportStaging(t *testing.T, target *os.Root) {
	t.Helper()
	packEntries, err := os.ReadDir(filepath.Join(target.Name(), "content", "packs"))
	require.NoError(t, err)
	for _, entry := range packEntries {
		assert.False(t, strings.HasSuffix(entry.Name(), ".staging"), entry.Name())
	}
}
