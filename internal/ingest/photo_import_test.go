package ingest

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoImportGrouping(t *testing.T) {
	root := t.TempDir()
	grouped := GroupPhotoCandidates([]PhotoImportCandidate{
		{Path: filepath.Join(root, "IMG_0001.ARW"), Kind: store.PhotoSourceRAW},
		{Path: filepath.Join(root, "IMG_0001.JPG"), Kind: store.PhotoSourceImage},
		{Path: filepath.Join(root, "IMG_0001.XMP"), Kind: store.PhotoSourceSidecar},
		{Path: filepath.Join(root, "CLIP.MP4"), Kind: store.PhotoSourceVideo},
	})
	require.Len(t, grouped, 2)
	assert.Len(t, grouped[0].Members, 3)
	assert.Len(t, grouped[1].Members, 1)

	for _, name := range []string{"IMG_0001.ARW", "IMG_0001.DNG"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(name), 0o600))
	}
	ing := newTestIngester(t)
	report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Ambiguous)
	require.Len(t, report.Run.Ambiguities, 1)
	require.Len(t, report.Run.Ambiguities[0].Candidates, 2)
	assert.NotEmpty(t, report.Run.Ambiguities[0].Candidates[0].SourcePath)
	assert.NotEmpty(t, report.Run.Ambiguities[0].Candidates[0].BlobHash)
}

func TestPhotoImportAmbiguity(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.ARW"), []byte("raw-one"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.DNG"), []byte("raw-two"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.JPG"), []byte("jpeg"), 0o600))
	ing := newTestIngester(t)
	first, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	require.Len(t, first.Run.Ambiguities, 1)
	choice := first.Run.Ambiguities[0].Candidates[0]
	second, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{Choice: &store.PhotoImportChoice{
		GroupKey: first.Run.Ambiguities[0].GroupKey, RawSourcePath: choice.SourcePath, RawBlobHash: choice.BlobHash,
	}})
	require.NoError(t, err)
	assert.Equal(t, 2, second.Added)
	assert.Zero(t, second.Ambiguous)
	firstAsset, err := ing.Store.PhotoAssetForNode(t.Context(), nodeByName(t, ing, "/photos/IMG_0001.ARW").ID)
	require.NoError(t, err)
	secondAsset, err := ing.Store.PhotoAssetForNode(t.Context(), nodeByName(t, ing, "/photos/IMG_0001.DNG").ID)
	require.NoError(t, err)
	assert.NotEqual(t, firstAsset.ID, secondAsset.ID)
	assert.Len(t, firstAsset.Files, 2)
	assert.Len(t, secondAsset.Files, 1)
}

func TestPhotoImportAmbiguityWithExistingRaw(t *testing.T) {
	root := t.TempDir()
	rawOne := filepath.Join(root, "IMG_0001.ARW")
	require.NoError(t, os.WriteFile(rawOne, []byte("raw-one"), 0o600))
	ing := newTestIngester(t)
	first, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, first.Added)
	rawTwo := filepath.Join(root, "IMG_0001.DNG")
	require.NoError(t, os.WriteFile(rawTwo, []byte("raw-two"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "IMG_0001.JPG"), []byte("jpeg"), 0o600))
	ambiguous, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, ambiguous.Ambiguous)
	require.Len(t, ambiguous.Run.Ambiguities[0].Candidates, 2)
	choice := ambiguous.Run.Ambiguities[0].Candidates[0]
	if choice.SourcePath != rawOne {
		choice = ambiguous.Run.Ambiguities[0].Candidates[1]
	}
	resolved, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{Choice: &store.PhotoImportChoice{
		GroupKey: ambiguous.Run.Ambiguities[0].GroupKey, RawSourcePath: choice.SourcePath, RawBlobHash: choice.BlobHash,
	}})
	require.NoError(t, err)
	assert.Equal(t, 2, resolved.Added)
	assert.Zero(t, resolved.Ambiguous)
	firstAsset, err := ing.Store.PhotoAssetForNode(t.Context(), nodeByName(t, ing, "/photos/IMG_0001.ARW").ID)
	require.NoError(t, err)
	secondAsset, err := ing.Store.PhotoAssetForNode(t.Context(), nodeByName(t, ing, "/photos/IMG_0001.DNG").ID)
	require.NoError(t, err)
	assert.NotEqual(t, firstAsset.ID, secondAsset.ID)
	assert.Len(t, firstAsset.Files, 2)
	assert.Len(t, secondAsset.Files, 1)
}

