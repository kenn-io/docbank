package api

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
)

func TestPhotoImportCheckpointPersistsSelectionAndValidatesFormatAndHeaderSize(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "imports")
	owner := photoCheckpointStore{dir: dir}
	next := &photoImportCheckpoint{SourceID: uuid.New(), Revision: uuid.New(), Destination: "/", ManifestVersion: 2, ManifestHash: strings.Repeat("a", 64), PickerKind: "files"}
	require.NoError(t, owner.replace("profile", photoCheckpointChange{Next: next}))
	file := filepath.Join(dir, "profile.jsonl")
	// A new owner after restart loads the original source and selection digest.
	reopened := photoCheckpointStore{dir: dir}
	header, loaded, err := reopened.read("profile", true)
	require.NoError(t, err)
	require.Equal(t, next, loaded)
	require.Equal(t, next.SourceID, header.SourceID)
	original, err := os.ReadFile(file)
	require.NoError(t, err)
	// Unsupported formats survive read, replacement, and clear attempts.
	future := strings.Replace(string(original), `"formatVersion":2`, `"formatVersion":999`, 1)
	require.NoError(t, os.WriteFile(file, []byte(future), 0600))
	_, _, err = reopened.read("profile", true)
	require.ErrorContains(t, err, "unsupported or malformed format")
	require.ErrorContains(t, reopened.replace("profile", photoCheckpointChange{Expected: &photoCheckpointIdentity{SourceID: header.SourceID, Revision: header.Revision}}), "unsupported or malformed format")
	preserved, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, future, string(preserved))
	// Unknown members, broken JSON, and other manifest versions are malformed formats.
	line, rest, _ := strings.Cut(strings.TrimSuffix(string(original), "\n"), "\n")
	for _, contents := range []string{
		strings.Replace(line, `"formatVersion":2`, `"formatVersion":2,"unknown":true`, 1) + "\n" + rest,
		line + "\n" + strings.Replace(rest, `"destination"`, `"unknown":true,"destination"`, 1),
		"{broken\n" + rest,
		line + "\n{broken",
		line + "\n" + strings.Replace(rest, `"manifestVersion":2`, `"manifestVersion":1`, 1),
		line + "\n" + strings.Replace(rest, `"manifestVersion":2`, `"manifestVersion":99`, 1),
	} {
		require.NoError(t, os.WriteFile(file, []byte(contents), 0600))
		_, _, err = reopened.read("profile", true)
		require.ErrorContains(t, err, "unsupported or malformed format", contents)
	}
	// The small header remains readable even when the body cannot be decoded.
	require.NoError(t, os.WriteFile(file, original[:bytes.IndexByte(original, '\n')+1], 0600))
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.WriteString(strings.Repeat("x", photoCheckpointMaxBytes+1))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	bounded, _, err := reopened.read("profile", false)
	require.NoError(t, err)
	require.Equal(t, header, bounded)
	_, _, err = reopened.read("profile", true)
	require.ErrorContains(t, err, "unsupported or malformed format")
}

func TestPhotoImportCheckpointClearRemovesFile(t *testing.T) {
	t.Parallel()
	owner := photoCheckpointStore{dir: filepath.Join(t.TempDir(), "imports")}
	require.NoError(t, owner.replace("profile", photoCheckpointChange{}))
	require.NoDirExists(t, owner.dir)
	next := &photoImportCheckpoint{SourceID: uuid.New(), Revision: uuid.New(), Destination: "/", ManifestVersion: 2, ManifestHash: strings.Repeat("a", 64), PickerKind: "files"}
	require.NoError(t, owner.replace("profile", photoCheckpointChange{Next: next}))
	require.NoError(t, owner.replace("profile", photoCheckpointChange{Expected: &photoCheckpointIdentity{SourceID: next.SourceID, Revision: next.Revision}}))
	require.NoFileExists(t, filepath.Join(owner.dir, "profile.jsonl"))
}
