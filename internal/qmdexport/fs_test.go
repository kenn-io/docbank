package qmdexport

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnchoredComponentsAndEnumeration(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	r, err := acquireOwnedRoot(t.Context(), filepath.Join(base, "export"), ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	for _, name := range []string{"", ".", "..", "a/b", `a\b`, "x:y", "\x00", "C:"} {
		_, _, err := r.staging.createFile(name, movableEntry)
		require.Error(t, err, "%q", name)
		_, _, err = r.staging.openDir(name)
		require.Error(t, err, "%q", name)
	}
	for i := range 130 {
		f, _, err := r.staging.createFile(fmt.Sprintf("file-%03d", i), movableEntry)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	for range 2 {
		entries, err := r.staging.entries(t.Context(), 130)
		require.NoError(t, err)
		require.Len(t, entries, 130)
	}
	_, err = r.staging.entries(t.Context(), 129)
	require.ErrorIs(t, err, errDirectoryBound)
}

func TestNativeNoOverwriteAndAtomicReplacement(t *testing.T) {
	base := syntheticPrivateBase(t)
	syntheticSentinel(t, base)
	r, err := acquireOwnedRoot(t.Context(), filepath.Join(base, "export"), ownershipHooks{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	stage, id, err := r.staging.createDir("stage", movableEntry)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stage.Close()) })
	f, _, err := stage.createFile("sentinel", movableEntry)
	require.NoError(t, err)
	_, err = f.WriteString("first generation\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	stageReader, _, err := r.staging.openMovableDir("stage")
	require.NoError(t, err)
	require.NoError(t, stageReader.Close())
	require.NoError(t, moveDirectoryNoReplace(r.staging, "stage", stage, id, r.generations, "selected"))
	other, otherID, err := r.staging.createDir("other", movableEntry)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	require.Error(t, moveDirectoryNoReplace(r.staging, "other", other, otherID, r.generations, "selected"))
	require.Equal(t, []byte("first generation\n"), readSyntheticSentinel(t, filepath.Join(r.absolute, "generations", "selected", "sentinel")))
	old, oldID, err := r.root.createFile("pointer", movableEntry)
	require.NoError(t, err)
	_, err = old.WriteString("old\n")
	require.NoError(t, err)
	require.NoError(t, old.Close())
	reader, _, err := r.root.openFile("pointer")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	next, nextID, err := r.root.createFile("next", movableEntry)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, next.Close()) })
	_, err = next.WriteString("new\n")
	require.NoError(t, err)
	require.NoError(t, confirmSelectedFile(next))
	require.NoError(t, replaceFile(r.root, "next", next, nextID, r.root, "pointer"))
	require.NoError(t, confirmSelectedFile(next))
	require.Error(t, r.root.sameEntry("pointer", oldID))
	require.Equal(t, []byte("new\n"), readSyntheticSentinel(t, filepath.Join(r.absolute, "pointer")))
	buf := make([]byte, 4)
	_, err = reader.ReadAt(buf, 0)
	require.NoError(t, err)
	require.Equal(t, []byte("old\n"), buf)
}
