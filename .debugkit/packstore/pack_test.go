package packstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestPackRepairsThenPacksAndSweepsLooseContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("pack this loose content")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	require.NoError(os.MkdirAll(layout.PacksDir(), 0o700))
	staleStaging := filepath.Join(layout.PacksDir(), "interrupted.staging")
	require.NoError(os.WriteFile(staleStaging, []byte("partial"), 0o600))

	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.PacksSealed)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(int64(len(content)), stats.BytesPacked)
	assert.Zero(stats.LooseSwept, "normal source cleanup is part of packing, not the redundant-loose sweep")
	assert.NoFileExists(layout.LoosePath(hash))
	assert.NoFileExists(staleStaging)

	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
	require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
	require.NoError(os.WriteFile(layout.LoosePath(hash), content, 0o600))
	stats, err = maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.LooseSwept, "a later redundant loose copy is sweep work")
	assert.NoFileExists(layout.LoosePath(hash))
}

func TestPackMixedLooseRepresentationsUsesLogicalIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	contents := [][]byte{
		[]byte("raw packing candidate"),
		bytes.Repeat([]byte("compressed packing candidate\n"), 128),
		bytes.Repeat([]byte("duplicate physical representation\n"), 96),
	}
	hashes := make([]Hash, len(contents))

	hashes[0] = writeMaintenanceLoose(t, layout, contents[0])
	rawRelative, err := filepath.Rel(layout.Root(), layout.LoosePath(hashes[0]))
	require.NoError(err)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hashes[0], Paths: []string{filepath.ToSlash(rawRelative)}, Size: int64(len(contents[0])),
	})

	hashes[1] = hashForTest(contents[1])
	writeCompressedLooseFixture(t, layout, hashes[1], int64(len(contents[1])), contents[1], nil)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hashes[1], Paths: []string{layout.CompressedLoosePath(hashes[1])}, Size: int64(len(contents[1])),
	})

	hashes[2] = writeMaintenanceLoose(t, layout, contents[2])
	writeCompressedLooseFixture(t, layout, hashes[2], int64(len(contents[2])), contents[2], nil)
	compressedRelative, err := filepath.Rel(layout.Root(), layout.CompressedLoosePath(hashes[2]))
	require.NoError(err)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hashes[2],
		Paths: []string{
			filepath.ToSlash(compressedRelative),
			layout.LoosePath(hashes[2]),
		},
		Size: int64(len(contents[2])),
	})

	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(len(contents), stats.BlobsPacked)
	assert.Equal(int64(len(contents[0])+len(contents[1])+len(contents[2])), stats.BytesPacked)

	indexed, records := catalog.snapshot()
	require.Len(records, 1)
	require.Len(indexed, len(contents), "one pack entry per logical hash")
	for index, hash := range hashes {
		entry, found := indexed[hash]
		require.True(found)
		assert.Equal(int64(len(contents[index])), entry.RawLen, "RawLen is decoded logical length")
		got, size := readStoreTest(t, maintainer.store, hash)
		assert.Equal(int64(len(contents[index])), size)
		assert.Equal(contents[index], got)
		assert.NoFileExists(layout.LoosePath(hash))
		assert.NoFileExists(layout.CompressedLoosePath(hash))
	}
}

func TestPackTreatsNoncanonicalHashZstdPathAsLegacyRaw(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("legacy raw bytes with a compressed-looking filename")
	hash := hashForTest(content)
	legacyRelative := filepath.ToSlash(filepath.Join("legacy", hash.String()+".zst"))
	legacyPath := filepath.Join(layout.Root(), filepath.FromSlash(legacyRelative))
	require.NoError(os.MkdirAll(filepath.Dir(legacyPath), 0o700))
	require.NoError(os.WriteFile(legacyPath, content, 0o600))
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{legacyRelative}, Size: int64(len(content)),
	})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.NoFileExists(legacyPath)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackClassifiesWindowsCaseVariantCanonicalPathAsCompressed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("case-insensitive canonical compressed path\n"), 32)
	hash := hashForTest(content)
	canonical := layout.CompressedLoosePath(hash)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	variant := filepath.Join(filepath.Dir(canonical), strings.ToUpper(filepath.Base(canonical)))
	require.NotEqual(canonical, variant)
	require.NoError(os.Rename(canonical, variant))
	originalEqual := canonicalLoosePathEqual
	canonicalLoosePathEqual = func(left, right string) bool {
		return canonicalLoosePathEqualForOS("windows", left, right)
	}
	t.Cleanup(func() { canonicalLoosePathEqual = originalEqual })
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{Hash: hash, Paths: []string{variant}, Size: int64(len(content))})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestCanonicalLoosePathEqualForOS(t *testing.T) {
	canonical := filepath.Join("root", "ab", "abcdef.zst")
	caseVariant := filepath.Join("ROOT", "AB", "ABCDEF.ZST")
	noncanonical := filepath.Join("root", "legacy", "abcdef.zst")

	assert.True(t, canonicalLoosePathEqualForOS("windows", canonical, caseVariant))
	assert.False(t, canonicalLoosePathEqualForOS("linux", canonical, caseVariant))
	assert.False(t, canonicalLoosePathEqualForOS("windows", canonical, noncanonical))
}

