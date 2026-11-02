package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

var errInjectedPrimary = errors.New("injected primary failure")

func TestLooseWriteStreamsAndChecksExpectedMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("streamed-content-"), 32*1024)
	hash := hashForTest(content)
	size := int64(len(content))
	store := newLooseStoreForTest(t, StagingSameDirectory)

	result, err := store.Write(t.Context(), &boundedChunkReader{r: bytes.NewReader(content), max: 32 << 10}, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: hash,
		ExpectedSize: size,
		SizeKnown:    true,
	})
	require.NoError(err)
	assert.Equal(hash, result.Hash)
	assert.Equal(size, result.Size)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(size, result.StoredSize)
	assert.True(result.Created)
	stored, err := os.ReadFile(result.Path)
	require.NoError(err)
	assert.Equal(content, stored)
	assert.Empty(matchingFiles(t, filepath.Dir(result.Path), ".staging-"))
}

func TestLooseWriteBytesComputesIdentityBeforeSameDirectoryStaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("in-memory-content-"), 4096)
	store := newLooseStoreForTest(t, StagingSameDirectory)

	result, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	assert.Equal(hashForTest(content), result.Hash)
	assert.Equal(int64(len(content)), result.Size)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(int64(len(content)), result.StoredSize)
	assert.True(result.Created)
	stored, err := os.ReadFile(result.Path)
	require.NoError(err)
	assert.Equal(content, stored)
}

func TestLooseWriteBytesChecksExpectedMetadata(t *testing.T) {
	require := require.New(t)
	content := []byte("actual")
	store := newLooseStoreForTest(t, StagingSameDirectory)

	_, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: hashForTest([]byte("other")),
	})
	require.ErrorIs(err, ErrContentMismatch)

	_, err = store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedSize: int64(len(content) + 1),
		SizeKnown:    true,
	})
	require.ErrorIs(err, ErrContentMismatch)
}

func TestLooseWriteRejectsHashAndSizeMismatch(t *testing.T) {
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	content := []byte("actual")
	wrongHash := hashForTest([]byte("other"))

	_, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: wrongHash,
	})
	require.ErrorIs(err, ErrContentMismatch)

	actualHash := hashForTest(content)
	_, err = store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: actualHash,
		ExpectedSize: int64(len(content) + 1), SizeKnown: true,
	})
	require.ErrorIs(err, ErrContentMismatch)
	assert.NoFileExists(t, store.layout.LoosePath(actualHash))
}

func TestLooseWriteCancellationAndLimitCleanStaging(t *testing.T) {
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := filepath.Join(store.layout.Root(), "tmp")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := store.Write(ctx, bytes.NewReader([]byte("content")), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash,
	})
	require.ErrorIs(err, context.Canceled)
	assert.Empty(t, matchingFiles(t, stagingDir, ".staging-"))

	_, err = store.Write(t.Context(), bytes.NewReader([]byte("too large")), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: 3,
	})
	require.ErrorIs(err, ErrContentMismatch)
	assert.Empty(t, matchingFiles(t, stagingDir, ".staging-"))
}

func TestLooseWriteCompressionCancellationCleansStaging(t *testing.T) {
	content := bytes.Repeat([]byte("cancel compressed staging\n"), 4096)
	for _, staging := range []StagingMode{StagingSameDirectory, StagingStoreDirectory} {
		for _, cancelAt := range []string{"source read", "zstd write"} {
			t.Run(cancelAt+"/"+stagingName(staging), func(t *testing.T) {
				require := require.New(t)
				store := newLooseStoreForTest(t, staging)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				src := io.Reader(&cancelAfterRead{reader: bytes.NewReader(content), cancel: cancel})
				if cancelAt == "zstd write" {
					src = bytes.NewReader(content)
					originalWriter := newLooseZstdWriter
					newLooseZstdWriter = func(dst io.Writer) (io.WriteCloser, error) {
						writer, err := originalWriter(dst)
						if err != nil {
							return nil, err
						}
						return &cancelOnWriteCloser{WriteCloser: writer, cancel: cancel}, nil
					}
					t.Cleanup(func() { newLooseZstdWriter = originalWriter })
				}
				opts := WriteOptions{
					Durability:   AtomicPublication,
					Dedup:        VerifyFullHash,
					ExpectedHash: hashForTest(content),
					Compression: LooseCompressionOptions{
						Enabled: true,
					},
				}

				_, err := store.Write(ctx, src, opts)

				require.ErrorIs(err, context.Canceled)
				assertNoLooseWriteResidue(t, store, hashForTest(content))
			})
		}
	}
}

func TestLooseWriteCompressionFailureCleansStaging(t *testing.T) {
	content := bytes.Repeat([]byte("zstd write failure\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	writeErr := errors.New("injected zstd write failure")
	originalWriter := newLooseZstdWriter
	newLooseZstdWriter = func(io.Writer) (io.WriteCloser, error) {
		return &errorWriteCloser{err: writeErr}, nil
	}
	t.Cleanup(func() { newLooseZstdWriter = originalWriter })

	_, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, writeErr)
	assertNoLooseWriteResidue(t, store, hashForTest(content))
}

func TestLooseWritePublicationFailureCleansStaging(t *testing.T) {
	content := bytes.Repeat([]byte("publication failure\n"), 4096)
	publishErr := errors.New("injected publication failure")
	originalPublish := publishLooseFile
	publishLooseFile = func(string, string) error { return publishErr }
	t.Cleanup(func() { publishLooseFile = originalPublish })

	for _, staging := range []StagingMode{StagingSameDirectory, StagingStoreDirectory} {
		t.Run(stagingName(staging), func(t *testing.T) {
			store := newLooseStoreForTest(t, staging)
			result, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: hashForTest(content),
				Compression: LooseCompressionOptions{
					Enabled: true,
				},
			})

			require.ErrorIs(t, err, publishErr)
			assert.Equal(t, hashForTest(content), result.Hash)
			assertNoLooseWriteResidue(t, store, result.Hash)
		})
	}
}

// Without hard links, atomicfile.PublishNoReplace renames staging onto the
// canonical name, so the write must tolerate its staging file being consumed.
func TestLooseWriteToleratesPublicationThatConsumesStaging(t *testing.T) {
	content := bytes.Repeat([]byte("portable no-replace publication\n"), 4096)
	for _, compression := range []LooseCompressionOptions{
		{},
		{Enabled: true},
	} {
		name := "raw"
		if compression.Enabled {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingStoreDirectory)
			originalPublish := publishLooseFile
			publishCalls := 0
			publishLooseFile = func(staging, final string) error {
				publishCalls++
				return os.Rename(staging, final)
			}
			t.Cleanup(func() { publishLooseFile = originalPublish })

			result, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: hashForTest(content),
				Compression:  compression,
			})

			require.NoError(err)
			assert.True(result.Created)
			assert.FileExists(result.Path)
			assert.Equal(1, publishCalls)
			assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(result.Hash), ".staging-"))

			deduplicated, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: result.Hash,
				Compression:  compression,
			})
			require.NoError(err)
			assert.False(deduplicated.Created)
			assert.Equal(result.Path, deduplicated.Path)
			assert.Equal(1, publishCalls, "deduplication does not attempt publication")
		})
	}
}

func TestLooseWriteCompressedDurabilitySyncsSelectedFileAndShard(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("durable compressed content\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	originalFileSync := syncLooseFile
	originalDirSync := pack.SyncDir
	var fileSyncs int
	var syncedDirs []string
	syncLooseFile = func(file *os.File) error {
		fileSyncs++
		return originalFileSync(file)
	}
	pack.SyncDir = func(path string) error {
		syncedDirs = append(syncedDirs, filepath.Clean(path))
		return originalDirSync(path)
	}
	t.Cleanup(func() {
		syncLooseFile = originalFileSync
		pack.SyncDir = originalDirSync
	})

	result, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.NoError(err)
	require.Equal(LooseEncodingZstd, result.Encoding)
	assert.Equal(1, fileSyncs, "only the selected staging file is synced")
	assert.Contains(syncedDirs, filepath.Clean(filepath.Dir(result.Path)))
	assert.NoFileExists(store.layout.LoosePath(result.Hash))
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(result.Hash), ".staging-"))
	assert.FileExists(result.Path)
}

func TestLooseWriteDurableStoreStagingSyncsAllUnlinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("durable staging unlink\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := store.layout.LooseStagingDir(hashForTest(content))
	originalSync := syncLooseStagingDir
	var syncs int
	syncLooseStagingDir = func(path string) error {
		syncs++
		assert.Equal(filepath.Clean(stagingDir), filepath.Clean(path))
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"), "all staging unlinks precede their durability sync")
		return originalSync(path)
	}
	t.Cleanup(func() { syncLooseStagingDir = originalSync })

	result, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.NoError(err)
	assert.Equal(1, syncs)
	assert.FileExists(result.Path)
	assert.NoFileExists(store.layout.LoosePath(result.Hash))
}

func TestLooseWriteDurableStoreStagingSyncsCancellationCleanup(t *testing.T) {
	assert := assert.New(t)
	content := bytes.Repeat([]byte("durable cancelled staging\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := store.layout.LooseStagingDir(hashForTest(content))
	originalSync := syncLooseStagingDir
	var syncs int
	syncLooseStagingDir = func(path string) error {
		syncs++
		assert.Equal(filepath.Clean(stagingDir), filepath.Clean(path))
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
		return originalSync(path)
	}
	t.Cleanup(func() { syncLooseStagingDir = originalSync })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	_, err := store.Write(ctx, &cancelAfterRead{reader: bytes.NewReader(content), cancel: cancel}, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(1, syncs)
}

func TestLooseWriteDurableStoreStagingSyncsCreationFailureCleanup(t *testing.T) {
	assert := assert.New(t)
	content := []byte("staging creation cleanup")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := store.layout.LooseStagingDir(hashForTest(content))
	chmodErr := errors.New("injected staging chmod failure")
	originalChmod := chmodLooseStagingFile
	originalSync := syncLooseStagingDir
	chmodLooseStagingFile = func(*os.File, fs.FileMode) error { return chmodErr }
	var syncs int
	syncLooseStagingDir = func(path string) error {
		syncs++
		assert.Equal(filepath.Clean(stagingDir), filepath.Clean(path))
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
		return originalSync(path)
	}
	t.Cleanup(func() {
		chmodLooseStagingFile = originalChmod
		syncLooseStagingDir = originalSync
	})

	_, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: DurablePublication,
		Dedup:      VerifyFullHash,
	})

	require.ErrorIs(t, err, chmodErr)
	assert.Equal(1, syncs)
}

func TestLooseWriteDurableStagingSyncFailureIsReturned(t *testing.T) {
	assert := assert.New(t)
	content := bytes.Repeat([]byte("staging sync failure\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	syncErr := errors.New("injected staging directory sync failure")
	originalSync := syncLooseStagingDir
	syncLooseStagingDir = func(string) error { return syncErr }
	t.Cleanup(func() { syncLooseStagingDir = originalSync })

	result, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, syncErr)
	assert.True(result.Created)
	assert.FileExists(result.Path)
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(result.Hash), ".staging-"))
}

