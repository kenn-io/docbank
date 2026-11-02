package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

type benchmarkContentSource struct{ content []byte }

func (s benchmarkContentSource) Open(context.Context, ContentRef) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.content)), nil
}

// A source with per-open latency exposes capture scheduling separately from
// compression throughput. Allocation reporting catches object-sized buffers
// allocated for small references whose size was not known to the caller.
func BenchmarkCaptureUnknownThumbnails(b *testing.B) {
	src := &mapSource{blobs: map[string][]byte{}}
	var refs []ContentRef
	for i := range 64 {
		body := bytes.Repeat([]byte{byte(i)}, 4096)
		ref, hash := sourceRef(body)
		ref.Size = -1
		refs = append(refs, ref)
		src.blobs[hash] = body
	}
	delayed := captureSourceFunc(func(ctx context.Context, ref ContentRef) (io.ReadCloser, error) {
		select {
		case <-time.After(10 * time.Millisecond): //nolint:kennlint // deadlinetest: simulated per-open source latency that the benchmark measures, not a wait on test state
			return src.Open(ctx, ref)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	root := b.TempDir()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		repo, err := Init(filepath.Join(root, fmt.Sprintf("repo-%d", i)))
		require.NoError(b, err)
		appender := NewPackAppender(repo, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
		_, err = CaptureAttachments(b.Context(), "", append([]ContentRef(nil), refs...), nil, appender,
			CaptureOptions{Jobs: 8, Source: delayed})
		require.NoError(b, err)
		_, _, err = appender.Finish()
		require.NoError(b, err)
	}
}

func BenchmarkBackupCaptureStream(b *testing.B) {
	contents := map[string][]byte{
		"raw-1MiB":        backupBenchmarkNoise(1 << 20),
		"compressed-1MiB": bytes.Repeat([]byte("backup stream benchmark "), (1<<20)/24),
	}
	for name, content := range contents {
		b.Run(name, func(b *testing.B) {
			id := pack.ComputeBlobID(content)
			ref := ContentRef{Hash: id.String(), Size: int64(len(content))}
			root := b.TempDir()
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			b.ResetTimer()
			for i := range b.N {
				repo, err := Init(filepath.Join(root, fmt.Sprintf("repo-%d", i)))
				require.NoError(b, err)
				appender := NewPackAppender(repo, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, ".benchpack")
				_, err = CaptureAttachments(b.Context(), "", []ContentRef{ref}, map[string]bool{}, appender,
					CaptureOptions{Jobs: 1, Source: benchmarkContentSource{content: content}})
				require.NoError(b, err)
				_, _, err = appender.Finish()
				require.NoError(b, err)
			}
		})
	}
}

func BenchmarkRepoStreamingReads(b *testing.B) {
	contents := map[string][]byte{
		"raw-1MiB":        backupBenchmarkNoise(1 << 20),
		"compressed-1MiB": bytes.Repeat([]byte("backup stream benchmark "), (1<<20)/24),
	}
	for name, content := range contents {
		b.Run(name, func(b *testing.B) {
			require := require.New(b)
			repo, err := Init(filepath.Join(b.TempDir(), "repo"))
			require.NoError(err)
			known := map[pack.BlobID]IndexEntry{}
			appender := NewPackAppender(repo, known, pack.DefaultZstdLevel, nil, testPackExt)
			id, _, err := appender.Add(content)
			require.NoError(err)
			_, _, err = appender.Finish()
			require.NoError(err)
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			b.ResetTimer()
			for range b.N {
				reader, err := repo.OpenBlob(b.Context(), known, id, nil, testPackExt)
				require.NoError(err)
				_, copyErr := io.Copy(io.Discard, reader)
				require.NoError(errors.Join(copyErr, reader.Close()))
			}
		})
	}
}

func backupBenchmarkNoise(size int) []byte {
	result := make([]byte, size)
	var state uint32 = 1
	for i := range result {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		result[i] = byte(state)
	}
	return result
}