func TestPackMergesDuplicateCandidateFallbackPathsAndAliases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("duplicate candidate fallback\n"), 64)
	hash := hashForTest(content)
	rawPath := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, bytes.Repeat([]byte{'x'}, len(content)), 0o600))
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	base := newMaintenanceCatalog()
	base.members[hash] = Reference{Hash: hash}
	catalog := &listedCandidateCatalog{
		maintenanceCatalog: base,
		listed: []Candidate{
			{
				Hash: hash, Size: int64(len(content)),
				Paths:          []string{filepath.ToSlash(filepath.Join("missing", hash.String())), rawPath},
				OriginalHashes: []string{"alias-a"},
			},
			{
				Hash: hash, Size: int64(len(content)),
				Paths:          []string{layout.CompressedLoosePath(hash)},
				OriginalHashes: []string{"alias-b", "alias-a"},
			},
		},
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	require.Len(catalog.recorded, 1)
	assert.Equal([]string{"alias-a", "alias-b"}, catalog.recorded[0].OriginalHashes)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackMergesAliasesAfterFirstSuccessfulDuplicateCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("duplicate alias union\n"), 32)
	hash := writeMaintenanceLoose(t, layout, content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	base := newMaintenanceCatalog()
	base.members[hash] = Reference{Hash: hash}
	catalog := &listedCandidateCatalog{
		maintenanceCatalog: base,
		listed: []Candidate{
			{Hash: hash, Size: int64(len(content)), Paths: []string{layout.LoosePath(hash)}, OriginalHashes: []string{"first"}},
			{Hash: hash, Size: int64(len(content)), Paths: []string{layout.CompressedLoosePath(hash)}, OriginalHashes: []string{"second"}},
		},
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	require.Len(catalog.recorded, 1)
	assert.Equal([]string{"first", "second"}, catalog.recorded[0].OriginalHashes)
}

func TestPackRejectsContradictoryDuplicateCandidateSizes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("contradictory candidate metadata")
	hash := writeMaintenanceLoose(t, layout, content)
	base := newMaintenanceCatalog()
	base.members[hash] = Reference{Hash: hash}
	catalog := &listedCandidateCatalog{
		maintenanceCatalog: base,
		listed: []Candidate{
			{Hash: hash, Size: int64(len(content)), Paths: []string{layout.LoosePath(hash)}},
			{Hash: hash, Size: int64(len(content)) + 1, Paths: []string{layout.LoosePath(hash)}},
		},
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsCorrupt)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(layout.LoosePath(hash))
	assert.Empty(catalog.recorded)
}

func TestPackRejectsCorruptCompressedCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("verify compressed candidate\n"), 64)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, func(physical []byte) []byte {
		physical[len(physical)-1] ^= 0xff
		return physical
	})
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{layout.CompressedLoosePath(hash)}, Size: int64(len(content)),
	})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsCorrupt)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(layout.CompressedLoosePath(hash), "corrupt evidence is preserved")
}

func TestPackPreservesCompressedSourceReplacementAfterCatalogCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("source replacement race\n"), 128)
	hash := hashForTest(content)
	path := layout.CompressedLoosePath(hash)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	physical, err := os.Stat(path)
	require.NoError(err)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{path}, Size: int64(len(content)),
	})
	replacement := bytes.Repeat([]byte{0xa5}, int(physical.Size()))
	catalog.commitHook = func() {
		require.NoError(os.Remove(path))
		require.NoError(os.WriteFile(path, replacement, 0o600))
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(err, errIdentityChanged)
	assert.Equal(1, stats.BlobsPacked)
	assert.Equal(replacement, mustReadFile(t, path), "cleanup must not unlink a replacement inode")
	assertNoLooseRemovalClaims(t, path)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestSweepLooseSkipsPackedAuthorityWhenNoCanonicalLooseCandidateExists(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	base := newMaintenanceCatalog()
	var first Hash
	for index := range 64 {
		hash, err := ParseHash(fmt.Sprintf("%064x", index+1))
		require.NoError(err)
		if index == 0 {
			first = hash
		}
		base.members[hash] = Reference{Hash: hash}
		base.entries[hash] = IndexEntry{Hash: hash, PackID: pack.NewPackID()}
	}
	require.NoError(os.MkdirAll(layout.LoosePath(first), 0o700))
	symlink := layout.CompressedLoosePath(first)
	if err := os.Symlink("elsewhere", symlink); err != nil {
		t.Logf("symlink fixture unavailable: %v", err)
	}
	catalog := &countingResolveCatalog{maintenanceCatalog: base}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	var stats PackStats

	err := maintainer.sweepLoose(t.Context(), base.members, true, &stats)

	require.NoError(err)
	assert.Zero(catalog.resolveCalls, "an absent loose namespace must not open packed authority")
	assert.Equal(PackStats{}, stats)
	assert.DirExists(layout.LoosePath(first), "non-regular canonical-looking entries stay untouched")
}

func TestSweepLooseVerifiesPackedAuthorityForCanonicalLooseCandidate(t *testing.T) {
	for _, encoding := range []LooseEncoding{LooseEncodingRaw, LooseEncodingZstd} {
		t.Run(fmt.Sprint(encoding), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			layout := layoutForStoreTest(t)
			content := bytes.Repeat([]byte("redundant packed authority\n"), 32)
			entry := buildStoreTestPack(t, layout, content)
			if encoding == LooseEncodingRaw {
				require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
			} else {
				writeCompressedLooseFixture(t, layout, entry.Hash, int64(len(content)), content, nil)
			}
			base := newMaintenanceCatalog()
			base.members[entry.Hash] = Reference{Hash: entry.Hash}
			base.entries[entry.Hash] = entry
			base.packs[entry.PackID] = PackRecord{
				PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
			}
			catalog := &countingResolveCatalog{maintenanceCatalog: base}
			maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
			var stats PackStats

			err := maintainer.sweepLoose(t.Context(), base.members, true, &stats)

			require.NoError(err)
			assert.Equal(1, catalog.resolveCalls, "a loose candidate requires packed authority verification")
			assert.Equal(1, stats.LooseSwept)
			assert.NoFileExists(layout.LoosePath(entry.Hash))
			assert.NoFileExists(layout.CompressedLoosePath(entry.Hash))
		})
	}
}