func TestLooseWriteJoinsStagingCloseFailureOnEarlyReturns(t *testing.T) {
	for _, tt := range []struct {
		name        string
		configure   func(*testing.T, context.CancelFunc, error) io.Reader
		opts        func([]byte) WriteOptions
		wantPrimary func(error) bool
	}{
		{
			name: "cancellation",
			configure: func(_ *testing.T, cancel context.CancelFunc, _ error) io.Reader {
				return &cancelAfterRead{reader: bytes.NewReader(bytes.Repeat([]byte("cancel"), 4096)), cancel: cancel}
			},
			opts: func([]byte) WriteOptions {
				return WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, Compression: LooseCompressionOptions{Enabled: true}}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		{
			name: "copy failure",
			configure: func(_ *testing.T, _ context.CancelFunc, primary error) io.Reader {
				return &errorReader{err: primary}
			},
			opts: func([]byte) WriteOptions {
				return WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, Compression: LooseCompressionOptions{Enabled: true}}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, errInjectedPrimary) },
		},
		{
			name: "encoder failure",
			configure: func(t *testing.T, _ context.CancelFunc, primary error) io.Reader {
				t.Helper()
				originalWriter := newLooseZstdWriter
				newLooseZstdWriter = func(io.Writer) (io.WriteCloser, error) {
					return &errorWriteCloser{err: primary}, nil
				}
				t.Cleanup(func() { newLooseZstdWriter = originalWriter })
				return bytes.NewReader(bytes.Repeat([]byte("encoder"), 4096))
			},
			opts: func([]byte) WriteOptions {
				return WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, Compression: LooseCompressionOptions{Enabled: true}}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, errInjectedPrimary) },
		},
		{
			name: "validation failure",
			configure: func(_ *testing.T, _ context.CancelFunc, _ error) io.Reader {
				return bytes.NewReader([]byte("validation content"))
			},
			opts: func(content []byte) WriteOptions {
				return WriteOptions{
					Durability:   AtomicPublication,
					Dedup:        VerifyFullHash,
					ExpectedHash: hashForTest([]byte("different content")),
					Compression:  LooseCompressionOptions{Enabled: true},
				}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, ErrContentMismatch) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanupErr := errors.New("injected staging close failure")
			primaryErr := errInjectedPrimary
			store := newLooseStoreForTest(t, StagingStoreDirectory)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			content := []byte("validation content")
			originalClose := closeLooseStagingFile
			closeLooseStagingFile = func(file *os.File) error {
				return errors.Join(originalClose(file), cleanupErr)
			}
			t.Cleanup(func() { closeLooseStagingFile = originalClose })
			src := tt.configure(t, cancel, primaryErr)

			_, err := store.Write(ctx, src, tt.opts(content))

			require.Error(t, err)
			assert.True(t, tt.wantPrimary(err), "primary failure must remain in the returned error: %v", err)
			require.ErrorIs(t, err, cleanupErr)
		})
	}
}

func TestLooseWriteJoinsStagingRemoveFailure(t *testing.T) {
	primaryErr := errors.New("injected source failure")
	cleanupErr := errors.New("injected staging remove failure")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	originalRemove := removeLooseStagingFile
	removeLooseStagingFile = func(path string) error {
		return errors.Join(originalRemove(path), cleanupErr)
	}
	t.Cleanup(func() { removeLooseStagingFile = originalRemove })

	_, err := store.Write(t.Context(), &errorReader{err: primaryErr}, WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, primaryErr)
	require.ErrorIs(t, err, cleanupErr)
}

func TestLooseWriteRejectsExistingObjectAboveLimit(t *testing.T) {
	require := require.New(t)
	content := []byte("existing object exceeds limit")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	existing, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)

	result, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: existing.Hash,
		ExpectedSize: existing.Size,
		SizeKnown:    true,
		MaxBytes:     existing.Size - 1,
	})
	require.ErrorIs(err, ErrContentMismatch)
	require.Empty(result)
}

func TestLooseWriteDeduplicatedRawResultReportsPhysicalMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("existing raw physical metadata")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)

	result, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})

	require.NoError(err)
	assert.False(result.Created)
	assert.Equal(created.Path, result.Path)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(result.Size, result.StoredSize)
}

func TestLooseWriteReturnsIdentityAfterCompleteStaging(t *testing.T) {
	require := require.New(t)
	content := []byte("identity survives publication failure")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	require.NoError(os.WriteFile(filepath.Dir(store.layout.LoosePath(hash)), []byte("not a directory"), 0o600))

	result, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.Error(err)
	assert.Equal(t, hash, result.Hash)
	assert.Equal(t, int64(len(content)), result.Size)
}

func TestLooseWriteRequiresExplicitPolicies(t *testing.T) {
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	_, err := store.Write(t.Context(), bytes.NewReader(nil), WriteOptions{})
	require.ErrorIs(t, err, ErrInvalidPolicy)
}

func TestLooseWriteValidatesCompressionPolicy(t *testing.T) {
	store := newLooseStoreForTest(t, StagingSameDirectory)
	for _, tt := range []struct {
		name        string
		compression LooseCompressionOptions
		wantErr     bool
	}{
		{
			name:        "negative minimum bytes",
			compression: LooseCompressionOptions{MinBytes: -1},
			wantErr:     true,
		},
		{
			name:        "negative minimum savings",
			compression: LooseCompressionOptions{MinSavingsPercent: -1},
			wantErr:     true,
		},
		{
			name:        "minimum savings above 100",
			compression: LooseCompressionOptions{MinSavingsPercent: 101},
			wantErr:     true,
		},
		{
			name:        "minimum savings of 100",
			compression: LooseCompressionOptions{MinSavingsPercent: 100},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.WriteBytes(t.Context(), []byte("content"), WriteOptions{
				Durability:  AtomicPublication,
				Dedup:       VerifyFullHash,
				Compression: tt.compression,
			})

			if tt.wantErr {
				require.ErrorIs(t, err, ErrInvalidPolicy)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestLooseWriteCompressionPolicy(t *testing.T) {
	compressible := bytes.Repeat([]byte("compressible loose content\n"), 256)
	incompressible := deterministicLooseNoise(8 << 10)
	for _, tt := range []struct {
		name        string
		content     []byte
		compression LooseCompressionOptions
		want        LooseEncoding
	}{
		{
			name:        "disabled",
			content:     compressible,
			compression: LooseCompressionOptions{},
			want:        LooseEncodingRaw,
		},
		{
			name:    "below minimum size",
			content: compressible,
			compression: LooseCompressionOptions{
				Enabled:  true,
				MinBytes: int64(len(compressible)) + 1,
			},
			want: LooseEncodingRaw,
		},
		{
			name:    "incompressible",
			content: incompressible,
			compression: LooseCompressionOptions{
				Enabled: true,
			},
			want: LooseEncodingRaw,
		},
		{
			name:    "compressible",
			content: compressible,
			compression: LooseCompressionOptions{
				Enabled:           true,
				MinBytes:          int64(len(compressible)),
				MinSavingsPercent: 90,
			},
			want: LooseEncodingZstd,
		},
		{
			name:    "compressible at high threshold",
			content: compressible,
			compression: LooseCompressionOptions{
				Enabled:           true,
				MinSavingsPercent: 99,
			},
			want: LooseEncodingZstd,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, write := range []struct {
				name string
				run  func(*LooseStore) (WriteResult, error)
			}{
				{
					name: "stream",
					run: func(store *LooseStore) (WriteResult, error) {
						return store.Write(t.Context(), bytes.NewReader(tt.content), WriteOptions{
							Durability:   AtomicPublication,
							Dedup:        VerifyFullHash,
							ExpectedHash: hashForTest(tt.content),
							Compression:  tt.compression,
						})
					},
				},
				{
					name: "bytes",
					run: func(store *LooseStore) (WriteResult, error) {
						return store.WriteBytes(t.Context(), tt.content, WriteOptions{
							Durability:  AtomicPublication,
							Dedup:       VerifyFullHash,
							Compression: tt.compression,
						})
					},
				},
			} {
				t.Run(write.name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					store := newLooseStoreForTest(t, StagingSameDirectory)

					result, err := write.run(store)

					require.NoError(err)
					assert.Equal(hashForTest(tt.content), result.Hash, "hash describes logical content")
					assert.Equal(int64(len(tt.content)), result.Size, "size describes logical content")
					assert.Equal(tt.want, result.Encoding)
					assert.True(result.Created)
					assert.FileExists(result.Path)
					if tt.want == LooseEncodingZstd {
						assert.Equal(store.layout.CompressedLoosePath(result.Hash), result.Path)
						assert.Less(result.StoredSize, result.Size)
						assert.NoFileExists(store.layout.LoosePath(result.Hash))
					} else {
						assert.Equal(store.layout.LoosePath(result.Hash), result.Path)
						assert.Equal(result.Size, result.StoredSize)
						assert.NoFileExists(store.layout.CompressedLoosePath(result.Hash))
					}
					assert.Empty(matchingFiles(t, filepath.Dir(result.Path), ".staging-"))
				})
			}
		})
	}
}

func TestLooseWriteCompressionExactSavingsBoundaryIncludesHeader(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 100)
	originalWriter := newLooseZstdWriter
	newLooseZstdWriter = func(dst io.Writer) (io.WriteCloser, error) {
		return &fixedSizeEncoder{dst: dst, payloadBytes: 64}, nil
	}
	t.Cleanup(func() { newLooseZstdWriter = originalWriter })

	for _, tt := range []struct {
		name       string
		minSavings int
		want       LooseEncoding
		storedSize int64
	}{
		{
			name:       "exact twenty percent is inclusive",
			minSavings: 20,
			want:       LooseEncodingZstd,
			storedSize: 80,
		},
		{
			name:       "one percent above exact boundary is raw",
			minSavings: 21,
			want:       LooseEncodingRaw,
			storedSize: 100,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)

			result, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability: AtomicPublication,
				Dedup:      VerifyFullHash,
				Compression: LooseCompressionOptions{
					Enabled:           true,
					MinSavingsPercent: tt.minSavings,
				},
			})

			require.NoError(t, err)
			assert.Equal(tt.want, result.Encoding)
			assert.Equal(tt.storedSize, result.StoredSize)
			if tt.want == LooseEncodingZstd {
				assert.FileExists(store.layout.CompressedLoosePath(result.Hash))
				assert.NoFileExists(store.layout.LoosePath(result.Hash))
			} else {
				assert.FileExists(store.layout.LoosePath(result.Hash))
				assert.NoFileExists(store.layout.CompressedLoosePath(result.Hash))
			}
		})
	}
}

