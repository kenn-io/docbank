package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

// KIT_STREAM_TEST_BYTES can exercise the same path above the pack frame limit.
func TestLargeExtrasCaptureVerifyPruneRestore(t *testing.T) {
	r := initTestRepo(t)
	dbPath, contentDir, dataDir, _ := seedBackupFixture(t)
	size := largeBackupStreamTestBytes(t, 64<<20+1)
	extraDir := t.TempDir()
	extra := writeLargeAttachment(t, extraDir, size, true)
	opts := createOpts(dbPath, contentDir, dataDir, t.TempDir())
	opts.Extras = ExtrasSpec{Files: []ExtrasFileSpec{{
		Path: filepath.Join(extraDir, extra.StoragePath), RecordAs: "recovery/history.db",
	}}}
	first, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(t, err)
	require.Equal(t, 6, first.MinReaderVersion, "older readers cannot restore chunked extras")
	second, err := Create(t.Context(), r, newTestApp(), opts)
	require.NoError(t, err)
	require.Less(t, second.BytesAdded, size, "unchanged extras must reuse stored chunks")
	_, err = Forget(t.Context(), r, ForgetOptions{SnapshotIDs: []string{second.SnapshotID}})
	require.NoError(t, err)
	// Force live chunks through prune's repacking path, not just a no-op walk.
	consolidatePruneFixture(t, r, 1<<20)
	pruned, err := Prune(t.Context(), r, newTestApp(), PruneOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, pruned.PacksToRepack)
	verified, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: first.SnapshotID})
	require.NoError(t, err)
	require.Empty(t, verified.Problems)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = Restore(t.Context(), r, newTestApp(), RestoreOptions{SnapshotID: first.SnapshotID, TargetDir: target})
	require.NoError(t, err)
	restored := filepath.Join(target, "recovery", "history.db")
	gotSize, gotHash := hashFileStream(t, restored)
	require.Equal(t, size, gotSize)
	require.Equal(t, extra.Hash, gotHash)
	if runtime.GOOS != "windows" {
		// Windows reports 0666 for writable files, not POSIX permissions.
		info, err := os.Stat(restored)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestChunkedExtrasRejectCorruptObjects(t *testing.T) {
	for _, fault := range []string{"hash", "size", "missing chunk"} {
		t.Run(fault, func(t *testing.T) {
			r, m := buildVerifyFixture(t)
			known, err := r.LoadBlobIndex()
			require.NoError(t, err)
			a := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
			t.Cleanup(a.Abort)
			first, _, err := a.Add([]byte("first"))
			require.NoError(t, err)
			second, _, err := a.Add([]byte("second"))
			require.NoError(t, err)
			whole := pack.ComputeBlobID([]byte("firstsecond"))
			recipe := objectRecipe{
				Version: 1, Blob: whole.String(), Bytes: 11,
				Chunks: objectChunks{{Blob: first.String(), Bytes: 5}, {Blob: second.String(), Bytes: 6}},
			}
			size := int64(11)
			switch fault {
			case "hash":
				recipe.Chunks[0], recipe.Chunks[1] = recipe.Chunks[1], recipe.Chunks[0]
			case "size":
				size++
			case "missing chunk":
				recipe.Chunks[1].Blob = pack.ComputeBlobID([]byte("absent")).String()
			}
			raw, err := json.Marshal(recipe)
			require.NoError(t, err)
			recipeID, _, err := a.Add(raw)
			require.NoError(t, err)
			raw, err = json.Marshal(ExtrasTree{Entries: []ExtrasEntry{{
				Path: "recovery/history.db", Blob: whole.String(), Size: size, Mode: 0o600,
			}}})
			require.NoError(t, err)
			treeID, _, err := a.Add(raw)
			require.NoError(t, err)
			_, entries, err := a.Finish()
			require.NoError(t, err)
			_, err = r.WriteIndex(entries)
			require.NoError(t, err)
			m.FormatVersion, m.MinReaderVersion = 6, 6
			m.Extras = ManifestExtras{Tree: treeID.String(), Recipes: []string{recipeID.String()}}
			id, err := r.WriteManifest(m)
			require.NoError(t, err)
			verified, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: id})
			require.NoError(t, err)
			require.NotEmpty(t, verified.Problems)
			if fault == "missing chunk" {
				require.Len(t, verified.Problems, 1, "a missing chunk must not also report a missing whole-file blob")
				require.Contains(t, verified.Problems[0].Detail, "object chunk "+recipe.Chunks[1].Blob+" not present in any index")
				quick, err := Verify(t.Context(), r, newTestApp(), VerifyOptions{SnapshotID: id, Quick: true})
				require.NoError(t, err)
				require.Equal(t, verified.Problems, quick.Problems)
			}
			target := filepath.Join(t.TempDir(), "restore")
			_, err = Restore(t.Context(), r, newTestApp(), RestoreOptions{SnapshotID: id, TargetDir: target})
			require.Error(t, err)
			if fault == "size" {
				require.ErrorContains(t, err, "object recipe differs from recorded size")
				require.NoDirExists(t, target, "preflight must reject the size before creating the target")
			}
			require.NoFileExists(t, filepath.Join(target, "app.db"))
			require.NoFileExists(t, filepath.Join(target, "recovery", "history.db"))
			staged, err := filepath.Glob(filepath.Join(target, "recovery", ".restore-*"))
			require.NoError(t, err)
			require.Empty(t, staged)
		})
	}
}

// Not parallel: the pack sync hook changes the real source file after its
// first chunk has been read, so no sleep or racing writer is needed.
func TestCaptureExtrasRejectsSizeChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delta   int64
		wantErr string
	}{
		{name: "growth", delta: 1, wantErr: "exceeds declared size"},
		{name: "shrinkage", delta: -1, wantErr: "differs from declared size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := initTestRepo(t)
			path := filepath.Join(t.TempDir(), "history.db")
			require.NoError(t, os.WriteFile(path, nil, 0o600))
			const size = int64(64<<20 + 2)
			require.NoError(t, os.Truncate(path, size))
			a := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
			t.Cleanup(a.Abort)
			a.targetSize = 1 // Seal the first chunk before reading the rest.
			originalSync := pack.SyncDir
			t.Cleanup(func() { pack.SyncDir = originalSync })
			changed := false
			pack.SyncDir = func(dir string) error {
				if !changed {
					changed = true
					if err := os.Truncate(path, size+tc.delta); err != nil {
						return err
					}
				}
				return originalSync(dir)
			}
			captured, err := CaptureExtras(t.Context(), ExtrasOptions{
				Spec: ExtrasSpec{Files: []ExtrasFileSpec{{Path: path, RecordAs: "history.db"}}},
			}, a)
			require.True(t, changed)
			require.ErrorContains(t, err, tc.wantErr)
			require.Empty(t, captured.Tree)
		})
	}
}
