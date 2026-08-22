package blob

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	docconfig "go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestS3PlacementLifecycle(t *testing.T) {
	endpoint := os.Getenv("DOCBANK_S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("DOCBANK_S3_TEST_ENDPOINT is not configured")
	}
	accessKey := envOr("DOCBANK_S3_TEST_ACCESS_KEY", "docbank-test")
	secretKey := envOr("DOCBANK_S3_TEST_SECRET_KEY", "docbank-test-secret")
	region := envOr("DOCBANK_S3_TEST_REGION", "us-east-1")
	bucket := fmt.Sprintf("docbank-%d", time.Now().UnixNano())
	prefix := "vertical"
	client := newS3TestClient(endpoint, region, accessKey, secretKey)
	_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	require.NoError(t, err)
	t.Cleanup(func() { cleanupS3TestBucket(t, client, bucket) })

	profile := writeS3TestProfile(t, accessKey, secretKey)
	binding := docconfig.StoreBindingConfig{
		Kind: "s3", Endpoint: endpoint, Region: region,
		Bucket: bucket, Prefix: prefix, CredentialProfile: profile,
		Priority: 20, ForcePathStyle: true,
	}
	root := t.TempDir()
	metadata, err := store.Open(filepath.Join(root, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, metadata.Close()) })
	secondary, err := metadata.PrepareSecondaryBlobStore(
		"archive", "s3", "archive_s3",
	)
	require.NoError(t, err)
	openBackend := func(
		ctx context.Context, binding docconfig.StoreBindingConfig,
		expected *packstore.Ownership,
	) (packstore.Backend, error) {
		return newConfiguredBackend(ctx, binding, expected, true)
	}
	unattached, err := openBackend(t.Context(), binding, nil)
	require.NoError(t, err)
	inspector, ok := unattached.(packstore.NamespaceInspector)
	require.True(t, ok)
	empty, err := inspector.NamespaceEmpty(t.Context())
	require.NoError(t, err)
	assert.True(t, empty)
	ownership := packstore.Ownership{
		Format: packstore.OwnershipFormatV1, Vault: metadata.VaultID(),
		Store: packstore.StoreID(secondary.ID), Epoch: secondary.OwnershipEpoch,
	}
	require.NoError(t, unattached.ReplaceOwnership(t.Context(), ownership, nil))
	require.NoError(t, ProbeConfiguredBackend(t.Context(), unattached))
	require.NoError(t, closeBackend(unattached))
	require.NoError(t, metadata.RegisterBlobStore(t.Context(), secondary))

	registry := newRegistry(
		t.Context(), metadata.VaultID(),
		map[string]docconfig.StoreBindingConfig{"archive_s3": binding},
		[]StoreSpec{{
			ID: secondary.ID, Kind: secondary.Kind, Role: secondary.Role,
			Lifecycle: secondary.Lifecycle, Binding: secondary.Binding,
			OwnershipEpoch: secondary.OwnershipEpoch,
		}}, openBackend,
	)
	options := Options{Registry: registry}
	blobs, err := NewWithOptions(
		store.NewPackCatalog(metadata), filepath.Join(root, "blobs"), options,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	runner := PlacementRunner{Metadata: metadata, Blobs: blobs}

	remoteOnly := createS3PlacementFile(
		t, metadata, blobs, "remote.txt", []byte("remote-only content"),
	)
	remoteHash := placeS3TestFile(
		t, metadata, runner, remoteOnly, secondary.ID, true,
	)
	assertBlobContent(t, blobs, remoteHash, []byte("remote-only content"))

	packed := createS3PlacementFile(
		t, metadata, blobs, "packed.txt", []byte("packed source content"),
	)
	stats, err := blobs.Maintainer().Pack(t.Context(), packstore.PackOptions{})
	require.NoError(t, err)
	require.Positive(t, stats.BlobsPacked)
	packedHash := placeS3TestFile(
		t, metadata, runner, packed, secondary.ID, true,
	)
	assertBlobContent(t, blobs, packedHash, []byte("packed source content"))

	redundant := createS3PlacementFile(
		t, metadata, blobs, "repair.txt", []byte("repairable content"),
	)
	repairHash := placeS3TestFile(
		t, metadata, runner, redundant, secondary.ID, false,
	)
	_, err = client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key: aws.String(fmt.Sprintf(
			"%s/loose/%s/%s", prefix, repairHash[:2], repairHash,
		)),
		Body: bytes.NewReader([]byte("damaged")),
	})
	require.NoError(t, err)
	repairPlan, err := metadata.PlanStorageRecovery(
		t.Context(), "repair", repairHash, secondary.ID,
	)
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createS3RecoveryOperation(t, metadata, repairPlan),
	))
	repairedLocation, err := runner.currentLocation(
		t.Context(), repairHash, secondary.ID,
	)
	require.NoError(t, err)
	remote, ok := blobs.ReadBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	repairedStream, repairedSize, err := remote.OpenLoose(
		t.Context(), packstore.Hash(repairHash), *repairedLocation.Loose,
	)
	require.NoError(t, err)
	repairedContent, err := io.ReadAll(repairedStream)
	require.NoError(t, err)
	require.NoError(t, repairedStream.Close())
	assert.Equal(t, int64(len("repairable content")), repairedSize)
	assert.Equal(t, []byte("repairable content"), repairedContent)

	attached, ok := blobs.WritableBackend(packstore.StoreID(secondary.ID))
	require.True(t, ok)
	taken := ownership
	taken.Epoch = "50000000-0000-4000-8000-000000000001"
	require.NoError(t, attached.ReplaceOwnership(t.Context(), taken, &ownership))
	observation := blobs.RefreshStore(t.Context(), secondary.ID)
	assert.Equal(t, StoreFenced, observation.State)
	salvagePlan, err := metadata.PlanStorageRecovery(
		t.Context(), "salvage", remoteHash, secondary.ID,
	)
	require.NoError(t, err)
	require.NoError(t, runner.Run(
		t.Context(), createS3RecoveryOperation(t, metadata, salvagePlan),
	))
	assertBlobContent(t, blobs, remoteHash, []byte("remote-only content"))

	reclaim, err := openBackend(t.Context(), binding, nil)
	require.NoError(t, err)
	require.NoError(t, reclaim.ReplaceOwnership(t.Context(), ownership, &taken))
	require.NoError(t, closeBackend(reclaim))
	assert.Equal(t, StoreOnline, blobs.RefreshStore(t.Context(), secondary.ID).State)
	evacuation, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: metadata.RootID(), SourceStoreID: secondary.ID,
		DestinationStoreID: metadata.PrimaryBlobStoreID(), RetireSource: true,
		Evacuate: true,
	})
	require.NoError(t, err)
	require.NoError(t, metadata.BeginBlobStoreEvacuation(t.Context(), secondary.ID))
	require.NoError(t, runner.Run(
		t.Context(), createS3PlacementOperation(t, metadata, "evacuate", evacuation),
	))
	after, err := metadata.BlobStoreBySelector(t.Context(), secondary.ID)
	require.NoError(t, err)
	assert.Equal(t, "detached", after.Lifecycle)
	assertBlobContent(t, blobs, packedHash, []byte("packed source content"))
}