func TestSweepLooseDoesNotReportValidSourceCorruptWhenRemovalPinIsUnavailable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("valid redundant loose source without removal authority")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.openIdentityPin = func(string) (identityPin, fs.FileInfo, error) {
		return nil, nil, fs.ErrPermission
	}
	var stats PackStats

	err := maintainer.sweepLoose(t.Context(), catalog.members, true, &stats)

	require.NoError(err)
	assert.Zero(stats.BlobsCorrupt)
	assert.Zero(stats.LooseSwept)
	assert.FileExists(layout.LoosePath(entry.Hash))
}

func TestPackDoesNotRequireRemovalAuthorityForReadableCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("pack readable source without removal authority")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog := newMaintenanceCatalog()
	catalog.addLoose(hash, layout.LoosePath(hash))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.openIdentityPin = func(string) (identityPin, fs.FileInfo, error) {
		return nil, nil, fs.ErrPermission
	}

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.Zero(stats.BlobsCorrupt)
	assert.FileExists(layout.LoosePath(hash), "unavailable cleanup preserves the redundant loose source")
	location, err := catalog.Resolve(t.Context(), hash)
	require.NoError(err)
	require.NotNil(location.Pack)
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackSourcePinLimitRotatesWithoutLeakingPins(t *testing.T) {
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	var order []Hash
	for index := range 10 {
		content := fmt.Appendf(nil, "tiny source %02d", index)
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
		order = append(order, hash)
	}
	catalog.setCandidateOrder(order)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.packedSourcePinLimit = 3
	opened, closed := 0, 0
	baseOpen := maintainer.openVerificationPin
	maintainer.openVerificationPin = func(path string) (identityPin, fs.FileInfo, error) {
		pin, identity, err := baseOpen(path)
		if err != nil {
			return nil, nil, err
		}
		opened++
		return &observedIdentityPin{identityPin: pin, closed: &closed}, identity, nil
	}

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(t, err)
	assert.Equal(10, stats.BlobsPacked)
	assert.Equal(4, stats.PacksSealed)
	assert.Equal(opened, closed)
	assert.Equal(10, opened)
}

func TestPackedSourcePinLimitForSoftLimit(t *testing.T) {
	for _, tt := range []struct {
		name string
		soft uint64
		want int
	}{
		{name: "below reserve", soft: 64, want: 1},
		{name: "small process", soft: 256, want: 64},
		{name: "common conservative limit", soft: 1_024, want: 448},
		{name: "target-derived ceiling", soft: 10_000, want: 4_096},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, packedSourcePinLimitForSoftLimit(tt.soft))
		})
	}
}

func TestPackedSourcePinLimitForReportedSoftLimitDistinguishesZeroFromInvalid(t *testing.T) {
	assert.Equal(t, 1, packedSourcePinLimitForReportedSoftLimit(0, false),
		"zero is a valid soft limit with no descriptors available for source pins")
	assert.Equal(t, fallbackPackedSourcePins, packedSourcePinLimitForReportedSoftLimit(^uint64(0), true),
		"a signed negative or infinity sentinel remains an invalid report after conversion")
	assert.Equal(t, 64, packedSourcePinLimitForReportedSoftLimit(256, false))
}

func TestNormalizePackedSourceSoftLimitRecognizesPortableSentinels(t *testing.T) {
	assert := assert.New(t)
	soft, invalid := normalizePackedSourceSoftLimit(uint64(0))
	assert.Equal(uint64(0), soft)
	assert.False(invalid)

	soft, invalid = normalizePackedSourceSoftLimit(int64(-1))
	assert.Equal(^uint64(0), soft)
	assert.True(invalid)

	soft, invalid = normalizePackedSourceSoftLimit(^uint64(0))
	assert.Equal(^uint64(0), soft)
	assert.True(invalid)

	soft, invalid = normalizePackedSourceSoftLimit(int64(^uint64(0) >> 1))
	assert.Equal(^uint64(0)>>1, soft)
	assert.True(invalid)

	soft, invalid = normalizePackedSourceSoftLimit(int64(256))
	assert.Equal(uint64(256), soft)
	assert.False(invalid)
}

func TestPackTargetDerivedSourcePinLimitKeepsThousandTinySourcesTogether(t *testing.T) {
	if testing.Short() {
		t.Skip("creates enough source files to protect the normal packing resource tradeoff")
	}
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	var order []Hash
	for index := range 1_000 {
		content := fmt.Appendf(nil, "default pin budget tiny source %04d", index)
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
		order = append(order, hash)
	}
	catalog.setCandidateOrder(order)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.packedSourcePinLimit = maxPackedSourcePins

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1_000, stats.BlobsPacked)
	assert.Equal(t, 1, stats.PacksSealed, "the normal resource cap must not fragment ordinary tiny-object packs")
}

