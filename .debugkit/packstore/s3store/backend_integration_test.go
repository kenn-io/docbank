package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/s3test"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestS3BackendConformance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := s3test.Load(t)
	ctx := t.Context()
	config := Config{Endpoint: fixture.Endpoint, Region: "us-east-1", Bucket: fixture.Bucket, Prefix: fixture.Prefix, ForcePathStyle: true, AllowInsecureTransport: true, Credentials: fixture.Credentials, InventoryPageSize: 1}

	backend, err := New(ctx, config)
	require.NoError(err)
	s3test.Prepare(t, backend.client, fixture)
	empty, err := backend.NamespaceEmpty(ctx)
	require.NoError(err)
	assert.True(empty)

	owner1 := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "vault-a", Store: "cold", Epoch: "epoch-1",
	}
	require.NoError(backend.ReplaceOwnership(ctx, owner1, nil))
	empty, err = backend.NamespaceEmpty(ctx)
	require.NoError(err)
	assert.False(empty)
	assert.Equal(owner1.Store, backend.StoreID())
	actual, err := backend.Ownership(ctx)
	require.NoError(err)
	assert.Equal(owner1, actual)

	report, err := backend.Probe(ctx)
	require.NoError(err)
	assert.Equal(CapabilityReport{
		StrongReadAfterWrite: true, RepeatableReads: true, RangeReads: true,
		MultipartPublication: true, ConditionalWrites: true, Listing: true, Delete: true,
	}, report)

	content := bytes.Repeat([]byte("remote document\n"), 1024)
	hash := hashOf(content)
	loose, err := backend.PublishLoose(ctx, hash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		})
	require.NoError(err)
	assert.True(loose.Created)
	stream, size, err := backend.OpenLoose(ctx, hash, loose.Location)
	require.NoError(err)
	assert.Equal(int64(len(content)), size)
	got, err := io.ReadAll(stream)
	require.NoError(err)
	require.NoError(stream.Close())
	assert.Equal(content, got)
	assert.True(stream.Verified())
	duplicate, err := backend.PublishLoose(ctx, hash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		})
	require.NoError(err)
	assert.False(duplicate.Created)
	_, err = backend.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(backend.bucket),
		Key: aws.String(backend.keys.loose(
			hash, packstore.LooseEncodingRaw,
		)),
		Body: bytes.NewReader([]byte("corrupt")),
	})
	require.NoError(err)
	repaired, err := backend.RepairLoose(
		ctx, hash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		},
	)
	require.NoError(err)
	assert.False(repaired.Created)
	stream, _, err = backend.OpenLoose(ctx, hash, repaired.Location)
	require.NoError(err)
	require.NoError(stream.Verify())
	require.NoError(stream.Close())

	wrongHash := hashOf([]byte("different bytes"))
	_, err = backend.PublishLoose(ctx, wrongHash, bytes.NewReader(content),
		packstore.PublishOptions{
			ExpectedSize: int64(len(content)), SizeKnown: true,
			MaxBytes: int64(len(content)),
		})
	require.ErrorIs(err, packstore.ErrContentMismatch)
	_, err = backend.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(backend.bucket),
		Key:    aws.String(backend.keys.loose(wrongHash, packstore.LooseEncodingRaw)),
	})
	assert.Equal(404, statusCode(err), "invalid source must remain an unpublished multipart orphan")

	packID, packBytes, indexed := makePack(t, content)
	packReceipt, err := backend.PublishPack(ctx, packID, bytes.NewReader(packBytes),
		packstore.PublishOptions{
			ExpectedSize: int64(len(packBytes)), SizeKnown: true,
			MaxBytes: int64(len(packBytes)),
		})
	require.NoError(err)
	assert.True(packReceipt.Created)
	packed, packedSize, err := backend.OpenPack(ctx, hash, indexed)
	require.NoError(err)
	assert.Equal(int64(len(content)), packedSize)
	got, err = io.ReadAll(packed)
	require.NoError(err)
	require.NoError(packed.Close())
	assert.Equal(content, got)
	assert.True(packed.Verified())

	var foreign []string
	for _, name := range []string{"operator-note.txt", "foreign/\x01+entry", "foreign/literal%2F+name/nested", "loose%2F" + hash.String()[:2] + "%2F" + hash.String()} {
		key := backend.keys.join(name)
		_, err = backend.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(backend.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader([]byte("preserve me")),
		})
		require.NoError(err)
		foreign = append(foreign, key)
	}
	objects, unknown := collectInventory(t, ctx, backend)
	assert.Len(objects, 2)
	assert.ElementsMatch(foreign, unknown)

	oldBackend, err := New(ctx, Config{
		Endpoint: config.Endpoint, Region: config.Region, Bucket: config.Bucket,
		Prefix: config.Prefix, ForcePathStyle: true, Credentials: config.Credentials,
		AllowInsecureTransport: config.AllowInsecureTransport,
		ExpectedOwnership:      &owner1,
	})
	require.NoError(err)
	owner2 := owner1
	owner2.Epoch = "epoch-2"
	require.NoError(backend.ReplaceOwnership(ctx, owner2, &owner1))
	err = oldBackend.Retire(ctx, packstore.ObjectRef{
		LooseHash: hash, LooseEncoding: packstore.LooseEncodingRaw,
	})
	require.ErrorIs(err, packstore.ErrStoreFenced)

	require.NoError(backend.Retire(ctx, packstore.ObjectRef{PackID: packID}))
	_, _, err = backend.OpenPack(ctx, hash, indexed)
	assert.ErrorIs(err, packstore.ErrPhysicalMissing)
}

