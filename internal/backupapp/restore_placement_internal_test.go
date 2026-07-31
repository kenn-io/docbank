package backupapp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
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
