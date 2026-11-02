package backup

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestSnapshotReaderRejectsUnselectedObjects(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	repo := initTestRepo(t)
	appender := NewPackAppender(repo, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	t.Cleanup(appender.Abort)
	add := func(data []byte) pack.BlobID {
		id, _, err := appender.Add(data)
		require.NoError(err)
		return id
	}
	selected := add([]byte("selected"))
	other := add([]byte("other"))
	orphan := add([]byte("orphan"))
	metadata := add([]byte(`{"notes":[]}`))
	list, err := EncodeAttachmentList([]ContentRef{{Hash: selected.String(), Size: 8}})
	require.NoError(err)
	listID := add(list)
	otherList, err := EncodeAttachmentList([]ContentRef{{Hash: other.String(), Size: 5}})
	require.NoError(err)
	otherListID := add(otherList)
	_, entries, err := appender.Finish()
	require.NoError(err)
	_, err = repo.WriteIndex(entries)
	require.NoError(err)
	m := &Manifest{
		FormatVersion: 3, MinReaderVersion: 3, CreatedAt: "2026-01-01T00:00:00Z",
		Metadata:    &ManifestMetadata{Format: "test-json-v1", Blob: metadata.String(), Bytes: 12},
		Attachments: ManifestAttachments{Lists: []string{listID.String()}},
	}
	selectedSnapshot, err := repo.WriteManifest(m)
	require.NoError(err)
	m.Attachments.Lists = []string{otherListID.String()}
	_, err = repo.WriteManifest(m)
	require.NoError(err)
	reader, err := repo.OpenSnapshot(t.Context(), selectedSnapshot, testPackExt)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(reader.Close()) })
	for _, id := range []pack.BlobID{other, orphan, metadata, listID} {
		var output bytes.Buffer
		n, copyErr := reader.CopyObject(t.Context(), id, &output)
		require.ErrorIs(copyErr, fs.ErrNotExist)
		assert.Zero(n)
		assert.Empty(output.Bytes())
	}
	var output bytes.Buffer
	_, err = reader.CopyObject(t.Context(), selected, &output)
	require.NoError(err)
	assert.Equal("selected", output.String())
}

func TestSnapshotReaderRejectsDamagedObjects(t *testing.T) {
	for _, fault := range []string{
		"reordered", "missing chunk", "chunk size", "recipe size", "conflicting sizes",
		"conflicting extras", "duplicate extras", "case-folded extras",
		"footer size", "index footer", "payload", "new reader", "valid",
	} {
		t.Run(fault, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			repo := initTestRepo(t)
			appender := NewPackAppender(repo, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
			t.Cleanup(appender.Abort)
			add := func(data []byte) pack.BlobID {
				id, _, err := appender.Add(data)
				require.NoError(err)
				return id
			}
			a, b := add([]byte("a")), add([]byte("b"))
			whole := pack.ComputeBlobID([]byte("ab"))
			recipe := objectRecipe{
				Version: 1, Blob: whole.String(), Bytes: 2,
				Chunks: objectChunks{{Blob: a.String(), Bytes: 1}, {Blob: b.String(), Bytes: 1}},
			}
			ref := ContentRef{Hash: whole.String(), Size: 2}
			switch fault {
			case "reordered":
				recipe.Chunks[0], recipe.Chunks[1] = recipe.Chunks[1], recipe.Chunks[0]
			case "missing chunk":
				recipe.Chunks[1].Blob = pack.ComputeBlobID([]byte("absent")).String()
			case "chunk size":
				recipe.Chunks[0].Bytes = 2
				recipe.Bytes = 3
				ref.Size = 3
			case "recipe size":
				ref.Size = 3
			case "footer size":
				ref = ContentRef{Hash: a.String(), Size: 2}
			}
			raw, err := json.Marshal(recipe)
			require.NoError(err)
			recipeID := add(raw)
			refs := []ContentRef{ref}
			if fault == "conflicting sizes" {
				refs = []ContentRef{{Hash: a.String(), Size: 1}, {Hash: a.String(), Size: 2}}
			}
			list, err := EncodeAttachmentList(refs)
			require.NoError(err)
			listID := add(list)
			tree := ExtrasTree{Entries: []ExtrasEntry{{Path: "recovery.db", Blob: a.String(), Size: 1}}}
			switch fault {
			case "conflicting extras":
				tree.Entries = append(tree.Entries, ExtrasEntry{Path: "recovery.db", Blob: b.String(), Size: 1})
			case "duplicate extras":
				tree.Entries = append(tree.Entries, tree.Entries[0])
			case "case-folded extras":
				tree.Entries = append(tree.Entries, ExtrasEntry{Path: "Recovery.db", Blob: a.String(), Size: 1})
			}
			raw, err = json.Marshal(tree)
			require.NoError(err)
			treeID := add(raw)
			_, entries, err := appender.Finish()
			require.NoError(err)
			for i := range entries {
				if entries[i].Blob != a {
					continue
				}
				if fault == "index footer" {
					entries[i].StoredLen++
				}
				if fault == "payload" {
					file, openErr := os.OpenFile(repo.packPath(entries[i].PackID, testPackExt), os.O_RDWR, 0)
					require.NoError(openErr)
					_, writeErr := file.WriteAt([]byte("x"), int64(entries[i].Offset))
					require.NoError(writeErr)
					require.NoError(file.Close())
				}
			}
			_, err = repo.WriteIndex(entries)
			require.NoError(err)
			m := testManifest("2026-01-01T00:00:00Z", "", 0)
			m.FormatVersion, m.MinReaderVersion = 6, 6
			m.Attachments = ManifestAttachments{Lists: []string{listID.String()}, Recipes: []string{recipeID.String()}}
			m.Extras.Tree = treeID.String()
			if fault == "new reader" {
				m.MinReaderVersion = SupportedReaderVersion + 1
			}
			snapshotID, err := repo.WriteManifest(m)
			require.NoError(err)
			reader, err := repo.OpenSnapshot(t.Context(), snapshotID, testPackExt)
			if reader != nil {
				t.Cleanup(func() { assert.NoError(reader.Close()) })
			}
			switch fault {
			case "conflicting extras", "duplicate extras":
				require.ErrorContains(err, `lists path "recovery.db" twice`)
			case "case-folded extras":
				require.ErrorContains(err, "collide under case-folded key")
			}
			if err == nil {
				id, parseErr := pack.ParseBlobID(ref.Hash)
				require.NoError(parseErr)
				_, err = reader.CopyObject(t.Context(), id, io.Discard)
				if fault == "valid" {
					require.NoError(err)
					var output bytes.Buffer
					_, copyErr := reader.CopyObject(t.Context(), recipeID, &output)
					require.ErrorIs(copyErr, fs.ErrNotExist)
					assert.Empty(output.Bytes())
				} else {
					require.Error(err)
				}
				require.NoError(reader.Close())
			} else {
				assert.NotEqual("valid", fault)
			}
			// Both constructor and copy failures must leave maintenance possible.
			lock, lockErr := repo.AcquireExclusiveLock("test", false)
			require.NoError(lockErr)
			require.NoError(lock.Release())
		})
	}
}