func TestPackClosesSourcePinsWhenRecordPackFails(t *testing.T) {
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("source pin closes after catalog failure")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	recordErr := errors.New("injected RecordPack failure")
	catalog.recordErr = recordErr
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	opened, closed := 0, 0
	baseOpen := maintainer.openVerificationPin
	maintainer.openVerificationPin = func(path string) (identityPin, fs.FileInfo, error) {
		pin, identity, err := baseOpen(path)
		if err != nil {
			return nil, nil, err
		}
		opened++
		return &observedIdentityPin{identityPin: pin, closed: &closed}, identity, nil
	}

	_, err := maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(t, err, recordErr)
	assert.Equal(1, opened)
	assert.Equal(opened, closed)
	assert.FileExists(layout.LoosePath(hash))
}

func TestPackReportsSourcePinCloseFailure(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("source pin close errors remain visible")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	closeErr := errors.New("injected source pin close failure")
	baseOpen := maintainer.openVerificationPin
	maintainer.openVerificationPin = func(path string) (identityPin, fs.FileInfo, error) {
		pin, identity, err := baseOpen(path)
		if err != nil {
			return nil, nil, err
		}
		return &observedIdentityPin{identityPin: pin, closeErr: closeErr}, identity, nil
	}

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(t, err, closeErr)
	assert.Equal(t, 1, stats.BlobsPacked, "catalog commit remains authoritative despite cleanup failure")
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(t, content, got)
}

func TestPackCancellationDuringCompressedCandidateCleansScratch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("cancel compressed candidate\n"), 4096)
	hash := hashForTest(content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{layout.CompressedLoosePath(hash)}, Size: int64(len(content)),
	})
	ctx, cancel := context.WithCancel(t.Context())
	originalReader := newLooseZstdReader
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		reader, err := originalReader(src)
		if err != nil {
			return nil, err
		}
		return &cancelAfterFirstLooseRead{looseZstdReader: reader, cancel: cancel}, nil
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	opened, closed := 0, 0
	baseOpen := maintainer.openVerificationPin
	maintainer.openVerificationPin = func(path string) (identityPin, fs.FileInfo, error) {
		pin, identity, err := baseOpen(path)
		if err != nil {
			return nil, nil, err
		}
		opened++
		return &observedIdentityPin{identityPin: pin, closed: &closed}, identity, nil
	}

	_, err := maintainer.Pack(ctx, PackOptions{})

	require.ErrorIs(err, context.Canceled)
	assert.Equal(opened, closed, "cancellation closes every acquired source pin")
	assert.Equal(1, opened)
	assert.FileExists(layout.CompressedLoosePath(hash))
	indexed, records := catalog.snapshot()
	assert.Empty(indexed)
	assert.Empty(records)
	var scratch []string
	require.NoError(filepath.WalkDir(layout.PacksDir(), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			scratch = append(scratch, path)
		}
		return nil
	}))
	assert.Empty(scratch)
}

func TestPackCancellationBetweenCandidatePathsClosesSelectedPin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("cancel between candidate paths\n"), 128)
	hash := writeMaintenanceLoose(t, layout, content)
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash,
		Paths: []string{
			layout.CompressedLoosePath(hash),
			layout.LoosePath(hash),
		},
		Size: int64(len(content)),
	})
	ctx, cancel := context.WithCancel(t.Context())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	maintainer.beforeCandidatePath = func(index int) {
		if index == 1 {
			cancel()
		}
	}
	opened, closed := 0, 0
	closeErr := errors.New("selected pin close failure")
	baseOpen := maintainer.openVerificationPin
	maintainer.openVerificationPin = func(path string) (identityPin, fs.FileInfo, error) {
		pin, identity, err := baseOpen(path)
		if err != nil {
			return nil, nil, err
		}
		opened++
		return &observedIdentityPin{identityPin: pin, closed: &closed, closeErr: closeErr}, identity, nil
	}

	_, err := maintainer.Pack(ctx, PackOptions{})

	require.ErrorIs(err, context.Canceled)
	require.ErrorIs(err, closeErr)
	assert.Equal(opened, closed, "the selected source pin closes on top-of-loop cancellation")
	assert.Equal(1, opened)
	assert.FileExists(layout.LoosePath(hash))
	assert.FileExists(layout.CompressedLoosePath(hash))
}

func TestPackPreservesDualCopiesWhenNeitherVerifies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("expected dual-copy logical bytes")
	hash := hashForTest(content)
	rawPath := layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, []byte("different dual-copy raw bytes"), 0o600))
	writeCompressedLooseFixture(t, layout, hash, int64(len(content)), []byte("different compressed bytes!!!!"), nil)
	compressedPath := layout.CompressedLoosePath(hash)
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{compressedPath, rawPath}, Size: int64(len(content)),
	})
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsCorrupt)
	assert.Zero(stats.BlobsPacked)
	assert.FileExists(rawPath)
	assert.FileExists(compressedPath)
}

