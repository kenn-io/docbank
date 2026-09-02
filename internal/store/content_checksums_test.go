package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuxiliaryChecksumPersistsByAuthoritativeSHA256Identity(t *testing.T) {
	s := newTestStore(t)
	hash := fakeHash("a9")
	const md5sum = "f6fdffe48c908deb0f4c3bd36c032e72"
	physical := BlobPhysical{
		Encoding: "raw", StoredBytes: 9, PackEligible: true, Created: true, MD5: md5sum,
	}
	node, err := s.CreateFile(t.Context(), s.RootID(), "source.bin", hash, 9, "", physical)
	require.NoError(t, err)

	record, err := s.BlobChecksums(t.Context(), hash)
	require.NoError(t, err)
	assert.Equal(t, BlobChecksumRecord{BlobSHA256: hash, MD5: md5sum}, record)
	assert.Equal(t, hash, node.BlobHash, "MD5 must not replace SHA-256 identity")

	_, err = s.CreateFile(t.Context(), s.RootID(), "deduplicated.bin", hash, 9, "",
		BlobPhysical{Encoding: "raw", StoredBytes: 9, PackEligible: true, MD5: md5sum})
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "disagreement.bin", hash, 9, "",
		BlobPhysical{Encoding: "raw", StoredBytes: 9, PackEligible: true,
			MD5: "00000000000000000000000000000000"})
	require.ErrorContains(t, err, "different MD5")
}

func TestAuxiliaryChecksumValidationAndMissingBackfillTargets(t *testing.T) {
	s := newTestStore(t)
	hash := fakeHash("b8")
	_, err := s.CreateFile(t.Context(), s.RootID(), "legacy.bin", hash, 4, "")
	require.NoError(t, err)
	_, err = s.BlobChecksums(t.Context(), hash)
	require.ErrorIs(t, err, ErrNotFound)

	targets, err := s.MissingBlobChecksumTargetsAfter(t.Context(), "", 10)
	require.NoError(t, err)
	require.Equal(t, []BlobChecksumTarget{{BlobSHA256: hash, Size: 4}}, targets)

	for _, invalid := range []string{"", "ABCDEF0123456789ABCDEF0123456789", strings.Repeat("g", 32), "abcd"} {
		err = s.RecordVerifiedBlobChecksum(context.Background(), BlobChecksumRecord{
			BlobSHA256: hash, MD5: invalid,
		})
		require.ErrorContains(t, err, "canonical lowercase MD5")
	}
	require.NoError(t, s.RecordVerifiedBlobChecksum(t.Context(), BlobChecksumRecord{
		BlobSHA256: hash, MD5: "8d777f385d3dfec8815d20f7496026dc",
	}))
	targets, err = s.MissingBlobChecksumTargetsAfter(t.Context(), "", 10)
	require.NoError(t, err)
	assert.Empty(t, targets)
}

func TestMissingBlobChecksumTargetsCanAdvancePastAFailingHash(t *testing.T) {
	s := newTestStore(t)
	for index, hash := range []string{fakeHash("a1"), fakeHash("b2"), fakeHash("c3")} {
		_, err := s.CreateFile(t.Context(), s.RootID(), "source-"+string(rune('a'+index))+".bin", hash, 4, "")
		require.NoError(t, err)
	}

	first, err := s.MissingBlobChecksumTargetsAfter(t.Context(), "", 1)
	require.NoError(t, err)
	require.Equal(t, []BlobChecksumTarget{{BlobSHA256: fakeHash("a1"), Size: 4}}, first)
	second, err := s.MissingBlobChecksumTargetsAfter(t.Context(), first[0].BlobSHA256, 1)
	require.NoError(t, err)
	assert.Equal(t, []BlobChecksumTarget{{BlobSHA256: fakeHash("b2"), Size: 4}}, second)
}
