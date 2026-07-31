package backupapp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestPrepareRestoreMappingsRejectsOverlappingFilesystemNamespaces(t *testing.T) {
	root := t.TempDir()
	manifest := placementManifest{Stores: []placementStore{
		{ID: "10000000-0000-4000-8000-000000000001"},
		{ID: "10000000-0000-4000-8000-000000000002"},
	}}
	mapping := RestoreStoreMap{
		Version: RestoreStoreMapVersion,
		Stores: []RestoreStoreMapping{
			{
				SourceID: manifest.Stores[0].ID, Name: "first", Binding: "first",
			},
			{
				SourceID: manifest.Stores[1].ID, Name: "second", Binding: "second",
			},
		},
	}
	bindings := map[string]config.StoreBindingConfig{
		"first": {
			Kind: "filesystem", Path: filepath.Join(root, "archive"),
		},
		"second": {
			Kind: "filesystem", Path: filepath.Join(root, "archive", "nested"),
		},
	}

	_, err := prepareRestoreMappings(
		filepath.Join(root, "restore"), mapping, manifest, bindings,
	)
	require.ErrorContains(t, err, "overlaps")
}

func TestPrepareRestoreMappingsRejectsOverlappingS3Prefixes(t *testing.T) {
	manifest := placementManifest{Stores: []placementStore{
		{ID: "10000000-0000-4000-8000-000000000001"},
		{ID: "10000000-0000-4000-8000-000000000002"},
	}}
	mapping := RestoreStoreMap{
		Version: RestoreStoreMapVersion,
		Stores: []RestoreStoreMapping{
			{
				SourceID: manifest.Stores[0].ID, Name: "first", Binding: "first",
			},
			{
				SourceID: manifest.Stores[1].ID, Name: "second", Binding: "second",
			},
		},
	}
	bindings := map[string]config.StoreBindingConfig{
		"first": {
			Kind: "s3", Endpoint: "https://objects.example",
			Bucket: "archive", Prefix: "docbank",
		},
		"second": {
			Kind: "s3", Endpoint: "https://OBJECTS.EXAMPLE/",
			Bucket: "archive", Prefix: "docbank/nested",
		},
	}

	_, err := prepareRestoreMappings(t.TempDir(), mapping, manifest, bindings)
	require.ErrorContains(t, err, "overlaps")
}

func TestPrepareRestoreMappingsRejectsImplicitAndExplicitAWSEndpoints(t *testing.T) {
	manifest := placementManifest{Stores: []placementStore{
		{ID: "10000000-0000-4000-8000-000000000001"},
		{ID: "10000000-0000-4000-8000-000000000002"},
	}}
	mapping := RestoreStoreMap{
		Version: RestoreStoreMapVersion,
		Stores: []RestoreStoreMapping{
			{
				SourceID: manifest.Stores[0].ID, Name: "first", Binding: "first",
			},
			{
				SourceID: manifest.Stores[1].ID, Name: "second", Binding: "second",
			},
		},
	}
	bindings := map[string]config.StoreBindingConfig{
		"first": {
			Kind: "s3", Region: "us-east-1", Bucket: "archive",
			Prefix: "docbank",
		},
		"second": {
			Kind: "s3", Endpoint: "https://s3.us-east-1.amazonaws.com",
			Region: "us-east-1", Bucket: "archive", Prefix: "docbank/nested",
		},
	}

	_, err := prepareRestoreMappings(t.TempDir(), mapping, manifest, bindings)
	require.ErrorContains(t, err, "overlaps")
}

