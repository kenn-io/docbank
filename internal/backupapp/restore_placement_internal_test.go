package backupapp

import (
	"bytes"
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

func TestClaimRestoreBackendFencesPostClaimPublication(t *testing.T) {
	namespace := filepath.Join(t.TempDir(), "archive")
	binding := config.StoreBindingConfig{Kind: "filesystem", Path: namespace}
	candidate := store.BlobStore{
		ID: "10000000-0000-4000-8000-000000000009", Name: "archive",
		OwnershipEpoch: "20000000-0000-4000-8000-000000000009",
	}
	backend, err := claimRestoreBackend(
		t.Context(), "30000000-0000-4000-8000-000000000009",
		candidate, binding, false, make(map[packstore.Ownership]string),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if closer, ok := backend.(interface{ Close() error }); ok {
			require.NoError(t, closer.Close())
		}
	})
	inspector, err := blob.NewFilesystemBackend(namespace, nil)
	require.NoError(t, err)
	current, err := inspector.Ownership(t.Context())
	require.NoError(t, err)
	taken := current
	taken.Epoch = "20000000-0000-4000-8000-000000000010"
	require.NoError(t, inspector.ReplaceOwnership(t.Context(), taken, &current))
	require.NoError(t, inspector.Close())

	_, err = backend.PublishLoose(
		t.Context(),
		packstore.Hash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		bytes.NewReader([]byte("x")),
		packstore.PublishOptions{ExpectedSize: 1, SizeKnown: true},
	)
	require.ErrorIs(t, err, packstore.ErrStoreFenced)
}

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
		filepath.Join(root, "restore"), mapping, manifest, bindings, nil,
	)
	require.ErrorContains(t, err, "overlaps")
}

func TestPrepareRestoreMappingsRejectsInvalidAndPrimaryStoreNames(t *testing.T) {
	manifest := placementManifest{Stores: []placementStore{{
		ID: "10000000-0000-4000-8000-000000000001",
	}}}
	for _, name := range []string{"bad\nname", "primary"} {
		t.Run(name, func(t *testing.T) {
			mapping := RestoreStoreMap{
				Version: RestoreStoreMapVersion,
				Stores: []RestoreStoreMapping{{
					SourceID: manifest.Stores[0].ID,
					Name:     name, Binding: "archive",
				}},
			}
			_, err := prepareRestoreMappings(
				t.TempDir(), mapping, manifest,
				map[string]config.StoreBindingConfig{
					"archive": {Kind: "filesystem", Path: t.TempDir()},
				}, nil,
			)
			require.Error(t, err)
		})
	}
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

	_, err := prepareRestoreMappings(t.TempDir(), mapping, manifest, bindings, nil)
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

	_, err := prepareRestoreMappings(t.TempDir(), mapping, manifest, bindings, nil)
	require.ErrorContains(t, err, "overlaps")
}

func TestPrepareRestoreMappingsRejectsProtectedFilesystemNamespaces(t *testing.T) {
	root := t.TempDir()
	manifest := placementManifest{Stores: []placementStore{{
		ID: "10000000-0000-4000-8000-000000000001",
	}}}
	mapping := RestoreStoreMap{
		Version: RestoreStoreMapVersion,
		Stores: []RestoreStoreMapping{{
			SourceID: manifest.Stores[0].ID, Name: "archive", Binding: "archive",
		}},
	}
	for _, protected := range []string{
		filepath.Join(root, "live-vault"), filepath.Join(root, "backup-repository"),
	} {
		t.Run(filepath.Base(protected), func(t *testing.T) {
			bindings := map[string]config.StoreBindingConfig{
				"archive": {Kind: "filesystem", Path: filepath.Join(protected, "secondary")},
			}
			_, err := prepareRestoreMappings(
				filepath.Join(root, "restore"), mapping, manifest, bindings,
				[]string{
					filepath.Join(root, "live-vault"),
					filepath.Join(root, "backup-repository"),
				},
			)
			require.ErrorContains(t, err, "protected storage")
		})
	}
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

	first := store.BlobStore{
		ID:             "10000000-0000-4000-8000-000000000001",
		Name:           "first",
		OwnershipEpoch: "20000000-0000-4000-8000-000000000001",
	}
	second := store.BlobStore{
		ID:             "10000000-0000-4000-8000-000000000002",
		Name:           "second",
		OwnershipEpoch: "20000000-0000-4000-8000-000000000002",
	}
	claims := make(map[packstore.Ownership]string)
	firstBackend, err := claimRestoreBackend(
		t.Context(), "30000000-0000-4000-8000-000000000001", first,
		config.StoreBindingConfig{
			Kind: "filesystem", Path: filepath.Join(root, "Archive"),
		},
		false, claims,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		if closer, ok := firstBackend.(interface{ Close() error }); ok {
			require.NoError(t, closer.Close())
		}
	})

	_, err = claimRestoreBackend(
		t.Context(), "30000000-0000-4000-8000-000000000001", second,
		config.StoreBindingConfig{
			Kind: "filesystem", Path: filepath.Join(root, "archive"),
		},
		true, claims,
	)
	require.ErrorContains(t, err, "already claimed by target store")

	claimed, err := blob.NewFilesystemBackend(
		filepath.Join(root, "Archive"), nil,
	)
	require.NoError(t, err)
	actual, err := claimed.Ownership(t.Context())
	require.NoError(t, err)
	asserted := packstore.Ownership{
		Format: packstore.OwnershipFormatV1,
		Vault:  "30000000-0000-4000-8000-000000000001",
		Store:  packstore.StoreID(first.ID),
		Epoch:  first.OwnershipEpoch,
	}
	require.Equal(t, asserted, actual)
	require.NoError(t, claimed.Close())
}
