package docbank

import (
	"bytes"
	"encoding/json"
	"image/color"
	"image/jpeg"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	internalblob "go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func TestVisualPreviewSupportsMediaType(t *testing.T) {
	require.True(t, VisualPreviewSupportsMediaType("IMAGE/JPEG; q=90"))
	require.True(t, VisualPreviewSupportsMediaType("image/x-adobe-dng"))
	require.False(t, VisualPreviewSupportsMediaType("video/mp4"))
}

func TestVaultVisualPreviewSizes(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	source := mediatest.JPEG(4200, 8, color.White)
	created, err := vault.Create(t.Context(), "/photo.jpg", bytes.NewReader(source), CreateOptions{MediaType: "image/jpeg", Expected: contentIdentity(source)})
	require.NoError(t, err)
	versionID := created.Version.ID
	_, err = vault.VisualPreviewForSize(t.Context(), versionID, VisualPreviewGrid)
	require.ErrorIs(t, err, ErrNotFound)
	for size, edge := range map[VisualPreviewSize]int{VisualPreviewGrid: 512, VisualPreviewFit: 2560} {
		first, err := vault.EnsureVisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		require.Equal(t, edge, first.Output.Width)
		_, err = vault.VisualPreview(t.Context(), versionID)
		require.ErrorIs(t, err, ErrNotFound)
	}
	large, err := vault.EnsureVisualPreview(t.Context(), versionID)
	require.NoError(t, err)
	require.Equal(t, 4096, large.Output.Width)
	for size, edge := range map[VisualPreviewSize]int{VisualPreviewGrid: 512, VisualPreviewFit: 2560, VisualPreviewLarge: 4096} {
		exact, err := vault.VisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		retry, err := vault.EnsureVisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		require.Equal(t, exact, retry)
		opened, err := vault.OpenVisualPreviewForSize(t.Context(), versionID, size)
		require.NoError(t, err)
		data, err := io.ReadAll(opened.Reader)
		require.NoError(t, err)
		require.NoError(t, opened.Reader.Close())
		image, err := jpeg.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, edge, image.Bounds().Dx())
		legacy, err := vault.VisualPreview(t.Context(), versionID)
		require.NoError(t, err)
		require.Equal(t, large.GenerationID, legacy.GenerationID)
	}
	_, err = vault.EnsureVisualPreviewForSize(t.Context(), versionID, "invalid")
	require.Error(t, err)
}

func TestVaultEnsureVisualPreviewReusesOutputAfterPrimaryPlacement(t *testing.T) {
	root := t.TempDir()
	secondaryRoot := t.TempDir()
	require.NoError(t, internalblob.EnsureFilesystemNamespace(secondaryRoot))
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	secondary, err := metadata.PrepareSecondaryBlobStore("archive", "filesystem", "archive")
	require.NoError(t, err)
	backend, err := internalblob.NewFilesystemBackend(secondaryRoot, nil)
	require.NoError(t, err)
	require.NoError(t, backend.ReplaceOwnership(t.Context(), packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: metadata.VaultID(),
		Store: packstore.StoreID(secondary.ID), Epoch: secondary.OwnershipEpoch,
	}, nil))
	require.NoError(t, backend.Close())
	require.NoError(t, metadata.RegisterBlobStore(t.Context(), secondary))
	require.NoError(t, metadata.Close())
	vault, err := New(t.Context(), Config{
		Root: root,
		StoreBindings: map[string]StoreBinding{
			"archive": {Kind: "filesystem", Path: secondaryRoot},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	source := mediatest.JPEG(4200, 8, color.White)
	created, err := vault.Create(t.Context(), "/photo.jpg", bytes.NewReader(source), CreateOptions{
		MediaType: "image/jpeg", Expected: contentIdentity(source),
	})
	require.NoError(t, err)
	preview, err := vault.EnsureVisualPreview(t.Context(), created.Version.ID)
	require.NoError(t, err)
	opened, err := vault.OpenVisualPreview(t.Context(), created.Version.ID)
	require.NoError(t, err)
	previewBytes, err := io.ReadAll(opened.Reader)
	require.NoError(t, err)
	require.NoError(t, opened.Reader.Verify())
	require.NoError(t, opened.Reader.Close())
	_, err = vault.Create(t.Context(), "/preview-copy.jpg", bytes.NewReader(previewBytes), CreateOptions{
		MediaType: "image/jpeg", Expected: contentIdentity(previewBytes),
	})
	require.NoError(t, err)

	plan, err := vault.metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: vault.metadata.RootID(), SourceStoreID: vault.metadata.PrimaryBlobStoreID(),
		DestinationStoreID: secondary.ID, RetireSource: true,
	})
	require.NoError(t, err)
	var outputPlacement *store.PlacementHash
	for index := range plan.Hashes {
		if plan.Hashes[index].Hash == preview.Output.BlobSHA256 {
			outputPlacement = &plan.Hashes[index]
			break
		}
	}
	require.NotNil(t, outputPlacement)
	require.True(t, outputPlacement.RetireSource)
	requestJSON, err := json.Marshal(plan.Request)
	require.NoError(t, err)
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	operation, err := vault.metadata.CreateStorageOperation(t.Context(), store.StorageOperationCreate{
		Kind: "place", StoreReferences: []store.StorageOperationStoreReference{
			{StoreID: plan.Request.SourceStoreID, Role: "source"},
			{StoreID: plan.Request.DestinationStoreID, Role: "destination"},
		},
		RequestDigest: plan.Digest, RequestJSON: string(requestJSON),
		PlanJSON: string(planJSON), TotalObjects: int64(len(plan.Hashes)),
	})
	require.NoError(t, err)
	require.NoError(t, (internalblob.PlacementRunner{
		Metadata: vault.metadata, Blobs: vault.blobs,
	}).Run(t.Context(), operation.ID))
	_, err = vault.metadata.PhysicalContent(t.Context(), preview.Output.BlobSHA256)
	require.ErrorIs(t, err, store.ErrPhysicalAuthorityMissing)

	retry, err := vault.EnsureVisualPreview(t.Context(), created.Version.ID)
	require.NoError(t, err)
	require.Equal(t, preview.GenerationID, retry.GenerationID)
	explicit, err := vault.EnsureVisualPreviewForSize(t.Context(), created.Version.ID, VisualPreviewLarge)
	require.NoError(t, err)
	require.Equal(t, preview.GenerationID, explicit.GenerationID)
}
