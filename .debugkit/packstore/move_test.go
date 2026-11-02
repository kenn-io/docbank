package packstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveCrashBeforeVerificationLeavesNoAuthorityReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	content := []byte("copy requiring destination verification")
	hash := hashForTest(content)
	source := attachedFilesystemBackend(t, "source", "source-epoch")
	destination := attachedFilesystemBackend(t, "destination", "destination-epoch")
	sourceReceipt, err := source.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(err)

	receipt, err := Move(
		ctx,
		source,
		&corruptReadbackBackend{Backend: destination},
		MoveRequest{
			Source: ReadLocation{
				StoreID: sourceReceipt.StoreID, Generation: "source-generation",
				Loose: &sourceReceipt.Location,
			},
			Destination: destination.StoreID(),
			Identity:    BlobIdentity{Hash: hash, Size: int64(len(content))},
		},
	)

	assert.Equal(MoveReceipt{}, receipt)
	require.ErrorIs(err, ErrPhysicalCorrupt)
	stream, _, err := destination.OpenLoose(ctx, hash, sourceReceipt.Location)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal(content, got)
}

func TestMoveCrashAfterVerificationReturnsRecoverableOrphan(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	content := []byte("verified physical orphan")
	hash := hashForTest(content)
	source := attachedFilesystemBackend(t, "source", "source-epoch")
	destination := attachedFilesystemBackend(t, "destination", "destination-epoch")
	sourceReceipt, err := source.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(err)

	receipt, err := Move(
		ctx,
		source,
		destination,
		MoveRequest{
			Source: ReadLocation{
				StoreID: sourceReceipt.StoreID, Generation: "source-generation",
				Loose: &sourceReceipt.Location,
			},
			Destination: destination.StoreID(),
			Identity:    BlobIdentity{Hash: hash, Size: int64(len(content))},
		},
	)

	require.NoError(err)
	assert.True(receipt.Verified)
	assert.Equal(destination.StoreID(), receipt.Destination.StoreID)
	stream, _, err := destination.OpenLoose(ctx, hash, *receipt.Destination.Loose)
	require.NoError(err)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal(content, got)
}

func TestMoveVerifiesDestinationReadBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	content := []byte("destination read-back")
	hash := hashForTest(content)
	source := attachedFilesystemBackend(t, "source", "source-epoch")
	destination := attachedFilesystemBackend(t, "destination", "destination-epoch")
	sourceReceipt, err := source.PublishLoose(
		ctx,
		hash,
		bytes.NewReader(content),
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)
	require.NoError(err)
	counting := &countingReadbackBackend{Backend: destination}

	receipt, err := Move(
		ctx,
		source,
		counting,
		MoveRequest{
			Source: ReadLocation{
				StoreID: sourceReceipt.StoreID, Generation: "source-generation",
				Loose: &sourceReceipt.Location,
			},
			Destination: destination.StoreID(),
			Identity:    BlobIdentity{Hash: hash, Size: int64(len(content))},
		},
	)

	require.NoError(err)
	assert.True(receipt.Verified)
	assert.Equal(1, counting.readbacks)
}

func TestMoveRetirementKeepsActiveReaderUsable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	content := bytes.Repeat([]byte("active packed reader "), 4096)
	backend := attachedFilesystemBackend(t, "source", "source-epoch")
	entry := buildStoreTestPack(t, backend.Layout(), content)
	stream, _, err := backend.OpenPack(ctx, entry.Hash, entry)
	require.NoError(err)

	retireErr := backend.Retire(ctx, ObjectRef{PackID: entry.PackID})
	require.NoError(retireErr)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal(content, got)
}

func attachedFilesystemBackend(
	t *testing.T,
	store StoreID,
	epoch string,
) *FilesystemBackend {
	t.Helper()
	layout := layoutForStoreTest(t)
	backend, err := NewFilesystemBackend(layout, FilesystemBackendOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	ownership := Ownership{
		Format: OwnershipFormatV1,
		Vault:  "test-vault",
		Store:  store,
		Epoch:  epoch,
	}
	require.NoError(t, backend.ReplaceOwnership(t.Context(), ownership, nil))
	return backend
}

type corruptReadbackBackend struct {
	Backend
}

func (b *corruptReadbackBackend) OpenLoose(
	context.Context,
	Hash,
	LooseLocation,
) (VerifiedReadCloser, int64, error) {
	return nil, 0, errors.Join(
		ErrPhysicalCorrupt,
		errors.New("synthetic post-publication read-back failure"),
	)
}

type countingReadbackBackend struct {
	Backend
	readbacks int
}

func (b *countingReadbackBackend) OpenLoose(
	ctx context.Context,
	hash Hash,
	location LooseLocation,
) (VerifiedReadCloser, int64, error) {
	b.readbacks++
	return b.Backend.OpenLoose(ctx, hash, location)
}