func collectInventory(
	t *testing.T,
	ctx context.Context,
	backend *Backend,
) ([]packstore.InventoryObject, []string) {
	t.Helper()
	var objects []packstore.InventoryObject
	var unknown []string
	var cursor packstore.InventoryCursor
	for {
		page, err := backend.Inventory(ctx, cursor)
		require.NoError(t, err)
		objects = append(objects, page.Objects...)
		unknown = append(unknown, page.Unknown...)
		if page.NextCursor == "" {
			return objects, unknown
		}
		cursor = page.NextCursor
	}
}

func makePack(
	t *testing.T,
	content []byte,
) (string, []byte, packstore.IndexEntry) {
	t.Helper()
	packID, data, entries := makePackEntries(t, content)
	return packID, data, entries[0]
}

func makePackEntries(
	t *testing.T,
	contents ...[]byte,
) (string, []byte, []packstore.IndexEntry) {
	t.Helper()
	require := require.New(t)
	t.Helper()
	dir := t.TempDir()
	writer, err := pack.NewWriter(dir, pack.WriterOptions{})
	require.NoError(err)
	entries := make([]pack.Entry, 0, len(contents))
	for _, content := range contents {
		entry, appendErr := writer.Append(content)
		require.NoError(appendErr)
		entries = append(entries, entry)
	}
	packID := pack.NewPackID()
	path := filepath.Join(dir, packID+".pack")
	_, err = writer.Seal(path)
	require.NoError(err)
	data, err := os.ReadFile(path)
	require.NoError(err)
	indexed := make([]packstore.IndexEntry, 0, len(entries))
	for i, entry := range entries {
		indexed = append(indexed, packstore.IndexEntry{
			Hash: hashOf(contents[i]), PackID: packID,
			Offset: int64(entry.Offset), StoredLen: int64(entry.StoredLen),
			RawLen: int64(entry.RawLen), Flags: uint8(entry.Flags), CRC32C: entry.CRC32C,
		})
	}
	return packID, data, indexed
}

func hashOf(content []byte) packstore.Hash {
	sum := sha256.Sum256(content)
	return packstore.Hash(hex.EncodeToString(sum[:]))
}