func nodeByName(t *testing.T, ing *Ingester, path string) store.Node {
	t.Helper()
	node, err := ing.Store.NodeByPath(t.Context(), path)
	require.NoError(t, err)
	return node
}

func TestPhotoImportDiscovery(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "capture.JPG")
	require.NoError(t, os.WriteFile(keep, []byte("jpeg"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("junk"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("skip"), 0o600))
	ignored := filepath.Join(root, ".Spotlight-V100")
	require.NoError(t, os.Mkdir(ignored, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ignored, "hidden.JPG"), []byte("skip"), 0o600))
	linkTarget := filepath.Join(root, "linked.JPG")
	require.NoError(t, os.WriteFile(linkTarget, []byte("linked"), 0o600))
	link := filepath.Join(root, "descendant.JPG")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	candidates, err := discoverPhotoCandidates(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	assert.ElementsMatch(t, []string{keep, linkTarget}, []string{candidates[0].Path, candidates[1].Path})

	rootLink := filepath.Join(t.TempDir(), "camera-link")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Skipf("root symbolic links unavailable: %v", err)
	}
	candidates, err = discoverPhotoCandidates(t.Context(), rootLink)
	require.NoError(t, err)
	assert.NotEmpty(t, candidates)
}

func TestPhotoImportSourceStability(t *testing.T) {
	ing := newTestIngester(t)
	ctx := t.Context()
	for _, test := range []struct {
		name string
		edit func(t *testing.T, path string, before localFileFingerprint)
	}{
		{name: "size changed", edit: func(t *testing.T, path string, _ localFileFingerprint) {
			t.Helper()
			require.NoError(t, os.WriteFile(path, []byte("changed-size"), 0o600))
		}},
		{name: "same-size replacement", edit: func(t *testing.T, path string, before localFileFingerprint) {
			t.Helper()
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.WriteFile(path, []byte("same"), 0o600))
			require.NoError(t, os.Chtimes(path, before.info.ModTime(), before.info.ModTime()))
		}},
		{name: "disappeared", edit: func(t *testing.T, path string, _ localFileFingerprint) {
			t.Helper()
			require.NoError(t, os.Remove(path))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.JPG")
			require.NoError(t, os.WriteFile(path, []byte("same"), 0o600))
			before, err := observeLocalFileFingerprint(path)
			require.NoError(t, err)
			test.edit(t, path, before)
			_, err = ing.readLocalFile(ctx, path, path, nil, &before)
			require.Error(t, err)
			if test.name != "disappeared" {
				assert.ErrorIs(t, err, ErrSourceChanged)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "capture.JPG")
	other := filepath.Join(t.TempDir(), "other.JPG")
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o600))
	require.NoError(t, os.WriteFile(other, []byte("other"), 0o600))
	before, err := observeLocalFileFingerprint(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(other, path))
	_, err = ing.readLocalFile(ctx, path, path, nil, &before)
	require.Error(t, err)
	assert.Error(t, err)
}

func TestPhotoImportPublishedBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "capture.JPG")
	content := []byte("published photo bytes")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	ing := newTestIngester(t)
	report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Added)
	node, err := ing.Store.NodeByPath(t.Context(), "/photos/capture.JPG")
	require.NoError(t, err)
	reader, err := ing.Blobs.Open(node.BlobHash)
	require.NoError(t, err)
	got, readErr := io.ReadAll(reader)
	require.NoError(t, errors.Join(readErr, reader.Close()))
	assert.Equal(t, got, content)
}

func TestPhotoImportGateAndActivity(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "activity.JPG"), []byte("activity"), 0o600))
	ing := newTestIngester(t)
	begin, end := 0, 0
	report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
		ActivityBegin: func() { begin++ },
		ActivityEnd:   func() { end++ },
	})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Added)
	assert.Equal(t, 1, begin)
	assert.Equal(t, 1, end)
}