func TestPackPreservesSoleValidAndDiagnosticCopiesUntilAdoption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("sole valid loose representation\n"), 32)
	hash := writeMaintenanceLoose(t, layout, content)
	rawPath := layout.LoosePath(hash)
	compressedPath := layout.CompressedLoosePath(hash)
	corruptCompressed := []byte("corrupt compressed diagnostic evidence")
	require.NoError(os.WriteFile(compressedPath, corruptCompressed, 0o600))
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{rawPath, compressedPath}, Size: int64(len(content)),
	})
	catalog.recordErr = errors.New("injected adoption failure")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Pack(t.Context(), PackOptions{})
	require.ErrorContains(err, "injected adoption failure")
	assert.Equal(content, mustReadFile(t, rawPath), "the sole valid copy remains authoritative")
	assert.Equal(corruptCompressed, mustReadFile(t, compressedPath), "diagnostic evidence is preserved")

	catalog.recordErr = nil
	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.PacksAdopted, "the verified orphan remains authoritative over the unreadable preferred loose copy")
	assert.Zero(stats.PacksRemoved)
	assert.Zero(stats.BlobsPacked)
	assert.NoFileExists(rawPath, "the adopted pack now verifies as another logical representation")
	assert.Equal(corruptCompressed, mustReadFile(t, compressedPath), "corrupt diagnostic evidence remains")
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestPackSweepsBothVerifiedLooseRepresentations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("redundant packed content\n"), 32)
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	writeCompressedLooseFixture(t, layout, entry.Hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(2, stats.LooseSwept)
	assert.NoFileExists(layout.LoosePath(entry.Hash))
	assert.NoFileExists(layout.CompressedLoosePath(entry.Hash))
}

func TestPackSweepReturnsRemovalFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("report redundant loose removal failure")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
	removeErr := errors.New("injected loose sweep removal failure")
	originalRemove := removePinnedLooseClaim
	removePinnedLooseClaim = func(pin identityPin, path string) error {
		if filepath.Base(path) == "claimed" && strings.HasPrefix(
			filepath.Base(filepath.Dir(path)),
			"."+filepath.Base(layout.LoosePath(entry.Hash))+".remove-",
		) {
			return removeErr
		}
		return originalRemove(pin, path)
	}
	t.Cleanup(func() { removePinnedLooseClaim = originalRemove })
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(err, removeErr)
	assert.FileExists(layout.LoosePath(entry.Hash))
}

func TestPackRepacksCompressedOnlyAuthorityWhenIndexedPackIsCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("compressed-only recovery authority\n"), 32)
	entry := buildStoreTestPack(t, layout, content)
	writeCompressedLooseFixture(t, layout, entry.Hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: entry.Hash, Paths: []string{layout.CompressedLoosePath(entry.Hash)}, Size: int64(len(content)),
	})
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	path := layout.PackPath(entry.PackID)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	_, err = f.WriteAt([]byte{0xff}, entry.Offset)
	require.NoError(err)
	require.NoError(f.Close())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(int64(1), stats.MappingsPruned)
	assert.Equal(1, stats.BlobsPacked)
	got, _ := readStoreTest(t, maintainer.store, entry.Hash)
	assert.Equal(content, got)
}

func TestPackReconcilePreservesCompressedOnlyAuthorityOverOrphanPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("compressed authority over orphan pack\n"), 32)
	entry := buildStoreTestPack(t, layout, content)
	writeCompressedLooseFixture(t, layout, entry.Hash, int64(len(content)), content, nil)
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.PacksRemoved)
	assert.Zero(stats.PacksAdopted)
	assert.NoFileExists(layout.PackPath(entry.PackID))
	assert.FileExists(layout.CompressedLoosePath(entry.Hash))
}

func TestPackReconcileDoesNotTreatRawAsAuthoritativeAfterPreferredCompressedCorruption(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := bytes.Repeat([]byte("orphan pack remains readable authority\n"), 32)
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	writeCompressedLooseFixture(
		t,
		layout,
		entry.Hash,
		int64(len(content)),
		bytes.Repeat([]byte{'x'}, len(content)),
		nil,
	)
	catalog := newMaintenanceCatalog()
	catalog.members[entry.Hash] = Reference{Hash: entry.Hash}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Zero(stats.PacksRemoved)
	assert.Equal(1, stats.PacksAdopted)
	assert.FileExists(layout.PackPath(entry.PackID))
	got, _ := readStoreTest(t, maintainer.store, entry.Hash)
	assert.Equal(content, got)
}

func TestMaintenancePreflightsCompressedStoredSizeBeforeDecode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("small maintenance object")
	hash := hashForTest(content)
	path := layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	header := encodeCompressedLooseHeader(uint64(len(content)))
	physical := append(header[:], bytes.Repeat([]byte("oversized stored payload"), 4)...)
	require.NoError(os.WriteFile(path, physical, 0o600))
	originalReader := newLooseZstdReader
	decoderCalls := 0
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		decoderCalls++
		return originalReader(src)
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })
	limit := int64(len(content) + 1)

	_, err := verifyLoosePathIdentity(t.Context(), path, hash, limit, LooseEncodingZstd)

	var limitErr *LimitError
	require.ErrorAs(err, &limitErr)
	assert.Equal(LimitBlobStoredBytes, limitErr.Dimension)
	assert.Equal(uint64(len(physical)), limitErr.Actual)
	assert.Equal(uint64(limit), limitErr.Limit)
	assert.Zero(decoderCalls)
}

func TestPackDefersCompressedCandidateAboveStoredLimitBeforeDecode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	content := []byte("small pack candidate")
	hash := hashForTest(content)
	path := layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	header := encodeCompressedLooseHeader(uint64(len(content)))
	physical := append(header[:], bytes.Repeat([]byte("oversized stored payload"), 4)...)
	require.NoError(os.WriteFile(path, physical, 0o600))
	catalog := newMaintenanceCatalog()
	addMaintenanceCandidate(catalog, Candidate{
		Hash: hash, Paths: []string{path}, Size: int64(len(content)),
	})
	limits := DefaultLimits()
	limits.BlobBytes = int64(len(content) + 1)
	maintainer := newMaintainerForTest(t, catalog, layout, limits)
	originalReader := newLooseZstdReader
	decoderCalls := 0
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		decoderCalls++
		return originalReader(src)
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.BlobsCorrupt)
	assert.Zero(stats.BlobsPacked)
	assert.Zero(decoderCalls)
	assert.FileExists(path)
}

