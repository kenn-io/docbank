package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestCaptureReusesStoredWholeBlob(t *testing.T) {
	for _, source := range []bool{false, true} {
		name := "directory"
		if source {
			name = "source"
		}
		t.Run(name, func(t *testing.T) {
			repo := initTestRepo(t)
			db, dir, data, writer := seedBackupFixture(t)
			ref := writeLargeAttachment(t, dir, 64<<20+1, true)
			_, err := writer.ExecContext(t.Context(),
				`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
				 VALUES (?, ?, ?, '', '')`, ref.Hash, ref.StoragePath, ref.Size)
			require.NoError(t, err)

			// Seed the whole-file representation used by existing backups.
			file, err := os.Open(filepath.Join(dir, ref.StoragePath))
			require.NoError(t, err)
			prepared, prepareErr := pack.PrepareBlob(t.Context(), file, uint64(ref.Size), pack.DefaultZstdLevel,
				pack.AppendStreamOptions{ScratchDir: repo.Path(stagingDirName)})
			closeErr := file.Close()
			require.NoError(t, prepareErr)
			require.NoError(t, closeErr)
			appender := NewPackAppender(repo, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
			defer appender.Abort()
			_, err = appender.AddPrepared(t.Context(), prepared)
			require.NoError(t, err)
			_, entries, err := appender.Finish()
			require.NoError(t, err)
			_, err = repo.WriteIndex(entries)
			require.NoError(t, err)

			opts := createOpts(db, dir, data, t.TempDir())
			if source {
				opts.ContentSource = captureSourceFunc(func(_ context.Context, ref ContentRef) (io.ReadCloser, error) {
					rel, err := captureRelPath(ref)
					if err != nil {
						return nil, err
					}
					return os.Open(filepath.Join(dir, rel))
				})
			}
			manifest, err := Create(t.Context(), repo, newTestApp(), opts)
			require.NoError(t, err)
			assert.Empty(t, manifest.Attachments.Recipes, "reuse the existing whole blob")
			assert.Less(t, manifest.MinReaderVersion, 5, "whole blobs do not require the recipe reader")
			verified, err := Verify(t.Context(), repo, newTestApp(), VerifyOptions{SnapshotID: manifest.SnapshotID})
			require.NoError(t, err)
			require.Empty(t, verified.Problems)
			target := filepath.Join(t.TempDir(), "restored")
			_, err = Restore(t.Context(), repo, newTestApp(), RestoreOptions{SnapshotID: manifest.SnapshotID, TargetDir: target})
			require.NoError(t, err)
			size, hash := hashFileStream(t, filepath.Join(target, newTestApp().ContentDirName(), ref.StoragePath))
			assert.Equal(t, ref.Size, size)
			assert.Equal(t, ref.Hash, hash)

			file, err = os.OpenFile(filepath.Join(dir, ref.StoragePath), os.O_WRONLY, 0)
			require.NoError(t, err)
			_, writeErr := file.WriteAt([]byte("changed"), 0)
			closeErr = file.Close()
			require.NoError(t, writeErr)
			require.NoError(t, closeErr)
			_, err = Create(t.Context(), repo, newTestApp(), opts)
			require.ErrorContains(t, err, "does not match its hash", "reuse must still verify the live source")
		})
	}
}

func TestChunkedCaptureLetsSmallSourcesStart(t *testing.T) {
	appender, _, _ := newTestAppenderForSource(t)
	defer appender.Abort()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		opened := make(chan int64, 2)
		source := captureSourceFunc(func(ctx context.Context, ref ContentRef) (io.ReadCloser, error) {
			opened <- ref.Size
			<-ctx.Done()
			return nil, ctx.Err()
		})
		refs := []ContentRef{
			{Hash: blobID("large").String(), Size: 1<<30 + 1},
			{Hash: blobID("small").String(), Size: 1},
		}
		done := make(chan error, 1)
		go func() {
			_, err := CaptureAttachments(ctx, "", refs, nil, appender, CaptureOptions{Jobs: 2, Source: source})
			done <- err
		}()
		synctest.Wait()
		assert.Len(t, opened, 2, "a blocked chunked source must leave room for small workers")
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestCaptureChunkedSource(t *testing.T) {
	dir := t.TempDir()
	ref := writeLargeAttachment(t, dir, 64<<20+1, false)
	for _, mode := range []string{"declared size", "size hint", "wrong hash"} {
		t.Run(mode, func(t *testing.T) {
			input := ref
			switch mode {
			case "size hint":
				input.Size++
			case "wrong hash":
				input.Hash = blobID("different content").String()
			}
			source := captureSourceFunc(func(_ context.Context, _ ContentRef) (io.ReadCloser, error) {
				return os.Open(filepath.Join(dir, ref.StoragePath))
			})
			appender, repo, known := newTestAppenderForSource(t)
			defer appender.Abort()
			capture, err := CaptureAttachments(t.Context(), "", []ContentRef{input}, nil, appender, CaptureOptions{Source: source})
			if mode == "wrong hash" {
				require.ErrorContains(t, err, "does not match its hash")
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, capture.Recipes)
			assert.Equal(t, ref.Size, capture.BlobBytes)
			_, _, err = appender.Finish()
			require.NoError(t, err)
			recipes, err := loadObjectRecipes(t.Context(), repo, known,
				&Manifest{Attachments: ManifestAttachments{Recipes: capture.Recipes}}, testPackExt)
			require.NoError(t, err)
			id, err := pack.ParseBlobID(ref.Hash)
			require.NoError(t, err)
			stream, err := openObject(t.Context(), repo, known, id, recipes, testPackExt)
			require.NoError(t, err)
			digest := sha256.New()
			size, readErr := io.Copy(digest, stream)
			closeErr := stream.Close()
			require.NoError(t, readErr)
			require.NoError(t, closeErr)
			assert.Equal(t, ref.Size, size)
			assert.Equal(t, ref.Hash, hex.EncodeToString(digest.Sum(nil)))
		})
	}
}

func TestCaptureExactlyOneChunk(t *testing.T) {
	dir := t.TempDir()
	ref := writeLargeAttachment(t, dir, 64<<20, true)
	for _, mode := range []string{"content", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			appender, repo, known := newTestAppenderForSource(t)
			defer appender.Abort()
			id, err := pack.ParseBlobID(ref.Hash)
			require.NoError(t, err)
			if mode == "content" {
				capture, err := CaptureAttachments(t.Context(), dir, []ContentRef{ref}, nil, appender, CaptureOptions{})
				require.NoError(t, err)
				assert.Empty(t, capture.Recipes)
			} else {
				file, err := os.Open(filepath.Join(dir, ref.StoragePath))
				require.NoError(t, err)
				gotID, size, recipe, captureErr := captureObject(t.Context(), file, ref.Size, nil, appender)
				closeErr := file.Close()
				require.NoError(t, captureErr)
				require.NoError(t, closeErr)
				assert.Equal(t, id, gotID)
				assert.Equal(t, ref.Size, size)
				assert.Empty(t, recipe)
			}
			_, _, err = appender.Finish()
			require.NoError(t, err)
			stream, err := repo.OpenBlob(t.Context(), known, id, nil, testPackExt)
			require.NoError(t, err)
			digest := sha256.New()
			size, readErr := io.Copy(digest, stream)
			closeErr := stream.Close()
			require.NoError(t, readErr)
			require.NoError(t, closeErr)
			assert.Equal(t, ref.Size, size)
			assert.Equal(t, ref.Hash, hex.EncodeToString(digest.Sum(nil)))
		})
	}
}
