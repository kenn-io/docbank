package backup_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func TestChunkedBackupRestoreAndPrune(t *testing.T) {
	ctx := t.Context()
	base := t.TempDir()
	repo, err := backup.Init(filepath.Join(base, "repo"))
	require.NoError(t, err)
	content := bytes.Repeat([]byte("large content\n"), (64<<20)/14+2)
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	contentDir := filepath.Join(base, "content")
	rel := filepath.Join(hash[:2], hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(contentDir, rel)), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(contentDir, rel), content, 0o600))
	record := portableRecord{
		Notes: []string{strings.Repeat("n", (64<<20)+1)},
		Files: []portableFile{{Hash: hash, Size: int64(len(content)), Path: filepath.ToSlash(rel)}},
	}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	source := &portableSource{
		raw: raw, stats: json.RawMessage(`{"notes":1,"files":1}`),
		info: &backup.ContentInfo{Rows: 1, Refs: []backup.ContentRef{{Hash: hash, Size: int64(len(content))}}},
	}
	manifest, err := backup.Create(ctx, repo, portableApp{}, backup.CreateOptions{
		MetadataSource: source, ContentDir: contentDir, Jobs: 1,
	})
	require.NoError(t, err)
	require.Equal(t, 5, manifest.MinReaderVersion)
	require.NotEmpty(t, manifest.Attachments.Recipes)
	require.NotEmpty(t, manifest.Metadata.Recipe)
	source.info.Refs[0].Size = -1
	second, err := backup.Create(ctx, repo, portableApp{}, backup.CreateOptions{MetadataSource: source, ContentDir: contentDir, Jobs: 1})
	require.NoError(t, err)
	assert.Equal(t, manifest.SnapshotID, second.ParentID)
	assert.Equal(t, int64(len(content)), source.info.Refs[0].Size, "capture backfills unknown sizes in the caller's refs")
	assert.Equal(t, manifest.Attachments.Recipes, second.Attachments.Recipes)
	assert.Zero(t, second.BytesAdded, "unchanged chunks and recipes must deduplicate")
	source.info.Refs[0].Size = -1
	contentSource := &fakeContentSource{blobs: map[string][]byte{hash: content}}
	third, err := backup.Create(ctx, repo, portableApp{}, backup.CreateOptions{
		MetadataSource: source, ContentSource: contentSource, Jobs: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, second.SnapshotID, third.ParentID)
	assert.Equal(t, manifest.Attachments.Recipes, third.Attachments.Recipes)
	assert.Zero(t, third.BytesAdded, "unknown source sizes must preserve chunk deduplication")
	assert.Equal(t, int64(len(content)), source.info.Refs[0].Size)

	source.info.Refs[0].Size = -1
	contentSource.blobs[hash] = []byte("changed content")
	_, err = backup.Create(ctx, repo, portableApp{}, backup.CreateOptions{
		MetadataSource: source, ContentSource: contentSource, Jobs: 1,
	})
	require.ErrorContains(t, err, "does not match its hash", "recorded sizes must not bypass source verification")
	for _, quick := range []bool{true, false} {
		result, err := backup.Verify(ctx, repo, portableApp{}, backup.VerifyOptions{Quick: quick, All: true})
		require.NoError(t, err)
		require.Empty(t, result.Problems)
	}
	require.NoError(t, os.RemoveAll(contentDir))
	_, err = backup.Prune(ctx, repo, portableApp{}, backup.PruneOptions{})
	require.NoError(t, err)
	target := filepath.Join(base, "restored")
	result, err := backup.Restore(ctx, repo, portableApp{}, backup.RestoreOptions{
		SnapshotID: third.SnapshotID, TargetDir: target, MetadataRestorer: portableRestorer{},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), result.AttachmentBytes)
	file, err := os.Open(filepath.Join(target, "content", rel))
	require.NoError(t, err)
	digest := sha256.New()
	_, err = io.Copy(digest, file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	assert.Equal(t, hash, hex.EncodeToString(digest.Sum(nil)))

	known, err := repo.LoadBlobIndex()
	require.NoError(t, err)
	recipeID, err := pack.ParseBlobID(manifest.Attachments.Recipes[0])
	require.NoError(t, err)
	encoded, err := repo.ReadBlob(known, recipeID, nil, portableApp{}.PackFileExtension())
	require.NoError(t, err)
	for _, fault := range []string{"reordered", "missing"} {
		t.Run(fault, func(t *testing.T) {
			var recipe map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &recipe))
			var chunks []json.RawMessage
			require.NoError(t, json.Unmarshal(recipe["chunks"], &chunks))
			if fault == "reordered" {
				chunks[0], chunks[1] = chunks[1], chunks[0]
			} else {
				var chunk map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(chunks[0], &chunk))
				chunk["blob"] = json.RawMessage(`"` + strings.Repeat("0", 64) + `"`)
				chunks[0], err = json.Marshal(chunk)
				require.NoError(t, err)
			}
			recipe["chunks"], err = json.Marshal(chunks)
			require.NoError(t, err)
			raw, err := json.Marshal(recipe)
			require.NoError(t, err)
			appender := backup.NewPackAppender(repo, known, 3, nil, portableApp{}.PackFileExtension())
			t.Cleanup(appender.Abort)
			id, _, err := appender.Add(raw)
			require.NoError(t, err)
			_, entries, err := appender.Finish()
			require.NoError(t, err)
			_, err = repo.WriteIndex(entries)
			require.NoError(t, err)
			broken := *manifest
			broken.Attachments.Recipes = []string{id.String()}
			snapshotID, err := repo.WriteManifest(&broken)
			require.NoError(t, err)
			verified, err := backup.Verify(t.Context(), repo, portableApp{}, backup.VerifyOptions{SnapshotID: snapshotID})
			require.NoError(t, err)
			require.NotEmpty(t, verified.Problems)
			for _, compressed := range []bool{false, true} {
				failedTarget := filepath.Join(t.TempDir(), "failed")
				_, err = backup.Restore(t.Context(), repo, portableApp{}, backup.RestoreOptions{
					SnapshotID: snapshotID, TargetDir: failedTarget, MetadataRestorer: portableRestorer{},
					LooseCompression:  packstore.LooseCompressionOptions{Enabled: compressed},
					BeforePublication: func(context.Context, backup.RestorePublicationTarget) error { return nil },
				})
				require.Error(t, err)
				assert.NoFileExists(t, filepath.Join(failedTarget, portableApp{}.DBFileName()), "failed chunk verification must not publish database authority")
				assert.NoFileExists(t, filepath.Join(failedTarget, "content", rel))
				assert.NoFileExists(t, filepath.Join(failedTarget, "content", rel+".zst"))
			}
		})
	}
}