func TestPackOrphanSweepRecognizesOnlyCanonicalLooseNames(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	rawHash := writeMaintenanceLoose(t, layout, []byte("raw orphan"))
	compressedContent := []byte("compressed orphan")
	compressedHash := hashForTest(compressedContent)
	writeCompressedLooseFixture(t, layout, compressedHash, int64(len(compressedContent)), compressedContent, nil)
	unknown := layout.LoosePath(hashForTest([]byte("unknown extension"))) + ".bak"
	require.NoError(os.MkdirAll(filepath.Dir(unknown), 0o700))
	require.NoError(os.WriteFile(unknown, []byte("preserve"), 0o600))
	symlinkHash := hashForTest([]byte("symlink orphan"))
	symlinkPath := layout.CompressedLoosePath(symlinkHash)
	require.NoError(os.MkdirAll(filepath.Dir(symlinkPath), 0o700))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(os.WriteFile(target, []byte("target"), 0o600))
	require.NoError(os.Symlink(target, symlinkPath))
	maintainer := newMaintainerForTest(t, newMaintenanceCatalog(), layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(2, stats.LooseOrphansRemoved)
	assert.NoFileExists(layout.LoosePath(rawHash))
	assert.NoFileExists(layout.CompressedLoosePath(compressedHash))
	assert.FileExists(unknown)
	info, err := os.Lstat(symlinkPath)
	require.NoError(err)
	assert.NotZero(info.Mode() & os.ModeSymlink)
}

func TestPackDefersOversizedBlobWithoutFailingRun(t *testing.T) {
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := bytes.Repeat([]byte("x"), 9)
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	limits := DefaultLimits()
	limits.BlobBytes = 8
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.PacksSealed)
	assert.FileExists(layout.LoosePath(hash))
}

func TestPackIncompleteReferenceInventoryPreservesLooseOrphans(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	catalog.referencesComplete = false
	liveContent := []byte("valid referenced content still packs")
	liveHash := writeMaintenanceLoose(t, layout, liveContent)
	catalog.addLoose(liveHash, layout.LoosePath(liveHash))
	orphanContent := []byte("unknown reachability must preserve this loose object")
	orphanHash := writeMaintenanceLoose(t, layout, orphanContent)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	assert.True(stats.LooseOrphanSweepSuppressed)
	assert.FileExists(layout.LoosePath(orphanHash))
	assert.NoFileExists(layout.LoosePath(liveHash))
}

func TestPackIncompleteReferenceInventoryDefersOrphanPackReconciliation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	catalog.referencesComplete = false
	entry := buildStoreTestPack(t, layout, []byte("deferred orphan pack content"))
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Zero(stats.PacksAdopted)
	assert.Zero(stats.PacksRemoved)
	require.FileExists(layout.PackPath(entry.PackID))

	catalog.members[entry.Hash] = Reference{Hash: entry.Hash, OriginalHashes: []string{entry.Hash.String()}}
	catalog.referencesComplete = true
	stats, err = maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.PacksAdopted)
	indexed, records := catalog.snapshot()
	assert.Equal(entry, indexed[entry.Hash])
	assert.Contains(records, entry.PackID)
}

func TestPackRotatesBeforeExceedingMaintenanceOutputLimits(t *testing.T) {
	for _, tt := range []struct {
		name   string
		limits func() Limits
	}{
		{name: "container bytes", limits: func() Limits {
			limits := DefaultLimits()
			limits.PackBytes = int64(pack.MinEntryOffset + 8 + 65 + 40)
			return limits
		}},
		{name: "footer bytes", limits: func() Limits {
			limits := DefaultLimits()
			limits.FooterBytes = 65
			return limits
		}},
		{name: "entry count", limits: func() Limits {
			limits := DefaultLimits()
			limits.PackEntries = 1
			return limits
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			layout := layoutForStoreTest(t)
			catalog := newMaintenanceCatalog()
			for _, content := range [][]byte{[]byte("aaaaaaaa"), []byte("bbbbbbbb"), []byte("cccccccc")} {
				hash := writeMaintenanceLoose(t, layout, content)
				catalog.addLoose(hash, layout.LoosePath(hash))
			}
			limits := tt.limits()
			maintainer := newMaintainerForTest(t, catalog, layout, limits)

			stats, err := maintainer.Pack(t.Context(), PackOptions{})
			require.NoError(err)
			assert.Equal(3, stats.PacksSealed)
			_, records := catalog.snapshot()
			require.Len(records, 3)
			for packID := range records {
				reader, openErr := OpenMaintenancePack(layout.PackPath(packID), limits)
				require.NoError(openErr)
				require.NoError(reader.Close())
			}
		})
	}
}

func TestPackDefersBlobThatCannotFitAnEmptyOutputPack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("eight888")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	limits := DefaultLimits()
	limits.PackBytes = int64(pack.MinEntryOffset + len(content) + 65 + 40 - 1)
	maintainer := newMaintainerForTest(t, catalog, layout, limits)

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.BlobsDeferredOversized)
	assert.Zero(stats.PacksSealed)
	assert.FileExists(layout.LoosePath(hash))
}

