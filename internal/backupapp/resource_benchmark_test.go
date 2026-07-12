package backupapp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

type resourceNoiseReader struct{ state uint32 }

func (r *resourceNoiseReader) Read(p []byte) (int, error) {
	for i := range p {
		r.state ^= r.state << 13
		r.state ^= r.state >> 17
		r.state ^= r.state << 5
		p[i] = byte(r.state)
	}
	return len(p), nil
}

type resourceFixture struct {
	root     string
	metadata *store.Store
	blobs    *blob.Store
	nodes    []store.Node
	closed   bool
}

func (f *resourceFixture) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return errors.Join(f.blobs.Close(), f.metadata.Close())
}

func newResourceFixture(b *testing.B, sizes ...int64) *resourceFixture {
	b.Helper()
	root := b.TempDir()
	metadata, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(b, err)
	blobsDir := filepath.Join(root, "blobs")
	require.NoError(b, os.MkdirAll(filepath.Join(blobsDir, "tmp"), 0o700))
	blobs, err := blob.New(store.NewPackCatalog(metadata), blobsDir)
	require.NoError(b, err)
	fixture := &resourceFixture{root: root, metadata: metadata, blobs: blobs}
	b.Cleanup(func() { require.NoError(b, fixture.Close()) })
	require.NoError(b, blobs.WithMutation(context.Background(), func() error {
		for i, size := range sizes {
			hash, written, writeErr := blobs.WriteContext(context.Background(),
				io.LimitReader(&resourceNoiseReader{state: uint32(i + 1)}, size))
			if writeErr != nil {
				return writeErr
			}
			node, createErr := metadata.CreateFile(context.Background(), metadata.RootID(),
				fmt.Sprintf("resource-%d.bin", i), hash, written, "application/octet-stream")
			if createErr != nil {
				return createErr
			}
			fixture.nodes = append(fixture.nodes, node)
		}
		return nil
	}))
	return fixture
}

func BenchmarkDocbankVerifiedRead64MiB(b *testing.B) {
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "packed"
		}
		b.Run(name, func(b *testing.B) {
			fixture := newResourceFixture(b, blob.MaxBlobBytes)
			if packed {
				stats, err := fixture.blobs.Maintainer().Pack(context.Background(), packstore.PackOptions{})
				require.NoError(b, err)
				require.Equal(b, 1, stats.BlobsPacked)
			}
			b.ReportAllocs()
			b.SetBytes(blob.MaxBlobBytes)
			baselineFDs := 0
			if entries, readErr := filepath.Glob("/dev/fd/*"); readErr == nil {
				baselineFDs = len(entries)
			}
			b.ResetTimer()
			peakFDs := baselineFDs
			for range b.N {
				stream, size, err := fixture.blobs.OpenStream(fixture.nodes[0].BlobHash)
				require.NoError(b, err)
				if entries, readErr := filepath.Glob("/dev/fd/*"); readErr == nil {
					peakFDs = max(peakFDs, len(entries))
				}
				require.Equal(b, blob.MaxBlobBytes, size)
				_, copyErr := io.Copy(io.Discard, stream)
				require.NoError(b, copyErr)
				require.True(b, stream.Verified())
				require.NoError(b, stream.Close())
			}
			if baselineFDs > 0 {
				b.ReportMetric(float64(peakFDs-baselineFDs), "stream-fds")
			}
		})
	}
}

func BenchmarkDocbankWritePackRepack64MiB(b *testing.B) {
	part := blob.MaxBlobBytes / 3
	b.ReportAllocs()
	b.SetBytes(blob.MaxBlobBytes)
	for range b.N {
		fixture := newResourceFixture(b, part, part, blob.MaxBlobBytes-2*part)
		_, err := fixture.blobs.Maintainer().Pack(context.Background(), packstore.PackOptions{TargetSize: 128 << 20})
		require.NoError(b, err)
		require.NoError(b, fixture.blobs.WithMutation(context.Background(), func() error {
			for _, node := range fixture.nodes[1:] {
				if _, _, trashErr := fixture.metadata.Trash(context.Background(), node.ID, -1); trashErr != nil {
					return trashErr
				}
			}
			if _, emptyErr := fixture.metadata.TrashEmpty(context.Background(), 0, true); emptyErr != nil {
				return emptyErr
			}
			return fixture.metadata.DeleteBlobRows(context.Background(),
				[]string{fixture.nodes[1].BlobHash, fixture.nodes[2].BlobHash})
		}))
		stats, err := fixture.blobs.Maintainer().Repack(context.Background(), packstore.RepackOptions{
			Now: time.Now().UTC().Add(48 * time.Hour),
			Selection: packstore.RepackSelection{
				MinAge: time.Nanosecond, MinDeadStored: 1,
			},
		})
		require.NoError(b, err)
		require.Equal(b, 1, stats.PacksRewritten)
		require.NoError(b, fixture.Close())
	}
}

func BenchmarkDocbankBackupRoundTrip64MiB(b *testing.B) {
	fixture := newResourceFixture(b, blob.MaxBlobBytes)
	app := backupapp.New("benchmark-version")
	root := b.TempDir()
	b.ReportAllocs()
	b.SetBytes(3 * blob.MaxBlobBytes)
	b.ResetTimer()
	for i := range b.N {
		repo, err := backup.Init(filepath.Join(root, fmt.Sprintf("repo-%d", i)))
		require.NoError(b, err)
		_, err = backup.Create(context.Background(), repo, app, backup.CreateOptions{
			DBPath:        filepath.Join(fixture.root, "docbank.db"),
			ContentSource: backupapp.NewContentSource(fixture.blobs), Jobs: 1,
		})
		require.NoError(b, err)
		verified, err := backup.Verify(context.Background(), repo, app, backup.VerifyOptions{Jobs: 1})
		require.NoError(b, err)
		require.Empty(b, verified.Problems)
		_, err = backup.Restore(context.Background(), repo, app, backup.RestoreOptions{
			TargetDir: filepath.Join(root, fmt.Sprintf("restore-%d", i)), Jobs: 1,
		})
		require.NoError(b, err)
	}
}