func TestCanonicalS3EndpointUsesAWSSDKPartitions(t *testing.T) {
	tests := []struct {
		name      string
		region    string
		endpoint  string
		partition string
	}{
		{
			name: "commercial", region: "us-east-1",
			endpoint: "https://s3.us-east-1.amazonaws.com", partition: "aws",
		},
		{
			name: "default region", endpoint: "https://s3.us-east-1.amazonaws.com",
			partition: "aws",
		},
		{
			name: "china", region: "cn-north-1",
			endpoint: "https://s3.cn-north-1.amazonaws.com.cn", partition: "aws-cn",
		},
		{
			name: "govcloud", region: "us-gov-west-1",
			endpoint: "https://s3.us-gov-west-1.amazonaws.com", partition: "aws-us-gov",
		},
		{
			name: "european sovereign", region: "eusc-de-east-1",
			endpoint: "https://s3.eusc-de-east-1.amazonaws.eu", partition: "aws-eusc",
		},
		{
			name: "iso", region: "us-iso-east-1",
			endpoint: "https://s3.us-iso-east-1.c2s.ic.gov", partition: "aws-iso",
		},
		{
			name: "iso b", region: "us-isob-east-1",
			endpoint: "https://s3.us-isob-east-1.sc2s.sgov.gov", partition: "aws-iso-b",
		},
		{
			name: "iso e", region: "eu-isoe-west-1",
			endpoint: "https://s3.eu-isoe-west-1.cloud.adc-e.uk", partition: "aws-iso-e",
		},
		{
			name: "iso f", region: "us-isof-south-1",
			endpoint: "https://s3.us-isof-south-1.csp.hci.ic.gov", partition: "aws-iso-f",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			implicit, err := canonicalS3Endpoint("", test.region)
			require.NoError(t, err)
			require.Equal(t, test.partition, implicit)
			explicit, err := canonicalS3Endpoint(test.endpoint, test.region)
			require.NoError(t, err)
			require.Equal(t, test.partition, explicit)
		})
	}

	commercial, err := canonicalS3Endpoint("", "us-east-1")
	require.NoError(t, err)
	govCloud, err := canonicalS3Endpoint(
		"https://s3.us-gov-west-1.amazonaws.com", "us-gov-west-1",
	)
	require.NoError(t, err)
	require.NotEqual(t, commercial, govCloud)
}

func TestApplyRestorePlacementRejectsCaseInsensitiveFilesystemAliasClaim(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "CaseProbe")
	require.NoError(t, os.WriteFile(probe, []byte("probe"), 0o600))
	if _, err := os.Stat(filepath.Join(root, "caseprobe")); errors.Is(err, fs.ErrNotExist) {
		t.Skip("test requires a case-insensitive filesystem")
	} else {
		require.NoError(t, err)
	}

	target := filepath.Join(root, "restore")
	require.NoError(t, os.Mkdir(target, 0o700))
	databasePath := filepath.Join(target, "docbank.db")
	metadata, err := store.Open(databasePath)
	require.NoError(t, err)
	require.NoError(t, metadata.Close())

	manifest := placementManifest{Stores: []placementStore{
		{ID: "10000000-0000-4000-8000-000000000001"},
		{ID: "10000000-0000-4000-8000-000000000002"},
	}}
	mapping := RestoreStoreMap{
		Version: RestoreStoreMapVersion,
		Stores: []RestoreStoreMapping{
			{
				SourceID: manifest.Stores[0].ID, Name: "first", Binding: "first",
			},
			{
				SourceID: manifest.Stores[1].ID, Name: "second",
				Binding: "second", Takeover: true,
			},
		},
	}
	err = applyRestorePlacement(
		t.Context(), target, databasePath, store.DefaultSQLiteDriver(), manifest,
		RestorePlacementOptions{
			Map: &mapping,
			Bindings: map[string]config.StoreBindingConfig{
				"first": {
					Kind: "filesystem", Path: filepath.Join(root, "Archive"),
				},
				"second": {
					Kind: "filesystem", Path: filepath.Join(root, "archive"),
				},
			},
		},
	)
	require.ErrorContains(t, err, "already claimed by target store")

	restored, err := store.Open(databasePath)
	require.NoError(t, err)
	first, err := restored.BlobStoreBySelector(t.Context(), "first")
	require.NoError(t, err)
	vaultID := restored.VaultID()
	require.NoError(t, restored.Close())
	claimed, err := blob.NewFilesystemBackend(
		filepath.Join(root, "Archive"), nil,
	)
	require.NoError(t, err)
	actual, err := claimed.Ownership(t.Context())
	require.NoError(t, err)
	asserted := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  vaultID, Store: packstore.StoreID(first.ID),
		Epoch: first.OwnershipEpoch,
	}
	require.Equal(t, asserted, actual)
	require.NoError(t, claimed.Close())
}