func createS3PlacementFile(
	t *testing.T,
	metadata *store.Store,
	blobs *Store,
	name string,
	content []byte,
) store.Node {
	t.Helper()
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(content))
	require.NoError(t, err)
	encoding, err := written.EncodingName()
	require.NoError(t, err)
	node, err := metadata.CreateFile(
		t.Context(), metadata.RootID(), name, written.Hash, written.Size,
		"text/plain", store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize,
			PackEligible: written.PackEligible, Created: written.Created,
		},
	)
	require.NoError(t, err)
	return node
}

func placeS3TestFile(
	t *testing.T,
	metadata *store.Store,
	runner PlacementRunner,
	node store.Node,
	destination string,
	retire bool,
) string {
	t.Helper()
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{
		TargetNodeID: node.ID, SourceStoreID: metadata.PrimaryBlobStoreID(),
		DestinationStoreID: destination, RetireSource: retire,
	})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 1)
	assert.Equal(t, plan.TransferBytes, plan.ReadBackBytes)
	assert.Equal(t, plan.TransferBytes, plan.RemoteEgressBytes)
	assert.Zero(t, plan.ScratchBytes)
	require.NoError(t, runner.Run(
		t.Context(), createS3PlacementOperation(t, metadata, "place", plan),
	))
	return plan.Hashes[0].Hash
}

func createS3PlacementOperation(
	t *testing.T,
	metadata *store.Store,
	kind string,
	plan store.PlacementPlan,
) string {
	t.Helper()
	requestJSON, err := json.Marshal(plan.Request)
	require.NoError(t, err)
	planJSON, err := json.Marshal(plan)
	require.NoError(t, err)
	operation, err := metadata.CreateStorageOperation(
		t.Context(), store.StorageOperationCreate{
			Kind: kind, RequestDigest: plan.Digest,
			SourceStoreID: func() string {
				if kind == "evacuate" {
					return plan.Request.SourceStoreID
				}
				return ""
			}(),
			RequestJSON: string(requestJSON), PlanJSON: string(planJSON),
			TotalObjects: int64(len(plan.Hashes)),
		},
	)
	require.NoError(t, err)
	return operation.ID
}

func createS3RecoveryOperation(
	t *testing.T,
	metadata *store.Store,
	plan store.StorageRecoveryPlan,
) string {
	t.Helper()
	encoded, err := json.Marshal(plan)
	require.NoError(t, err)
	operation, err := metadata.CreateStorageOperation(
		t.Context(), store.StorageOperationCreate{
			Kind: plan.Kind, RequestDigest: plan.Digest,
			RequestJSON: string(encoded), PlanJSON: string(encoded), TotalObjects: 1,
		},
	)
	require.NoError(t, err)
	return operation.ID
}

func assertBlobContent(
	t *testing.T, blobs *Store, hash string, expected []byte,
) {
	t.Helper()
	stream, size, err := blobs.OpenStreamContext(t.Context(), hash)
	require.NoError(t, err)
	actual, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Equal(t, int64(len(expected)), size)
	assert.Equal(t, expected, actual)
}

func newS3TestClient(
	endpoint, region, accessKey, secretKey string,
) *s3.Client {
	cfg := aws.Config{
		Region: region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
			accessKey, secretKey, "",
		)),
	}
	return s3.NewFromConfig(cfg, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})
}

func writeS3TestProfile(t *testing.T, accessKey, secretKey string) string {
	t.Helper()
	const profile = "docbank-minio"
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(credentialsPath, fmt.Appendf(nil,
		"[%s]\naws_access_key_id = %s\naws_secret_access_key = %s\n",
		profile, accessKey, secretKey,
	), 0o600))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsPath)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	return profile
}

func cleanupS3TestBucket(t *testing.T, client *s3.Client, bucket string) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	var continuation *string
	for {
		page, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), ContinuationToken: continuation,
		})
		if err != nil {
			t.Logf("list MinIO cleanup objects: %v", err)
			return
		}
		for _, object := range page.Contents {
			_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket), Key: object.Key,
			})
			if err != nil {
				t.Logf("delete MinIO cleanup object: %v", err)
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		continuation = page.NextContinuationToken
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		t.Logf("delete MinIO test bucket: %v", err)
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
