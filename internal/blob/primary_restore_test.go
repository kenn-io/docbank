package blob

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestPrimaryRestoreHandoffRecoversBothPublicationSides(t *testing.T) {
	blobsDir := filepath.Join(t.TempDir(), "blobs")
	prior := testOwnership("10000000-0000-4000-8000-000000000001")
	next := testOwnership("20000000-0000-4000-8000-000000000002")
	priorDatabaseDigest := testDatabaseDigest('1')
	backend, _, err := openPrimaryOwnershipBackend(t.Context(), blobsDir)
	require.NoError(t, err)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), prior, nil))
	require.NoError(t, backend.Close())

	handoff, err := NewPrimaryRestoreHandoff(blobsDir, next, &priorDatabaseDigest)
	require.NoError(t, err)
	require.NoError(t, handoff.Prepare(t.Context()))
	assertPrimaryOwnership(t, blobsDir, next)
	require.NoError(t, RecoverPrimaryRestoreHandoff(
		t.Context(), blobsDir, &prior, &priorDatabaseDigest, nil,
	))
	assertPrimaryOwnership(t, blobsDir, prior)
	pending, err := PrimaryRestoreHandoffPending(blobsDir)
	require.NoError(t, err)
	assert.False(t, pending)

	handoff, err = NewPrimaryRestoreHandoff(blobsDir, next, &priorDatabaseDigest)
	require.NoError(t, err)
	require.NoError(t, handoff.Prepare(t.Context()))
	nextDatabaseDigest := testDatabaseDigest('2')
	require.NoError(t, RecoverPrimaryRestoreHandoff(
		t.Context(), blobsDir, &next, &nextDatabaseDigest, nil,
	))
	assertPrimaryOwnership(t, blobsDir, next)
	pending, err = PrimaryRestoreHandoffPending(blobsDir)
	require.NoError(t, err)
	assert.False(t, pending)
}

func TestPrimaryRestoreHandoffRejectsEqualOwnership(t *testing.T) {
	blobsDir := filepath.Join(t.TempDir(), "blobs")
	ownership := testOwnership("10000000-0000-4000-8000-000000000001")
	priorDigest := testDatabaseDigest('1')
	backend, _, err := openPrimaryOwnershipBackend(t.Context(), blobsDir)
	require.NoError(t, err)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), ownership, nil))
	require.NoError(t, backend.Close())
	handoff, err := NewPrimaryRestoreHandoff(blobsDir, ownership, &priorDigest)
	require.NoError(t, err)
	require.EqualError(t, handoff.Prepare(t.Context()), "primary restore handoff does not change ownership")
	assertPrimaryOwnership(t, blobsDir, ownership)
	pending, err := PrimaryRestoreHandoffPending(blobsDir)
	require.NoError(t, err)
	require.False(t, pending)
}

func TestPrimaryRestoreHandoffRecoversUnpublishedNewVault(t *testing.T) {
	blobsDir := filepath.Join(t.TempDir(), "blobs")
	next := testOwnership("20000000-0000-4000-8000-000000000002")
	priorDatabaseDigest := ""
	handoff, err := NewPrimaryRestoreHandoff(blobsDir, next, &priorDatabaseDigest)
	require.NoError(t, err)
	require.NoError(t, handoff.Prepare(t.Context()))

	require.NoError(t, RecoverPrimaryRestoreHandoff(
		t.Context(), blobsDir, nil, &priorDatabaseDigest, nil,
	))
	layout, err := newLayout(blobsDir)
	require.NoError(t, err)
	_, err = os.Stat(layout.OwnershipPath())
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestPrimaryRestoreHandoffRetainsMarkerUntilCompletion(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(strconv.FormatBool(recover), func(t *testing.T) {
			blobsDir := filepath.Join(t.TempDir(), "blobs")
			next := testOwnership("20000000-0000-4000-8000-000000000002")
			priorDigest := ""
			handoff, err := NewPrimaryRestoreHandoff(blobsDir, next, &priorDigest)
			require.NoError(t, err)
			require.NoError(t, handoff.Prepare(t.Context()))
			failure := errors.New("controls reset failed")
			complete := func(replaced bool) error {
				require.True(t, replaced)
				pending, err := PrimaryRestoreHandoffPending(blobsDir)
				require.NoError(t, err)
				require.True(t, pending)
				return failure
			}
			finish := func() error {
				if recover {
					digest := testDatabaseDigest('2')
					return RecoverPrimaryRestoreHandoff(t.Context(), blobsDir, &next, &digest, complete)
				}
				return handoff.Commit(t.Context(), complete)
			}
			require.ErrorIs(t, finish(), failure)
			pending, err := PrimaryRestoreHandoffPending(blobsDir)
			require.NoError(t, err)
			require.True(t, pending)
			failure = nil
			require.NoError(t, finish())
			pending, err = PrimaryRestoreHandoffPending(blobsDir)
			require.NoError(t, err)
			require.False(t, pending)
		})
	}
}

func testDatabaseDigest(value byte) string {
	return strings.Repeat(string(value), 64)
}

func testOwnership(storeID string) packstore.Ownership {
	return packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "10000000-0000-4000-8000-000000000000",
		Store:  packstore.StoreID(storeID),
		Epoch:  storeID,
	}
}

func assertPrimaryOwnership(
	t *testing.T, blobsDir string, want packstore.Ownership,
) {
	t.Helper()
	backend, _, err := openPrimaryOwnershipBackend(t.Context(), blobsDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, backend.Close()) }()
	got, err := backend.Ownership(t.Context())
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