func TestPackSoftBudgetStopsAfterCommittedBlob(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	for _, content := range [][]byte{[]byte("first"), []byte("second")} {
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{MaxBytes: 1})
	require.NoError(t, err)
	assert.True(t, stats.BudgetExhausted)
	assert.Equal(t, 1, stats.BlobsPacked)
}

func TestPackPreservesCatalogCandidateOrder(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	contents := [][]byte{[]byte("first by locality"), []byte("second by locality"), []byte("third by locality")}
	want := make([]Hash, 0, len(contents))
	for _, content := range contents {
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
		want = append(want, hash)
	}
	sort.Slice(want, func(i, j int) bool { return want[i] > want[j] })
	catalog.setCandidateOrder(want)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	require.Equal(1, stats.PacksSealed)
	_, packs := catalog.snapshot()
	require.Len(packs, 1)
	var packID string
	for id := range packs {
		packID = id
	}
	reader, err := pack.OpenReader(layout.PackPath(packID), nil)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reader.Close()) })
	entries := reader.Entries()
	require.Len(entries, len(want))
	got := make([]Hash, len(entries))
	for i, entry := range entries {
		got[i] = Hash(entry.ID.String())
	}
	require.Equal(want, got)
}

func TestPackCommitFailureLeavesRecoverableOrphanAndLooseSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("survive catalog commit failure")
	hash := writeMaintenanceLoose(t, layout, content)
	catalog.addLoose(hash, layout.LoosePath(hash))
	catalog.recordErr = errors.New("injected catalog failure")
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err := maintainer.Pack(t.Context(), PackOptions{})
	require.ErrorContains(err, "injected catalog failure")
	assert.FileExists(layout.LoosePath(hash))
	orphans, err := filepath.Glob(filepath.Join(layout.PacksDir(), "*", "*"+PackExt))
	require.NoError(err)
	require.Len(orphans, 1)

	catalog.recordErr = nil
	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(1, stats.PacksRemoved, "the durable loose authority makes the failed pack redundant")
	assert.Equal(1, stats.PacksSealed)
	assert.NoFileExists(layout.LoosePath(hash))
	got, _ := readStoreTest(t, maintainer.store, hash)
	assert.Equal(content, got)
}

func TestRepairDropsDanglingRecordsAndUnreferencedMappings(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	deadHash := hashForTest([]byte("dead"))
	catalog.entries[deadHash] = IndexEntry{Hash: deadHash, PackID: pack.NewPackID()}
	danglingID := pack.NewPackID()
	catalog.packs[danglingID] = PackRecord{PackID: danglingID, EntryCount: 1, CreatedAt: time.Now()}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, stats.RecordsDropped)
	assert.Equal(t, int64(1), stats.MappingsPruned)
}

func TestRepairRepacksValidRawCopyWhenIndexedPackAndPreferredCompressedAreCorrupt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("recover from corrupt indexed pack")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	writeCompressedLooseFixture(
		t,
		layout,
		entry.Hash,
		int64(len(content)),
		bytes.Repeat([]byte{'x'}, len(content)),
		nil,
	)
	catalog.addLoose(entry.Hash, layout.LoosePath(entry.Hash))
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	path := layout.PackPath(entry.PackID)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	var damaged [1]byte
	_, err = f.ReadAt(damaged[:], entry.Offset)
	require.NoError(err)
	damaged[0] ^= 0xff
	_, err = f.WriteAt(damaged[:], entry.Offset)
	require.NoError(err)
	require.NoError(f.Close())
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(int64(1), stats.MappingsPruned)
	assert.Equal(1, stats.PacksSealed)
	assert.Equal(1, stats.BlobsPacked)
	indexed, _ := catalog.snapshot()
	require.Contains(indexed, entry.Hash)
	assert.NotEqual(entry.PackID, indexed[entry.Hash].PackID)
	assert.NoFileExists(layout.LoosePath(entry.Hash))
	got, _ := readStoreTest(t, maintainer.store, entry.Hash)
	assert.Equal(content, got)
}

func TestRepairCommitFailureRetainsCorruptPackedMappingWhenOnlyRawAlternateIsValid(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("retain corrupt packed authority until raw recovery commits")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	corruptCompressed := []byte("corrupt preferred compressed recovery source")
	require.NoError(os.WriteFile(layout.CompressedLoosePath(entry.Hash), corruptCompressed, 0o600))
	catalog.addLoose(entry.Hash, layout.LoosePath(entry.Hash))
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	path := layout.PackPath(entry.PackID)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(err)
	var damaged [1]byte
	_, err = f.ReadAt(damaged[:], entry.Offset)
	require.NoError(err)
	damaged[0] ^= 0xff
	_, err = f.WriteAt(damaged[:], entry.Offset)
	require.NoError(err)
	require.NoError(f.Close())
	commitErr := errors.New("injected recovery commit failure")
	catalog.recordErr = commitErr
	catalog.adoptErr = commitErr
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	_, err = maintainer.Pack(t.Context(), PackOptions{})

	require.ErrorIs(err, commitErr)
	location, err := catalog.Resolve(t.Context(), entry.Hash)
	require.NoError(err)
	require.NotNil(location.Pack)
	assert.Equal(entry, *location.Pack, "failed recovery retains the previous catalog authority")
	assert.Equal(content, mustReadFile(t, layout.LoosePath(entry.Hash)))
	assert.Equal(corruptCompressed, mustReadFile(t, layout.CompressedLoosePath(entry.Hash)))
}