func TestLooseWriteCompressionStreamsSourceWithPooledBuffer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("bounded compressed source\n"), 64*1024)
	store := newLooseStoreForTest(t, StagingSameDirectory)

	result, err := store.Write(t.Context(), &boundedChunkReader{
		r:   bytes.NewReader(content),
		max: 32 << 10,
	}, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: hashForTest(content),
		Compression:  LooseCompressionOptions{Enabled: true},
	})

	require.NoError(err)
	assert.Equal(LooseEncodingZstd, result.Encoding)
	assert.Equal(int64(len(content)), result.Size)
	assert.Less(result.StoredSize, result.Size)
	assert.FileExists(result.Path)
}

func TestLooseWriteSupportsEmptyAndStoreDirectoryStaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	result, err := store.Write(t.Context(), bytes.NewReader(nil), WriteOptions{
		Durability: DurablePublication, Dedup: VerifyTypeAndSize,
	})
	require.NoError(err)
	assert.Equal(hashForTest(nil), result.Hash)
	assert.Zero(result.Size)
	assert.FileExists(result.Path)
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(result.Hash), ".staging-"))
}

func TestLooseDurableWriteSurfacesExistingFileSyncFailure(t *testing.T) {
	require := require.New(t)
	content := []byte("durable sync failure")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	syncErr := errors.New("injected loose file sync failure")
	originalSync := syncLooseFile
	syncLooseFile = func(*os.File) error { return syncErr }
	t.Cleanup(func() { syncLooseFile = originalSync })

	_, err = store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: DurablePublication,
		Dedup:      VerifyFullHash,
	})
	require.ErrorIs(err, syncErr)
	assert.FileExists(t, created.Path)
}

func TestLooseDurableWriteRetriesRootSyncAfterDirectoryResidue(t *testing.T) {
	require := require.New(t)
	content := []byte("retry parent directory durability")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	syncErr := errors.New("injected root sync failure")
	originalSyncDir := pack.SyncDir
	var rootSyncs int
	pack.SyncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(store.layout.Root()) {
			rootSyncs++
			if rootSyncs == 1 {
				return syncErr
			}
		}
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })
	opts := WriteOptions{Durability: DurablePublication, Dedup: VerifyFullHash}

	_, err := store.WriteBytes(t.Context(), content, opts)
	require.ErrorIs(err, syncErr)
	_, err = store.WriteBytes(t.Context(), content, opts)
	require.NoError(err)
	assert.Equal(t, 2, rootSyncs, "existing directory residue must not suppress the parent sync retry")
}

func TestLooseDurableWriteSyncsRootForExistingObject(t *testing.T) {
	require := require.New(t)
	content := []byte("upgrade existing object durability")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	_, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	originalSyncDir := pack.SyncDir
	var synced []string
	pack.SyncDir = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	_, err = store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: DurablePublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	assert.Contains(t, synced, filepath.Clean(store.layout.Root()))
}

func TestLooseWriteDedupPrefersCompressedRepresentation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("preferred compressed representation\n"), 256)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled:           true,
			MinSavingsPercent: 50,
		},
	})
	require.NoError(err)
	require.Equal(LooseEncodingZstd, created.Encoding)
	rawPath := store.layout.LoosePath(created.Hash)
	require.NoError(os.WriteFile(rawPath, content, 0o600))

	result, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})

	require.NoError(err)
	assert.False(result.Created)
	assert.Equal(LooseEncodingZstd, result.Encoding)
	assert.Equal(created.Path, result.Path)
	assert.Equal(created.StoredSize, result.StoredSize)
	assert.FileExists(rawPath, "dedup leaves dual-copy reconciliation to maintenance")
	assert.FileExists(created.Path)
}

func TestLooseWriteDedupRejectsCorruptPreferredRepresentation(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("do not fall back from corrupt preferred content\n"), 256)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: true,
		},
	})
	require.NoError(err)
	require.Equal(LooseEncodingZstd, created.Encoding)
	rawPath := store.layout.LoosePath(created.Hash)
	require.NoError(os.WriteFile(rawPath, content, 0o600))
	corrupt := []byte("corrupt preferred copy")
	require.NoError(os.WriteFile(created.Path, corrupt, 0o600))

	_, err = store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})

	require.ErrorIs(err, ErrContentMismatch)
	assert.Equal(t, corrupt, mustReadFile(t, created.Path), "ordinary writes must not replace a corrupt preferred copy")
	assert.Equal(t, content, mustReadFile(t, rawPath), "a valid alternate copy must not mask preferred corruption")
}

func TestLooseWriteDedupRejectsOverlongCompressedPayloadAfterOneExtraByte(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	expected := []byte("short logical content")
	extra := bytes.Repeat([]byte("extra decoded content\n"), 4096)
	overlong := append(bytes.Clone(expected), extra...)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	hash := hashForTest(expected)
	path := store.layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	header := encodeCompressedLooseHeader(uint64(len(expected)))
	var physical bytes.Buffer
	_, err := physical.Write(header[:])
	require.NoError(err)
	encoder, err := zstd.NewWriter(&physical, zstd.WithEncoderConcurrency(1))
	require.NoError(err)
	_, err = encoder.Write(overlong)
	require.NoError(err)
	require.NoError(encoder.Close())
	require.NoError(os.WriteFile(path, physical.Bytes(), 0o600))
	overreadErr := errors.New("decoded beyond expected logical size plus one byte")
	overreadAttempted := false
	originalReader := newLooseZstdReader
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		decoder, err := originalReader(src)
		if err != nil {
			return nil, err
		}
		return &maxDecodedReader{
			reader:            decoder,
			remaining:         int64(len(expected)) + 1,
			overread:          overreadErr,
			overreadAttempted: &overreadAttempted,
		}, nil
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })

	_, err = store.WriteBytes(t.Context(), expected, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})

	require.ErrorIs(err, ErrContentMismatch)
	require.NotErrorIs(err, overreadErr)
	assert.False(overreadAttempted, "verification must stop after one decoded byte beyond the expected size")
	assert.Equal(physical.Bytes(), mustReadFile(t, path), "ordinary dedup must preserve the corrupt preferred copy")
	assert.NoFileExists(store.layout.LoosePath(hash))
}

func TestLooseFullHashRejectsConcatenatedCompressedFrames(t *testing.T) {
	content := bytes.Repeat([]byte("one logical object across two physical frames\n"), 64)
	half := len(content) / 2
	secondFrameEncoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	secondFrame := secondFrameEncoder.EncodeAll(content[half:], nil)
	require.NoError(t, secondFrameEncoder.Close())

	newFixture := func(t *testing.T) (*LooseStore, []byte) {
		t.Helper()
		store := newLooseStoreForTest(t, StagingSameDirectory)
		hash := hashForTest(content)
		writeCompressedLooseFixture(
			t,
			store.layout,
			hash,
			int64(len(content)),
			content[:half],
			func(physical []byte) []byte { return append(physical, secondFrame...) },
		)
		return store, mustReadFile(t, store.layout.CompressedLoosePath(hash))
	}

	t.Run("loose verify", func(t *testing.T) {
		assert := assert.New(t)
		store, before := newFixture(t)
		hash := hashForTest(content)

		result, exists, err := store.Verify(hash, int64(len(content)), VerifyFullHash, AtomicPublication)

		require.ErrorIs(t, err, ErrContentMismatch)
		assert.False(exists)
		assert.False(result.Created)
		assert.Equal(before, mustReadFile(t, store.layout.CompressedLoosePath(hash)))
	})

	t.Run("write dedup", func(t *testing.T) {
		assert := assert.New(t)
		store, before := newFixture(t)
		hash := hashForTest(content)

		result, err := store.WriteBytes(t.Context(), content, WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
		})

		require.ErrorIs(t, err, ErrContentMismatch)
		assert.Equal(hash, result.Hash)
		assert.False(result.Created)
		assert.Equal(before, mustReadFile(t, store.layout.CompressedLoosePath(hash)))
		assert.NoFileExists(store.layout.LoosePath(hash))
	})

	t.Run("stream verify", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		loose, before := newFixture(t)
		hash := hashForTest(content)
		store := newStoreForTest(t, &mapResolver{locations: map[Hash]Location{
			hash: {Member: true},
		}}, loose.layout)

		stream, size, err := store.OpenStream(t.Context(), hash)
		require.NoError(err)
		assert.Equal(int64(len(content)), size)
		require.ErrorIs(stream.Verify(), ErrContentMismatch)
		require.ErrorIs(stream.Close(), ErrContentMismatch)
		assert.False(stream.Verified())
		assert.Equal(before, mustReadFile(t, loose.layout.CompressedLoosePath(hash)))
	})
}

func TestLooseWriteDedupVerificationPolicies(t *testing.T) {
	require := require.New(t)
	content := []byte("right")
	hash := hashForTest(content)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	path := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, []byte("wrong"), 0o600))

	result, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyTypeAndSize,
		ExpectedHash: hash, ExpectedSize: int64(len(content)), SizeKnown: true,
	})
	require.NoError(err)
	assert.False(t, result.Created)
	stored, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, []byte("wrong"), stored, "structural dedup deliberately does not detect same-size bit rot")

	_, err = store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash,
		ExpectedHash: hash, ExpectedSize: int64(len(content)), SizeKnown: true,
	})
	require.ErrorIs(err, ErrContentMismatch)
}

func TestLooseRepairRawRestoresCorruptCanonicalContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("verified raw repair content")
	corrupt := []byte("corrupt raw replacement!!!!")
	require.Len(corrupt, len(content))
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	require.Equal(LooseEncodingRaw, created.Encoding)
	require.NoError(os.WriteFile(created.Path, corrupt, 0o600))

	_, err = store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.ErrorIs(err, ErrContentMismatch)
	assert.Equal(corrupt, mustReadFile(t, created.Path), "ordinary writes remain fail-closed")

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: created.Hash,
		Size: created.Size,
	}, RepairOptions{Durability: AtomicPublication})

	require.NoError(err)
	assert.Equal(created.Hash, result.Hash)
	assert.Equal(created.Size, result.Size)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(created.Path, result.Path)
	assert.Equal(created.Size, result.StoredSize)
	assert.True(result.Created)
	assert.Equal(content, readRepairedLoose(t, store, result))
	assert.NoFileExists(store.layout.CompressedLoosePath(result.Hash))
}

