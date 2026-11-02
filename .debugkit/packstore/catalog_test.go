package packstore_test

import (
	"crypto/sha256"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestValidateIndexEntriesRejectsDuplicateHashes(t *testing.T) {
	hash := packstore.Hash(pack.ComputeBlobID([]byte("duplicate")).String())
	packID := pack.NewPackID()
	entry := packstore.IndexEntry{
		Hash:      hash,
		PackID:    packID,
		Offset:    6,
		StoredLen: 9,
		RawLen:    9,
	}

	require.NoError(t, packstore.ValidateIndexEntries([]packstore.IndexEntry{entry}))
	err := packstore.ValidateIndexEntries([]packstore.IndexEntry{entry, entry})
	require.ErrorIs(t, err, packstore.ErrDuplicateHash)
}

func TestCatalogMetadataValidation(t *testing.T) {
	require := require.New(t)
	hash := packstore.Hash(pack.ComputeBlobID([]byte("entry")).String())
	packID := pack.NewPackID()
	now := time.Now().UTC()

	validEntry := packstore.IndexEntry{
		Hash:      hash,
		PackID:    packID,
		Offset:    6,
		StoredLen: 5,
		RawLen:    5,
	}
	require.NoError(validEntry.Validate())

	badHash := validEntry
	badHash.Hash = packstore.Hash("ABC")
	require.ErrorIs(badHash.Validate(), packstore.ErrInvalidHash)

	badPack := validEntry
	badPack.PackID = "../pack"
	require.Error(badPack.Validate())

	badLength := validEntry
	badLength.RawLen = -1
	require.Error(badLength.Validate())

	insideHeader := validEntry
	insideHeader.Offset = pack.MinEntryOffset - 1
	require.Error(insideHeader.Validate())

	hugeStored := validEntry
	hugeStored.StoredLen = int64(pack.MaxStoredLen) + 1
	require.Error(hugeStored.Validate())

	overflow := validEntry
	overflow.Offset = math.MaxInt64
	require.Error(overflow.Validate())

	record := packstore.PackRecord{
		PackID:      packID,
		EntryCount:  1,
		StoredBytes: 5,
		CreatedAt:   now,
	}
	require.NoError(record.Validate())
	record.EntryCount = -1
	require.Error(record.Validate())
}

func TestHashMatchesContent(t *testing.T) {
	content := []byte("hash identity")
	sum := sha256.Sum256(content)
	hash, err := packstore.ParseHash(pack.ComputeBlobID(content).String())
	require.NoError(t, err)
	assert.Equal(t, sum[:], hash.Bytes())
}