func TestRepairUsesVerifiedLooseSizeWhenPackedMetadataIsCorrupt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	content := []byte("derive recovery size from verified loose bytes")
	entry := buildStoreTestPack(t, layout, content)
	require.Equal(entry.Hash, writeMaintenanceLoose(t, layout, content))
	catalog.addLoose(entry.Hash, layout.LoosePath(entry.Hash))
	entry.RawLen++
	catalog.entries[entry.Hash] = entry
	catalog.packs[entry.PackID] = PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

	stats, err := maintainer.Pack(t.Context(), PackOptions{})

	require.NoError(err)
	assert.Equal(1, stats.BlobsPacked)
	location, err := catalog.Resolve(t.Context(), entry.Hash)
	require.NoError(err)
	require.NotNil(location.Pack)
	assert.NotEqual(entry.PackID, location.Pack.PackID)
	got, _ := readStoreTest(t, maintainer.store, entry.Hash)
	assert.Equal(content, got)
}

func TestReconcileAdoptsOnlyFullyVerifiedOrphanPack(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		name := "valid"
		if damaged {
			name = "damaged"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			layout := layoutForStoreTest(t)
			catalog := newMaintenanceCatalog()
			content := []byte("orphan recovery content")
			entry := buildStoreTestPack(t, layout, content)
			catalog.members[entry.Hash] = Reference{Hash: entry.Hash, OriginalHashes: []string{entry.Hash.String()}}
			if damaged {
				path := layout.PackPath(entry.PackID)
				f, err := os.OpenFile(path, os.O_RDWR, 0)
				require.NoError(err)
				_, err = f.WriteAt([]byte{0xff}, entry.Offset)
				require.NoError(err)
				require.NoError(f.Close())
			}
			maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())

			stats, err := maintainer.Pack(t.Context(), PackOptions{})
			require.NoError(err)
			entries, packs := catalog.snapshot()
			if damaged {
				assert.Equal(1, stats.PacksQuarantined)
				assert.Empty(entries)
				assert.Empty(packs)
				assert.FileExists(layout.PackPath(entry.PackID))
			} else {
				assert.Equal(1, stats.PacksAdopted)
				assert.Equal(entry, entries[entry.Hash])
				assert.Contains(packs, entry.PackID)
			}
		})
	}
}

func TestPackHonorsCancellationBeforeMutation(t *testing.T) {
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := maintainer.Pack(ctx, PackOptions{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestPackDurablyCreatesPacksDirectory(t *testing.T) {
	require := require.New(t)
	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	var synced []string
	originalSyncDir := pack.SyncDir
	pack.SyncDir = func(path string) error {
		synced = append(synced, path)
		return nil
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	_, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	require.Contains(synced, layout.Root())
}

func newMaintainerForTest(tb testing.TB, catalog Catalog, layout Layout, limits Limits) *Maintainer {
	tb.Helper()
	maintainer, err := NewMaintainer(catalog, layout, MaintainerOptions{Limits: limits})
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, maintainer.Close()) })
	return maintainer
}

func writeMaintenanceLoose(t *testing.T, layout Layout, content []byte) Hash {
	t.Helper()
	hash := hashForTest(content)
	path := layout.LoosePath(hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return hash
}

func addMaintenanceCandidate(catalog *maintenanceCatalog, candidate Candidate) {
	catalog.members[candidate.Hash] = Reference{
		Hash: candidate.Hash, OriginalHashes: []string{candidate.Hash.String()},
	}
	candidate.OriginalHashes = []string{candidate.Hash.String()}
	catalog.candidates[candidate.Hash] = candidate
}

type listedCandidateCatalog struct {
	*maintenanceCatalog
	listed   []Candidate
	recorded []Adoption
}

type countingResolveCatalog struct {
	*maintenanceCatalog
	resolveCalls int
}

type observedIdentityPin struct {
	identityPin
	closed   *int
	closeErr error
}

func (p *observedIdentityPin) Close() error {
	if p.closed != nil {
		*p.closed++
	}
	return errors.Join(p.identityPin.Close(), p.closeErr)
}

func (c *countingResolveCatalog) Resolve(ctx context.Context, hash Hash) (Location, error) {
	c.resolveCalls++
	return c.maintenanceCatalog.Resolve(ctx, hash)
}

type cancelAfterFirstLooseRead struct {
	looseZstdReader
	cancel    context.CancelFunc
	cancelled bool
}

func (r *cancelAfterFirstLooseRead) Read(p []byte) (int, error) {
	n, err := r.looseZstdReader.Read(p)
	if n > 0 && !r.cancelled {
		r.cancelled = true
		r.cancel()
	}
	return n, err
}

func (c *listedCandidateCatalog) ListUnpacked(context.Context) ([]Candidate, error) {
	return append([]Candidate(nil), c.listed...), nil
}

func (c *listedCandidateCatalog) RecordPack(ctx context.Context, record PackRecord, adoptions []Adoption) error {
	c.recorded = make([]Adoption, len(adoptions))
	for index, adoption := range adoptions {
		c.recorded[index] = adoption
		c.recorded[index].OriginalHashes = append([]string(nil), adoption.OriginalHashes...)
	}
	return c.maintenanceCatalog.RecordPack(ctx, record, adoptions)
}