func TestLooseRepairCompressedRestoresCorruptCanonicalContent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("verified compressed repair content\n"), 1024)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})
	require.NoError(err)
	require.Equal(LooseEncodingZstd, created.Encoding)
	corrupt := []byte("corrupt compressed representation")
	require.NoError(os.WriteFile(created.Path, corrupt, 0o600))

	_, err = store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})
	require.ErrorIs(err, ErrContentMismatch)
	assert.Equal(corrupt, mustReadFile(t, created.Path), "ordinary writes must not become an implicit repair")

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: created.Hash,
		Size: created.Size,
	}, RepairOptions{
		Durability:  AtomicPublication,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.NoError(err)
	assert.Equal(LooseEncodingZstd, result.Encoding)
	assert.Equal(store.layout.CompressedLoosePath(result.Hash), result.Path)
	assert.Less(result.StoredSize, result.Size)
	assert.Equal(content, readRepairedLoose(t, store, result))
	assert.NoFileExists(store.layout.LoosePath(result.Hash))
}

func TestLooseRepairReconcilesDualCopiesToSelectedRepresentation(t *testing.T) {
	content := bytes.Repeat([]byte("dual copy repair content\n"), 1024)
	for _, tt := range []struct {
		name        string
		compression LooseCompressionOptions
		want        LooseEncoding
	}{
		{name: "raw selected", want: LooseEncodingRaw},
		{
			name:        "zstd selected",
			compression: LooseCompressionOptions{Enabled: true},
			want:        LooseEncodingZstd,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)
			created, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability:  AtomicPublication,
				Dedup:       VerifyFullHash,
				Compression: LooseCompressionOptions{Enabled: true},
			})
			require.NoError(err)
			require.Equal(LooseEncodingZstd, created.Encoding)
			rawPath := store.layout.LoosePath(created.Hash)
			require.NoError(os.WriteFile(rawPath, []byte("stale raw copy"), 0o600))
			require.NoError(os.WriteFile(created.Path, []byte("stale compressed copy"), 0o600))

			result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
				Hash: created.Hash,
				Size: created.Size,
			}, RepairOptions{
				Durability:  AtomicPublication,
				Compression: tt.compression,
			})

			require.NoError(err)
			assert.Equal(tt.want, result.Encoding)
			assert.Equal(content, readRepairedLoose(t, store, result))
			if tt.want == LooseEncodingRaw {
				assert.Equal(rawPath, result.Path)
				assert.NoFileExists(created.Path, "raw repair removes the zstd canonical representation")
			} else {
				assert.Equal(created.Path, result.Path)
				assert.NoFileExists(rawPath, "zstd repair removes the raw canonical representation")
			}
		})
	}
}

func TestLooseRepairMismatchPreservesAllCanonicalCopies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	expected := []byte("expected repair bytes")
	wrong := []byte("different repair byte")
	require.Len(wrong, len(expected))
	store := newLooseStoreForTest(t, StagingSameDirectory)
	hash := hashForTest(expected)
	rawPath := store.layout.LoosePath(hash)
	compressedPath := store.layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	rawBefore := []byte("existing raw evidence")
	compressedBefore := []byte("existing compressed evidence")
	require.NoError(os.WriteFile(rawPath, rawBefore, 0o600))
	require.NoError(os.WriteFile(compressedPath, compressedBefore, 0o600))

	_, err := store.Repair(t.Context(), bytes.NewReader(wrong), LooseIdentity{
		Hash: hash,
		Size: int64(len(expected)),
	}, RepairOptions{
		Durability:  AtomicPublication,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(err, ErrContentMismatch)
	assert.Equal(rawBefore, mustReadFile(t, rawPath))
	assert.Equal(compressedBefore, mustReadFile(t, compressedPath))
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
}

func TestLooseRepairPublicationFailurePreservesAllCanonicalCopies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("repair publication failure")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	hash := hashForTest(content)
	rawPath := store.layout.LoosePath(hash)
	compressedPath := store.layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	rawBefore := []byte("existing raw evidence")
	compressedBefore := []byte("existing compressed evidence")
	require.NoError(os.WriteFile(rawPath, rawBefore, 0o600))
	require.NoError(os.WriteFile(compressedPath, compressedBefore, 0o600))
	publishErr := errors.New("injected repair replacement failure")
	originalReplace := publishLooseRepairFile
	publishLooseRepairFile = func(string, string, fs.FileInfo) (looseRepairPublishResult, error) {
		return looseRepairPublishResult{}, publishErr
	}
	t.Cleanup(func() { publishLooseRepairFile = originalReplace })

	_, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: AtomicPublication})

	require.ErrorIs(err, publishErr)
	assert.Equal(rawBefore, mustReadFile(t, rawPath))
	assert.Equal(compressedBefore, mustReadFile(t, compressedPath))
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
}

func TestLooseRepairPublicationFailurePreservesLastVerifiedStagingCopy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("last verified repair staging copy")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	canonical := store.layout.LoosePath(hash)
	publishErr := errors.New("injected unrecoverable replacement failure")
	originalReplace := publishLooseRepairFile
	var staging string
	publishLooseRepairFile = func(gotStaging, final string, _ fs.FileInfo) (looseRepairPublishResult, error) {
		staging = gotStaging
		assert.Equal(canonical, final)
		return looseRepairPublishResult{KeepStaging: true}, publishErr
	}
	t.Cleanup(func() {
		publishLooseRepairFile = originalReplace
		if staging != "" {
			_ = os.Remove(staging)
		}
	})

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: AtomicPublication})

	require.ErrorIs(err, publishErr)
	assert.False(result.Created)
	require.NotEmpty(staging)
	assert.Equal(content, mustReadFile(t, staging))
	assert.NoFileExists(canonical)
}

func TestLooseRepairDurableBackupRestorationSyncsShardBeforeStagingCleanup(t *testing.T) {
	for _, tt := range []struct {
		name    string
		syncErr error
	}{
		{name: "sync succeeds"},
		{name: "sync failure remains visible", syncErr: errors.New("injected restored shard sync failure")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			content := []byte("replacement disappears during partial Windows repair")
			oldCanonical := []byte("restored old canonical backup")
			store := newLooseStoreForTest(t, StagingStoreDirectory)
			hash := hashForTest(content)
			canonical := store.layout.LoosePath(hash)
			backup := canonical + ".repair-backup"
			stagingDir := store.layout.LooseStagingDir(hash)
			require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
			require.NoError(os.WriteFile(canonical, oldCanonical, 0o600))
			publishErr := errors.New("injected partial replacement failure")
			originalFileSync := syncLooseFile
			originalPublish := publishLooseRepairFile
			originalShardSync := syncLooseRepairShard
			originalStagingSync := syncLooseStagingDir
			var events []string
			syncLooseFile = func(file *os.File) error {
				events = append(events, "file")
				return originalFileSync(file)
			}
			publishLooseRepairFile = func(staging, final string, _ fs.FileInfo) (looseRepairPublishResult, error) {
				events = append(events, "publish")
				require.Equal(canonical, final)
				require.NoError(os.Rename(final, backup))
				require.NoError(os.Remove(staging))
				require.NoError(os.Link(backup, final))
				require.NoError(os.Remove(backup))
				return looseRepairPublishResult{SyncShard: true}, publishErr
			}
			syncLooseRepairShard = func(path string) error {
				events = append(events, "shard")
				assert.Equal(filepath.Dir(canonical), path)
				assert.Equal(oldCanonical, mustReadFile(t, canonical))
				assert.NoFileExists(backup)
				return tt.syncErr
			}
			syncLooseStagingDir = func(path string) error {
				events = append(events, "staging")
				assert.Equal(stagingDir, path)
				assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
				return originalStagingSync(path)
			}
			t.Cleanup(func() {
				syncLooseFile = originalFileSync
				publishLooseRepairFile = originalPublish
				syncLooseRepairShard = originalShardSync
				syncLooseStagingDir = originalStagingSync
			})

			result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
				Hash: hash,
				Size: int64(len(content)),
			}, RepairOptions{Durability: DurablePublication})

			require.ErrorIs(err, publishErr)
			if tt.syncErr != nil {
				require.ErrorIs(err, tt.syncErr)
			}
			assert.False(result.Created)
			assert.Equal(oldCanonical, mustReadFile(t, canonical))
			assert.Equal([]string{"file", "publish", "shard", "staging"}, events)
		})
	}
}

func TestLooseRepairDurableKeepStagingSyncsPreservedEntryBeforeReturn(t *testing.T) {
	for _, tt := range []struct {
		name    string
		syncErr error
	}{
		{name: "sync succeeds"},
		{name: "sync failure remains visible", syncErr: errors.New("injected preserved staging sync failure")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			content := []byte("preserved durable repair staging")
			store := newLooseStoreForTest(t, StagingStoreDirectory)
			hash := hashForTest(content)
			canonical := store.layout.LoosePath(hash)
			stagingDir := store.layout.LooseStagingDir(hash)
			publishErr := errors.New("injected unrecoverable durable replacement failure")
			originalFileSync := syncLooseFile
			originalPublish := publishLooseRepairFile
			originalShardSync := syncLooseRepairShard
			originalStagingSync := syncLooseStagingDir
			var events []string
			var staging string
			syncLooseFile = func(file *os.File) error {
				events = append(events, "file")
				return originalFileSync(file)
			}
			publishLooseRepairFile = func(gotStaging, final string, _ fs.FileInfo) (looseRepairPublishResult, error) {
				events = append(events, "publish")
				staging = gotStaging
				assert.Equal(canonical, final)
				return looseRepairPublishResult{KeepStaging: true, SyncStaging: true}, publishErr
			}
			syncLooseRepairShard = func(string) error {
				assert.Fail("preserving an unchanged staging entry must not sync the shard")
				return nil
			}
			syncLooseStagingDir = func(path string) error {
				events = append(events, "staging")
				assert.Equal(stagingDir, path)
				require.NotEmpty(staging)
				assert.Equal(content, mustReadFile(t, staging))
				return tt.syncErr
			}
			t.Cleanup(func() {
				syncLooseFile = originalFileSync
				publishLooseRepairFile = originalPublish
				syncLooseRepairShard = originalShardSync
				syncLooseStagingDir = originalStagingSync
				if staging != "" {
					_ = os.Remove(staging)
				}
			})

			result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
				Hash: hash,
				Size: int64(len(content)),
			}, RepairOptions{Durability: DurablePublication})

			require.ErrorIs(err, publishErr)
			if tt.syncErr != nil {
				require.ErrorIs(err, tt.syncErr)
			}
			assert.False(result.Created)
			require.NotEmpty(staging)
			assert.Equal(content, mustReadFile(t, staging))
			assert.NoFileExists(canonical)
			assert.Equal([]string{"file", "publish", "staging"}, events)
		})
	}
}

