package qmdexport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadCurrentDoesNotAdoptOrRepair(t *testing.T) {
	// Catches creation/repair during a read-only load.
	target := publicationTarget(t)
	_, err := LoadCurrent(target)
	require.Error(t, err)
	require.NoDirExists(t, target)
	receipt := publishOne(t, target)
	require.NoError(t, os.Remove(filepath.Join(target, lockName)))
	_, err = LoadCurrent(target)
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(target, lockName))
	require.DirExists(t, receipt.CollectionPath)
}

func TestLoadCurrentValidatesCanonicalManifestAndRootStamp(t *testing.T) {
	// Catches selecting a foreign stamp, lenient manifest, or malformed pointer.
	for _, change := range []string{"pointer", "canonical", "stamp", "collection-link"} {
		t.Run(change, func(t *testing.T) {
			target := publicationTarget(t)
			receipt := publishOne(t, target)
			switch change {
			case "pointer":
				require.NoError(t, os.WriteFile(filepath.Join(target, "CURRENT"), []byte(receipt.GenerationID), 0o600))
			case "canonical":
				name := filepath.Join(filepath.Dir(receipt.CollectionPath), "manifest.json")
				value, err := os.ReadFile(name)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(name, append([]byte(" "), value...), 0o600))
			case "stamp":
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(receipt.CollectionPath), generationStampName), []byte("{}\n"), 0o600))
			case "collection-link":
				renamed := filepath.Join(target, "synthetic-preserved-collection")
				require.NoError(t, os.Rename(receipt.CollectionPath, renamed))
				if err := os.Symlink(renamed, receipt.CollectionPath); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			loaded, err := LoadCurrent(target)
			require.Error(t, err)
			require.Empty(t, loaded.GenerationID)
		})
	}
}