func TestLooseRepairReplacementAPIErrorReturnsPublishedReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("replacement reached canonical despite API error")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	canonical := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
	require.NoError(os.WriteFile(canonical, []byte("old canonical evidence"), 0o600))
	publishErr := errors.New("injected partial ReplaceFileW failure")
	originalReplace := publishLooseRepairFile
	publishLooseRepairFile = func(staging, final string, _ fs.FileInfo) (looseRepairPublishResult, error) {
		require.Equal(canonical, final)
		require.NoError(os.Rename(staging, final))
		return looseRepairPublishResult{Created: true}, publishErr
	}
	t.Cleanup(func() { publishLooseRepairFile = originalReplace })

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: AtomicPublication})

	require.ErrorIs(err, publishErr)
	assert.True(result.Created)
	assert.Equal(canonical, result.Path)
	assert.Equal(content, mustReadFile(t, canonical))
}

func TestLooseRepairVerifiesSelectedStagingRepresentationBeforeReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("fully verify staged repair content\n"), 1024)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	hash := hashForTest(content)
	path := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	before := []byte("existing diagnostic evidence")
	require.NoError(os.WriteFile(path, before, 0o600))
	originalWriter := newLooseZstdWriter
	newLooseZstdWriter = func(dst io.Writer) (io.WriteCloser, error) {
		return &fixedSizeEncoder{dst: dst, payloadBytes: 1}, nil
	}
	t.Cleanup(func() { newLooseZstdWriter = originalWriter })

	_, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{
		Durability:  AtomicPublication,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(err, ErrContentMismatch)
	assert.Equal(before, mustReadFile(t, path), "unreadable staged zstd must not replace diagnostic evidence")
	assert.NoFileExists(store.layout.CompressedLoosePath(hash))
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
}

func TestLooseRepairRejectsSelectedPathSwapAfterVerification(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("verified repair staging identity")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	hash := hashForTest(content)
	canonical := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
	before := []byte("existing canonical evidence")
	require.NoError(os.WriteFile(canonical, before, 0o600))
	var displaced string
	originalAfterVerify := afterLooseRepairVerify
	afterLooseRepairVerify = func(path string) {
		displaced = path + ".verified"
		require.NoError(os.Rename(path, displaced))
		require.NoError(os.WriteFile(path, []byte("post-verification staging impostor"), 0o600))
	}
	t.Cleanup(func() {
		afterLooseRepairVerify = originalAfterVerify
		if displaced != "" {
			_ = os.Remove(displaced)
		}
	})

	_, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: AtomicPublication})

	require.ErrorIs(err, ErrContentMismatch)
	assert.Equal(before, mustReadFile(t, canonical))
	assert.NoFileExists(store.layout.CompressedLoosePath(hash))
}

func TestLooseRepairRejectsSameInodeMutationAfterVerification(t *testing.T) {
	content := []byte("verified repair staging content")
	for _, tt := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "overwrite",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("mutated repair staging content!"), 0o600))
			},
		},
		{
			name: "truncate",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Truncate(path, 5))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)
			hash := hashForTest(content)
			canonical := store.layout.LoosePath(hash)
			require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
			before := []byte("existing same-inode evidence")
			require.NoError(os.WriteFile(canonical, before, 0o600))
			originalAfterVerify := afterLooseRepairVerify
			afterLooseRepairVerify = func(path string) {
				identityBefore, err := os.Stat(path)
				require.NoError(err)
				tt.mutate(t, path)
				identityAfter, err := os.Stat(path)
				require.NoError(err)
				assert.True(os.SameFile(identityBefore, identityAfter), "fixture must mutate the verified inode in place")
			}
			t.Cleanup(func() { afterLooseRepairVerify = originalAfterVerify })

			_, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
				Hash: hash,
				Size: int64(len(content)),
			}, RepairOptions{Durability: AtomicPublication})

			require.ErrorIs(err, ErrContentMismatch)
			assert.Equal(before, mustReadFile(t, canonical))
			assert.NoFileExists(store.layout.CompressedLoosePath(hash))
		})
	}
}

func TestLooseRepairCancellationDuringStagingPreservesCanonicalEvidence(t *testing.T) {
	content := bytes.Repeat([]byte("cancel repair staging\n"), 4096)
	for _, staging := range []StagingMode{StagingSameDirectory, StagingStoreDirectory} {
		t.Run(stagingName(staging), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, staging)
			hash := hashForTest(content)
			canonical := store.layout.LoosePath(hash)
			require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
			before := []byte("existing staging-cancel evidence")
			require.NoError(os.WriteFile(canonical, before, 0o600))
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			_, err := store.Repair(ctx, &cancelAfterRead{reader: bytes.NewReader(content), cancel: cancel}, LooseIdentity{
				Hash: hash,
				Size: int64(len(content)),
			}, RepairOptions{Durability: AtomicPublication})

			require.ErrorIs(err, context.Canceled)
			assert.Equal(before, mustReadFile(t, canonical))
			assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
		})
	}
}

func TestLooseRepairCancellationDuringVerificationPreservesCanonicalEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("cancel repair verification\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	canonical := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
	before := []byte("existing verification-cancel evidence")
	require.NoError(os.WriteFile(canonical, before, 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	originalReader := newLooseZstdReader
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		reader, err := originalReader(src)
		if err != nil {
			return nil, err
		}
		return &cancelOnLooseRead{looseZstdReader: reader, cancel: cancel}, nil
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })

	_, err := store.Repair(ctx, bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{
		Durability:  AtomicPublication,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(err, context.Canceled)
	assert.Equal(before, mustReadFile(t, canonical))
	assert.NoFileExists(store.layout.CompressedLoosePath(hash))
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
}

func TestLooseRepairCancellationWhileWaitingForStripeSkipsVerification(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("cancel repair stripe wait\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	canonical := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(canonical), 0o700))
	before := []byte("existing stripe-cancel evidence")
	require.NoError(os.WriteFile(canonical, before, 0o600))
	releaseStripe, err := acquireLooseWriteStripe(t.Context(), hash)
	require.NoError(err)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(releaseStripe) }
	t.Cleanup(release)
	reachedStripe := make(chan struct{})
	originalBeforePublish := beforeLoosePublish
	beforeLoosePublish = func(gotHash Hash, _ LooseEncoding) {
		if gotHash == hash {
			close(reachedStripe)
		}
	}
	t.Cleanup(func() { beforeLoosePublish = originalBeforePublish })
	verificationStarted := false
	originalReader := newLooseZstdReader
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		verificationStarted = true
		return originalReader(src)
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	type repairOutcome struct {
		result WriteResult
		err    error
	}
	outcome := make(chan repairOutcome, 1)
	go func() {
		result, repairErr := store.Repair(ctx, bytes.NewReader(content), LooseIdentity{
			Hash: hash,
			Size: int64(len(content)),
		}, RepairOptions{
			Durability:  AtomicPublication,
			Compression: LooseCompressionOptions{Enabled: true},
		})
		outcome <- repairOutcome{result: result, err: repairErr}
	}()
	receiveLooseSignal(t, reachedStripe, "repair to queue for the loose publication stripe")
	cancel()

	var got repairOutcome
	select {
	case got = <-outcome:
	case <-time.After(time.Second):
		release()
		got = <-outcome
		assert.Fail("cancelled repair remained blocked on the loose publication stripe")
	}

	require.ErrorIs(got.err, context.Canceled)
	assert.False(got.result.Created)
	assert.False(verificationStarted, "repair verification must begin only after acquiring the hash stripe")
	assert.Equal(before, mustReadFile(t, canonical))
	assert.NoFileExists(store.layout.CompressedLoosePath(hash))
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
}

func TestLooseRepairDurablePublicationSyncsReplacementAndReconciliation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("durable repair content")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	rawPath := store.layout.LoosePath(hash)
	compressedPath := store.layout.CompressedLoosePath(hash)
	stagingDir := store.layout.LooseStagingDir(hash)
	shard := filepath.Dir(rawPath)
	require.NotEqual(filepath.Clean(stagingDir), filepath.Clean(shard))
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, []byte("corrupt raw"), 0o600))
	require.NoError(os.WriteFile(compressedPath, []byte("corrupt compressed"), 0o600))
	originalFileSync := syncLooseFile
	originalShardSync := syncLooseRepairShard
	originalStagingSync := syncLooseStagingDir
	var events []string
	syncLooseFile = func(file *os.File) error {
		events = append(events, "file")
		return originalFileSync(file)
	}
	syncLooseRepairShard = func(path string) error {
		assert.Equal(filepath.Clean(shard), filepath.Clean(path))
		assert.Equal(content, mustReadFile(t, rawPath))
		assert.NoFileExists(compressedPath)
		events = append(events, "shard")
		return originalShardSync(path)
	}
	syncLooseStagingDir = func(path string) error {
		assert.Equal(filepath.Clean(stagingDir), filepath.Clean(path))
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
		events = append(events, "staging")
		return originalStagingSync(path)
	}
	t.Cleanup(func() {
		syncLooseFile = originalFileSync
		syncLooseRepairShard = originalShardSync
		syncLooseStagingDir = originalStagingSync
	})

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: DurablePublication})

	require.NoError(err)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal([]string{"file", "shard", "staging"}, events)
}

func TestLooseRepairAlternateRemovalFailureReturnsPublishedReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("alternate cleanup receipt")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	rawPath := store.layout.LoosePath(hash)
	compressedPath := store.layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, []byte("old raw evidence"), 0o600))
	compressedBefore := []byte("old compressed evidence")
	require.NoError(os.WriteFile(compressedPath, compressedBefore, 0o600))
	removeErr := errors.New("injected alternate removal failure")
	originalRemove := removeLooseAlternateFile
	removeLooseAlternateFile = func(path string) error {
		assert.Equal(compressedPath, path)
		return removeErr
	}
	t.Cleanup(func() { removeLooseAlternateFile = originalRemove })

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: AtomicPublication})

	require.ErrorIs(err, removeErr)
	assert.Equal(hash, result.Hash)
	assert.Equal(int64(len(content)), result.Size)
	assert.Equal(rawPath, result.Path)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(int64(len(content)), result.StoredSize)
	assert.True(result.Created)
	assert.Equal(content, mustReadFile(t, rawPath))
	assert.Equal(compressedBefore, mustReadFile(t, compressedPath))
}

func TestLooseRepairShardSyncFailureReturnsPublishedReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("shard durability receipt")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	rawPath := store.layout.LoosePath(hash)
	compressedPath := store.layout.CompressedLoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, []byte("old raw evidence"), 0o600))
	require.NoError(os.WriteFile(compressedPath, []byte("old compressed evidence"), 0o600))
	syncErr := errors.New("injected repaired shard sync failure")
	originalSync := syncLooseRepairShard
	syncLooseRepairShard = func(path string) error {
		assert.Equal(filepath.Dir(rawPath), path)
		return syncErr
	}
	t.Cleanup(func() { syncLooseRepairShard = originalSync })

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: DurablePublication})

	require.ErrorIs(err, syncErr)
	assert.Equal(hash, result.Hash)
	assert.Equal(int64(len(content)), result.Size)
	assert.Equal(rawPath, result.Path)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(int64(len(content)), result.StoredSize)
	assert.True(result.Created)
	assert.Equal(content, mustReadFile(t, rawPath))
	assert.NoFileExists(compressedPath)
}

func TestLooseRepairStagingSyncFailureReturnsPublishedReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("staging durability receipt")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	rawPath := store.layout.LoosePath(hash)
	compressedPath := store.layout.CompressedLoosePath(hash)
	stagingDir := store.layout.LooseStagingDir(hash)
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	require.NoError(os.WriteFile(rawPath, []byte("old raw evidence"), 0o600))
	require.NoError(os.WriteFile(compressedPath, []byte("old compressed evidence"), 0o600))
	syncErr := errors.New("injected repair staging sync failure")
	originalSync := syncLooseStagingDir
	syncLooseStagingDir = func(path string) error {
		assert.Equal(stagingDir, path)
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
		return syncErr
	}
	t.Cleanup(func() { syncLooseStagingDir = originalSync })

	result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
		Hash: hash,
		Size: int64(len(content)),
	}, RepairOptions{Durability: DurablePublication})

	require.ErrorIs(err, syncErr)
	assert.Equal(hash, result.Hash)
	assert.Equal(int64(len(content)), result.Size)
	assert.Equal(rawPath, result.Path)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(int64(len(content)), result.StoredSize)
	assert.True(result.Created)
	assert.Equal(content, mustReadFile(t, rawPath))
	assert.NoFileExists(compressedPath)
	assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
}

func TestLooseRepairKeepsActiveReadersStableAcrossRepresentations(t *testing.T) {
	content := bytes.Repeat([]byte("stable repaired representation content\n"), 1024)
	for _, tt := range []struct {
		name            string
		initialEncoding LooseEncoding
		repairEncoding  LooseEncoding
	}{
		{name: "raw to raw", initialEncoding: LooseEncodingRaw, repairEncoding: LooseEncodingRaw},
		{name: "zstd to zstd", initialEncoding: LooseEncodingZstd, repairEncoding: LooseEncodingZstd},
		{name: "raw to zstd", initialEncoding: LooseEncodingRaw, repairEncoding: LooseEncodingZstd},
		{name: "zstd to raw", initialEncoding: LooseEncodingZstd, repairEncoding: LooseEncodingRaw},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingStoreDirectory)
			writeCompression := LooseCompressionOptions{Enabled: tt.initialEncoding == LooseEncodingZstd}
			created, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability:  AtomicPublication,
				Dedup:       VerifyFullHash,
				Compression: writeCompression,
			})
			require.NoError(err)
			require.Equal(tt.initialEncoding, created.Encoding)
			oldPhysical := []byte("old open physical bytes for " + tt.name)
			require.NoError(os.WriteFile(created.Path, oldPhysical, 0o600))
			active, err := openNoFollow(created.Path, false)
			require.NoError(err)
			t.Cleanup(func() { _ = active.Close() })

			result, err := store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
				Hash: created.Hash,
				Size: created.Size,
			}, RepairOptions{
				Durability: AtomicPublication,
				Compression: LooseCompressionOptions{
					Enabled: tt.repairEncoding == LooseEncodingZstd,
				},
			})
			require.NoError(err)

			oldBytes, err := io.ReadAll(active)
			require.NoError(err)
			assert.Equal(oldPhysical, oldBytes, "the active reader stays pinned to its old physical identity")
			assert.Equal(tt.repairEncoding, result.Encoding)
			assert.Equal(content, readRepairedLoose(t, store, result), "new readers see the repaired logical content")
			if tt.repairEncoding == LooseEncodingRaw {
				assert.Equal(store.layout.LoosePath(result.Hash), result.Path)
				assert.NoFileExists(store.layout.CompressedLoosePath(result.Hash))
			} else {
				assert.Equal(store.layout.CompressedLoosePath(result.Hash), result.Path)
				assert.NoFileExists(store.layout.LoosePath(result.Hash))
			}
		})
	}
}

func TestLooseRepairValidatesRequiredIdentityAndPolicyBeforeReading(t *testing.T) {
	content := []byte("repair validation content")
	hash := hashForTest(content)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	for _, tt := range []struct {
		name     string
		expected LooseIdentity
		opts     RepairOptions
		wantErr  error
	}{
		{
			name:     "missing hash",
			expected: LooseIdentity{Size: int64(len(content))},
			opts:     RepairOptions{Durability: AtomicPublication},
			wantErr:  ErrInvalidHash,
		},
		{
			name:     "negative size",
			expected: LooseIdentity{Hash: hash, Size: -1},
			opts:     RepairOptions{Durability: AtomicPublication},
			wantErr:  ErrInvalidPolicy,
		},
		{
			name:     "missing durability",
			expected: LooseIdentity{Hash: hash, Size: int64(len(content))},
			wantErr:  ErrInvalidPolicy,
		},
		{
			name:     "invalid compression",
			expected: LooseIdentity{Hash: hash, Size: int64(len(content))},
			opts: RepairOptions{
				Durability:  AtomicPublication,
				Compression: LooseCompressionOptions{Enabled: true, MinSavingsPercent: 101},
			},
			wantErr: ErrInvalidPolicy,
		},
		{
			name:     "expected size exceeds limit",
			expected: LooseIdentity{Hash: hash, Size: int64(len(content))},
			opts:     RepairOptions{Durability: AtomicPublication, MaxBytes: int64(len(content) - 1)},
			wantErr:  ErrContentMismatch,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := &countingLooseReader{reader: bytes.NewReader(content)}

			_, err := store.Repair(t.Context(), reader, tt.expected, tt.opts)

			require.ErrorIs(t, err, tt.wantErr)
			assert.Zero(t, reader.reads, "invalid repair input is rejected before consuming replacement bytes")
		})
	}
}

func TestLooseVerifyChecksCanonicalObject(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("verify existing object")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)

	result, exists, err := store.Verify(created.Hash, created.Size, VerifyFullHash, AtomicPublication)
	require.NoError(err)
	assert.True(exists)
	assert.Equal(created.Hash, result.Hash)

	missing := hashForTest([]byte("missing"))
	_, exists, err = store.Verify(missing, 0, VerifyFullHash, AtomicPublication)
	require.NoError(err)
	assert.False(exists)
	_, _, err = store.Verify("", 0, VerifyFullHash, AtomicPublication)
	require.ErrorIs(err, ErrInvalidHash)
}

func TestLooseVerifyRejectsSameInodeGrowthAfterFullHash(t *testing.T) {
	content := bytes.Repeat([]byte("same inode verification growth\n"), 256)
	for _, tt := range []struct {
		name        string
		compression LooseCompressionOptions
		want        LooseEncoding
	}{
		{name: "raw", want: LooseEncodingRaw},
		{
			name:        "compressed",
			compression: LooseCompressionOptions{Enabled: true},
			want:        LooseEncodingZstd,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)
			created, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability:  AtomicPublication,
				Dedup:       VerifyFullHash,
				Compression: tt.compression,
			})
			require.NoError(err)
			require.Equal(tt.want, created.Encoding)
			before, err := os.Stat(created.Path)
			require.NoError(err)

			if tt.want == LooseEncodingZstd {
				originalReader := newLooseZstdReader
				var mutated bool
				newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
					if !mutated {
						mutated = true
						appendLooseTestMutation(t, created.Path)
					}
					return originalReader(src)
				}
				t.Cleanup(func() { newLooseZstdReader = originalReader })
			} else {
				originalSnapshot := snapshotLoosePathIdentity
				var snapshots int
				snapshotLoosePathIdentity = func(path string) (fs.FileInfo, error) {
					if filepath.Clean(path) == filepath.Clean(created.Path) {
						snapshots++
						if snapshots == 2 {
							appendLooseTestMutation(t, path)
						}
					}
					return originalSnapshot(path)
				}
				t.Cleanup(func() { snapshotLoosePathIdentity = originalSnapshot })
			}

			result, exists, err := store.Verify(created.Hash, created.Size, VerifyFullHash, DurablePublication)

			require.ErrorIs(err, errIdentityChanged)
			assert.False(exists)
			assert.False(result.Created)
			after, statErr := os.Stat(created.Path)
			require.NoError(statErr)
			assert.True(os.SameFile(before, after), "fixture must grow the verified inode in place")
			assert.Greater(after.Size(), before.Size())
		})
	}
}

func TestLooseWriteBytesRejectsCompressedSameInodeGrowthDuringDedup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("compressed dedup growth\n"), 256)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	opts := WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	}
	created, err := store.WriteBytes(t.Context(), content, opts)
	require.NoError(err)
	require.Equal(LooseEncodingZstd, created.Encoding)

	originalReader := newLooseZstdReader
	var mutated bool
	newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
		if !mutated {
			mutated = true
			appendLooseTestMutation(t, created.Path)
		}
		return originalReader(src)
	}
	t.Cleanup(func() { newLooseZstdReader = originalReader })

	result, err := store.WriteBytes(t.Context(), content, opts)

	require.ErrorIs(err, ErrContentMismatch)
	assert.False(result.Created, "a raced deduplication must not report existing content as valid")
	assert.Equal(created.Hash, result.Hash)
}

func appendLooseTestMutation(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = file.WriteString("trailing mutation")
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

func TestLooseDurableVerifyRejectsIdentitySwap(t *testing.T) {
	require := require.New(t)
	content := []byte("durable identity must remain stable")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	displaced := created.Path + ".displaced"
	originalSnapshot := snapshotLoosePathIdentity
	var snapshots int
	snapshotLoosePathIdentity = func(path string) (fs.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(created.Path) {
			snapshots++
			if snapshots == 2 {
				require.NoError(os.Rename(created.Path, displaced))
				require.NoError(os.WriteFile(created.Path, content, 0o600))
			}
		}
		return originalSnapshot(path)
	}
	t.Cleanup(func() { snapshotLoosePathIdentity = originalSnapshot })

	_, _, err = store.Verify(created.Hash, created.Size, VerifyFullHash, DurablePublication)
	require.ErrorIs(err, errIdentityChanged)
	assert.FileExists(t, created.Path)
	assert.FileExists(t, displaced)
}

func TestLooseWriteFullHashRejectsChangedFileWithRestoredTimestamp(t *testing.T) {
	require := require.New(t)
	content := []byte("right")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	opts := WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash}
	result, err := store.WriteBytes(t.Context(), content, opts)
	require.NoError(err)
	_, err = store.WriteBytes(t.Context(), content, opts)
	require.NoError(err)

	before, err := os.Stat(result.Path)
	require.NoError(err)
	require.NoError(os.WriteFile(result.Path, []byte("wrong"), 0o600))
	require.NoError(os.Chtimes(result.Path, before.ModTime(), before.ModTime()))
	_, err = store.WriteBytes(t.Context(), content, opts)
	require.ErrorIs(err, ErrContentMismatch)
}

func TestLooseWriteFullHashHonorsCancellationWhileVerifyingExisting(t *testing.T) {
	content := bytes.Repeat([]byte("cancel existing verification\n"), 4096)
	tests := []struct {
		name        string
		compression LooseCompressionOptions
		maxRead     int64
		write       func(context.Context, *LooseStore, []byte, WriteOptions) error
	}{
		{
			name:    "stream raw",
			maxRead: looseCopyBufferBytes,
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.Write(ctx, bytes.NewReader(content), opts)
				return err
			},
		},
		{
			name:        "stream compressed",
			compression: LooseCompressionOptions{Enabled: true},
			maxRead:     64 << 10,
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.Write(ctx, bytes.NewReader(content), opts)
				return err
			},
		},
		{
			name:    "bytes raw",
			maxRead: looseCopyBufferBytes,
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.WriteBytes(ctx, content, opts)
				return err
			},
		},
		{
			name:        "bytes compressed",
			compression: LooseCompressionOptions{Enabled: true},
			maxRead:     64 << 10,
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.WriteBytes(ctx, content, opts)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)
			opts := WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: hashForTest(content),
				ExpectedSize: int64(len(content)),
				SizeKnown:    true,
				Compression:  tt.compression,
			}
			created, err := store.WriteBytes(t.Context(), content, opts)
			require.NoError(err)
			if tt.compression.Enabled {
				require.Equal(LooseEncodingZstd, created.Encoding)
			} else {
				require.Equal(LooseEncodingRaw, created.Encoding)
			}

			ctx, cancel := context.WithCancel(t.Context())
			var readBytes int64
			if tt.compression.Enabled {
				originalReader := newLooseZstdReader
				newLooseZstdReader = func(src io.Reader) (looseZstdReader, error) {
					decoder, decodeErr := originalReader(src)
					if decodeErr != nil {
						return nil, decodeErr
					}
					return &cancelAfterFirstLooseZstdRead{
						looseZstdReader: decoder,
						reader: &cancelAfterFirstRead{
							reader: decoder, cancel: cancel, readBytes: &readBytes,
						},
					}, nil
				}
				t.Cleanup(func() { newLooseZstdReader = originalReader })
			} else {
				originalReader := newLooseHashReader
				newLooseHashReader = func(readerCtx context.Context, src io.Reader) io.Reader {
					return originalReader(readerCtx, &cancelAfterFirstRead{
						reader: src, cancel: cancel, readBytes: &readBytes,
					})
				}
				t.Cleanup(func() { newLooseHashReader = originalReader })
			}

			err = tt.write(ctx, store, content, opts)

			require.ErrorIs(err, context.Canceled)
			assert.Positive(readBytes)
			assert.LessOrEqual(readBytes, tt.maxRead)
			assert.FileExists(created.Path)
		})
	}
}

func TestLooseWriteFullHashHonorsCancellationDuringFinalIdentityCheck(t *testing.T) {
	content := bytes.Repeat([]byte("cancel final identity check\n"), 256)
	tests := []struct {
		name        string
		compression LooseCompressionOptions
		write       func(context.Context, *LooseStore, []byte, WriteOptions) error
	}{
		{
			name: "stream raw",
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.Write(ctx, bytes.NewReader(content), opts)
				return err
			},
		},
		{
			name:        "stream compressed",
			compression: LooseCompressionOptions{Enabled: true},
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.Write(ctx, bytes.NewReader(content), opts)
				return err
			},
		},
		{
			name: "bytes raw",
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.WriteBytes(ctx, content, opts)
				return err
			},
		},
		{
			name:        "bytes compressed",
			compression: LooseCompressionOptions{Enabled: true},
			write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) error {
				_, err := store.WriteBytes(ctx, content, opts)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)
			opts := WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: hashForTest(content),
				ExpectedSize: int64(len(content)),
				SizeKnown:    true,
				Compression:  tt.compression,
			}
			created, err := store.WriteBytes(t.Context(), content, opts)
			require.NoError(err)

			ctx, cancel := context.WithCancel(t.Context())
			originalSnapshot := snapshotLoosePathIdentity
			var snapshots int
			var canceledDuringFinalSnapshot bool
			snapshotLoosePathIdentity = func(path string) (fs.FileInfo, error) {
				info, snapshotErr := originalSnapshot(path)
				if snapshotErr == nil && filepath.Clean(path) == filepath.Clean(created.Path) {
					snapshots++
					if snapshots == 2 {
						cancel()
						canceledDuringFinalSnapshot = true
					}
				}
				return info, snapshotErr
			}
			t.Cleanup(func() { snapshotLoosePathIdentity = originalSnapshot })

			err = tt.write(ctx, store, content, opts)

			require.ErrorIs(err, context.Canceled)
			assert.True(canceledDuringFinalSnapshot)
			assert.FileExists(created.Path)
		})
	}
}

type cancelAfterFirstRead struct {
	reader    io.Reader
	cancel    context.CancelFunc
	readBytes *int64
	once      sync.Once
}

func (r *cancelAfterFirstRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	*r.readBytes += int64(n)
	if n > 0 {
		r.once.Do(r.cancel)
	}
	return n, err
}

type cancelAfterFirstLooseZstdRead struct {
	looseZstdReader
	reader *cancelAfterFirstRead
}

func (r *cancelAfterFirstLooseZstdRead) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func TestLooseWriteMaxIntLimitDoesNotOverflow(t *testing.T) {
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	content := []byte("max-int limit remains bounded by io.Copy's int64 result")
	result, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: math.MaxInt64,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), result.Size)
}

func TestLooseStoreDefersMissingRootCreationUntilWritePolicyIsKnown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := filepath.Join(t.TempDir(), "missing", "store")
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(err)
	store, err := NewLooseStore(layout)
	require.NoError(err)
	assert.NoDirExists(root)
	_, err = store.WriteBytes(t.Context(), []byte("durable root"), WriteOptions{
		Durability: DurablePublication, Dedup: VerifyFullHash,
	})
	require.NoError(err)
	assert.DirExists(root)
}

func TestLooseWriteConcurrentDedup(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("concurrent"), 1024)
	hash := hashForTest(content)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	opts := WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: hash}

	const writers = 8
	results := make(chan WriteResult, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			result, err := store.Write(t.Context(), bytes.NewReader(content), opts)
			results <- result
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(err)
	}
	for result := range results {
		assert.Equal(t, hash, result.Hash)
	}
	stored, err := os.ReadFile(store.layout.LoosePath(hash))
	require.NoError(err)
	assert.Equal(t, content, stored)
}

func TestLooseWriteConcurrentRawAndCompressedPublishOneRepresentation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("cross-representation publication\n"), 4096)
	hash := hashForTest(content)
	layout, err := NewLayout(t.TempDir(), LayoutOptions{Staging: StagingSameDirectory})
	require.NoError(err)
	rawStore, err := NewLooseStore(layout)
	require.NoError(err)
	compressedStore, err := NewLooseStore(layout)
	require.NoError(err)
	arrived := make(chan LooseEncoding, 2)
	release := make(chan struct{})
	originalBeforePublish := beforeLoosePublish
	beforeLoosePublish = func(gotHash Hash, encoding LooseEncoding) {
		if gotHash == hash {
			arrived <- encoding
			<-release
		}
	}
	t.Cleanup(func() { beforeLoosePublish = originalBeforePublish })

	type writeOutcome struct {
		result WriteResult
		err    error
	}
	outcomes := make(chan writeOutcome, 2)
	var writers sync.WaitGroup
	writers.Go(func() {
		result, err := rawStore.WriteBytes(t.Context(), content, WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
		})
		outcomes <- writeOutcome{result: result, err: err}
	})
	writers.Go(func() {
		result, err := compressedStore.WriteBytes(t.Context(), content, WriteOptions{
			Durability:  AtomicPublication,
			Dedup:       VerifyFullHash,
			Compression: LooseCompressionOptions{Enabled: true},
		})
		outcomes <- writeOutcome{result: result, err: err}
	})

	firstArrival := receiveLooseEncoding(t, arrived)
	secondArrival := receiveLooseEncoding(t, arrived)
	assert.ElementsMatch([]LooseEncoding{LooseEncodingRaw, LooseEncodingZstd}, []LooseEncoding{firstArrival, secondArrival})
	close(release)
	writers.Wait()
	close(outcomes)

	var results []WriteResult
	for outcome := range outcomes {
		require.NoError(outcome.err)
		results = append(results, outcome.result)
	}
	require.Len(results, 2)
	assert.Equal(results[0].Path, results[1].Path)
	assert.Equal(results[0].Encoding, results[1].Encoding)
	created := 0
	for _, result := range results {
		if result.Created {
			created++
		}
	}
	assert.Equal(1, created)
	canonicalFiles := 0
	for _, path := range []string{layout.LoosePath(hash), layout.CompressedLoosePath(hash)} {
		if _, err := os.Stat(path); err == nil {
			canonicalFiles++
		} else {
			require.ErrorIs(err, fs.ErrNotExist)
		}
	}
	assert.Equal(1, canonicalFiles)
}

func TestLooseWriteCancelledWhileQueuedForStripeReturnsWithoutDedupOrPublish(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("cancel queued publication\n"), 4096)
	hash := hashForTest(content)
	layout, err := NewLayout(t.TempDir(), LayoutOptions{Staging: StagingSameDirectory})
	require.NoError(err)
	holderStore, err := NewLooseStore(layout)
	require.NoError(err)
	queuedStore, err := NewLooseStore(layout)
	require.NoError(err)
	holderAcquired := make(chan struct{})
	holderRelease := make(chan struct{})
	queuedAtAcquire := make(chan struct{})
	var releaseOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(func() { close(holderRelease) }) }
	t.Cleanup(releaseHolder)
	originalBeforePublish := beforeLoosePublish
	originalAfterAcquire := afterLooseStripeAcquire
	beforeLoosePublish = func(gotHash Hash, encoding LooseEncoding) {
		if gotHash == hash && encoding == LooseEncodingZstd {
			close(queuedAtAcquire)
		}
	}
	afterLooseStripeAcquire = func(gotHash Hash, encoding LooseEncoding) {
		if gotHash == hash && encoding == LooseEncodingRaw {
			close(holderAcquired)
			<-holderRelease
		}
	}
	t.Cleanup(func() {
		beforeLoosePublish = originalBeforePublish
		afterLooseStripeAcquire = originalAfterAcquire
	})

	type writeOutcome struct {
		result WriteResult
		err    error
	}
	holderOutcome := make(chan writeOutcome, 1)
	go func() {
		result, err := holderStore.WriteBytes(t.Context(), content, WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
		})
		holderOutcome <- writeOutcome{result: result, err: err}
	}()
	receiveLooseSignal(t, holderAcquired, "holder to acquire loose publication stripe")

	queuedCtx, cancelQueued := context.WithCancel(t.Context())
	t.Cleanup(cancelQueued)
	queuedOutcome := make(chan writeOutcome, 1)
	go func() {
		result, err := queuedStore.WriteBytes(queuedCtx, content, WriteOptions{
			Durability:  AtomicPublication,
			Dedup:       VerifyFullHash,
			Compression: LooseCompressionOptions{Enabled: true},
		})
		queuedOutcome <- writeOutcome{result: result, err: err}
	}()
	receiveLooseSignal(t, queuedAtAcquire, "queued writer to reach loose publication stripe")
	cancelQueued()

	var queued writeOutcome
	timedOut := false
	select {
	case queued = <-queuedOutcome:
	case <-time.After(time.Second):
		timedOut = true
		releaseHolder()
		queued = <-queuedOutcome
	}
	assert.False(timedOut, "cancelled queued writer must not wait for the stripe holder")
	require.ErrorIs(queued.err, context.Canceled)
	assert.False(queued.result.Created)
	assert.NoFileExists(layout.LoosePath(hash), "holder has not been released to publish")
	assert.NoFileExists(layout.CompressedLoosePath(hash), "cancelled queued writer must not publish")

	releaseHolder()
	holder := <-holderOutcome
	require.NoError(holder.err)
	assert.True(holder.result.Created)
	assert.Equal(LooseEncodingRaw, holder.result.Encoding)
	assert.FileExists(holder.result.Path)
}

func TestLooseWriteRejectsSymlinkDestination(t *testing.T) {
	require := require.New(t)
	content := []byte("content")
	hash := hashForTest(content)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	path := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(os.WriteFile(target, content, 0o600))
	require.NoError(os.Symlink(target, path))

	_, err := store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: hash,
	})
	require.Error(err)
	info, statErr := os.Lstat(path)
	require.NoError(statErr)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestRemoveLooseUsesExplicitDurability(t *testing.T) {
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	result, err := store.Write(t.Context(), bytes.NewReader([]byte("remove")), WriteOptions{
		Durability: DurablePublication, Dedup: VerifyFullHash,
	})
	require.NoError(err)
	require.NoError(store.Remove(result.Hash, BestEffortRemoval))
	assert.NoFileExists(t, result.Path)
	require.NoError(store.Remove(result.Hash, DurableRemoval), "missing durable removal is idempotent")
	require.ErrorIs(store.Remove(result.Hash, 0), ErrInvalidPolicy)
}

func TestLooseRemoveRemovesRawAndCompressedRepresentationsDurably(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	content := bytes.Repeat([]byte("remove both representations\n"), 16)
	result, err := store.WriteBytes(t.Context(), content, WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})
	require.NoError(err)
	require.Equal(LooseEncodingZstd, result.Encoding)
	rawPath := store.layout.LoosePath(result.Hash)
	require.NoError(os.WriteFile(rawPath, content, 0o600))
	var synced []string
	originalSyncDir := pack.SyncDir
	pack.SyncDir = func(path string) error {
		synced = append(synced, path)
		return nil
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	require.NoError(store.Remove(result.Hash, DurableRemoval))

	assert.NoFileExists(rawPath)
	assert.NoFileExists(result.Path)
	assert.Equal([]string{filepath.Dir(rawPath)}, synced)
}

func TestLooseRemoveRejectsSymlinksAndPreservesUnknownExtensions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest([]byte("remove canonical names only"))
	rawPath := store.layout.LoosePath(hash)
	compressedPath := store.layout.CompressedLoosePath(hash)
	unknownPath := rawPath + ".bak"
	require.NoError(os.MkdirAll(filepath.Dir(rawPath), 0o700))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(os.WriteFile(target, []byte("target"), 0o600))
	require.NoError(os.Symlink(target, rawPath))
	require.NoError(os.WriteFile(compressedPath, []byte("canonical physical bytes"), 0o600))
	require.NoError(os.WriteFile(unknownPath, []byte("unknown extension"), 0o600))

	err := store.Remove(hash, BestEffortRemoval)

	require.ErrorIs(err, ErrContentMismatch)
	rawInfo, statErr := os.Lstat(rawPath)
	require.NoError(statErr)
	assert.NotZero(rawInfo.Mode() & os.ModeSymlink)
	assert.NoFileExists(compressedPath, "the independent canonical representation is still removed")
	assert.FileExists(unknownPath)
	assert.FileExists(target)
}

func BenchmarkLooseWriteBytesDuplicate(b *testing.B) {
	content := bytes.Repeat([]byte("duplicate loose content\n"), 4096)
	store := newLooseStoreForTest(b, StagingSameDirectory)
	opts := WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash}
	_, err := store.WriteBytes(b.Context(), content, opts)
	require.NoError(b, err)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := store.WriteBytes(b.Context(), content, opts)
		require.NoError(b, err)
	}
}

func newLooseStoreForTest(tb testing.TB, staging StagingMode) *LooseStore {
	tb.Helper()
	opts := LayoutOptions{Staging: staging}
	if staging == StagingStoreDirectory {
		opts.StagingDir = "tmp"
	}
	layout, err := NewLayout(tb.TempDir(), opts)
	require.NoError(tb, err)
	store, err := NewLooseStore(layout)
	require.NoError(tb, err)
	return store
}

func hashForTest(content []byte) Hash {
	sum := sha256.Sum256(content)
	hash, err := ParseHash(string(bytesToHex(sum[:])))
	if err != nil {
		panic(err)
	}
	return hash
}

func bytesToHex(data []byte) []byte {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(data)*2)
	for i, value := range data {
		encoded[i*2] = digits[value>>4]
		encoded[i*2+1] = digits[value&0x0f]
	}
	return encoded
}

func deterministicLooseNoise(size int) []byte {
	content := make([]byte, size)
	state := uint64(0x9e3779b97f4a7c15)
	for i := range content {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		content[i] = byte(state)
	}
	return content
}

func matchingFiles(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern+"*"))
	require.NoError(t, err)
	return matches
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}

func readRepairedLoose(t *testing.T, loose *LooseStore, result WriteResult) []byte {
	t.Helper()
	require := require.New(t)
	t.Helper()
	resolver := &repairResolver{hash: result.Hash}
	store, err := NewStore(resolver, loose.layout, StoreOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	content, size, err := store.ReadBounded(t.Context(), result.Hash, result.Size)
	require.NoError(err)
	assert.Equal(t, result.Size, size)
	return content
}

func assertNoLooseWriteResidue(t *testing.T, store *LooseStore, hash Hash) {
	t.Helper()
	assert.NoFileExists(t, store.layout.LoosePath(hash))
	assert.NoFileExists(t, store.layout.CompressedLoosePath(hash))
	assert.Empty(t, matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
}

func stagingName(staging StagingMode) string {
	if staging == StagingSameDirectory {
		return "same directory"
	}
	return "store directory"
}

func receiveLooseEncoding(t *testing.T, values <-chan LooseEncoding) LooseEncoding {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for loose writer to reach publication barrier")
		return 0
	}
}

func receiveLooseSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for "+description)
	}
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
	done   bool
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	if len(p) > 1024 {
		p = p[:1024]
	}
	n, err := r.reader.Read(p)
	if n > 0 && !r.done {
		r.done = true
		r.cancel()
	}
	return n, err
}

type cancelOnWriteCloser struct {
	io.WriteCloser
	cancel context.CancelFunc
	done   bool
}

type cancelOnLooseRead struct {
	looseZstdReader
	cancel context.CancelFunc
	done   bool
}

func (r *cancelOnLooseRead) Read(p []byte) (int, error) {
	n, err := r.looseZstdReader.Read(p)
	if n > 0 && !r.done {
		r.done = true
		r.cancel()
	}
	return n, err
}

func (w *cancelOnWriteCloser) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	if !w.done {
		w.done = true
		w.cancel()
	}
	return n, err
}

type errorWriteCloser struct {
	err error
}

func (w *errorWriteCloser) Write([]byte) (int, error) { return 0, w.err }
func (w *errorWriteCloser) Close() error              { return nil }

type errorReader struct {
	err error
}

func (r *errorReader) Read([]byte) (int, error) { return 0, r.err }

type fixedSizeEncoder struct {
	dst          io.Writer
	payloadBytes int
}

func (w *fixedSizeEncoder) Write(p []byte) (int, error) { return len(p), nil }

func (w *fixedSizeEncoder) Close() error {
	_, err := w.dst.Write(make([]byte, w.payloadBytes))
	return err
}

type maxDecodedReader struct {
	reader            looseZstdReader
	remaining         int64
	overread          error
	overreadAttempted *bool
}

func (r *maxDecodedReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		*r.overreadAttempted = true
		return 0, r.overread
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *maxDecodedReader) Close() { r.reader.Close() }

type boundedChunkReader struct {
	r   io.Reader
	max int
}

type countingLooseReader struct {
	reader io.Reader
	reads  int
}

func (r *countingLooseReader) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

type repairResolver struct {
	hash Hash
}

func (r *repairResolver) Resolve(_ context.Context, hash Hash) (Location, error) {
	return Location{Member: hash == r.hash}, nil
}

func (r *boundedChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		return 0, errors.New("write attempted an unbounded source read")
	}
	return r.r.Read(p)
}
