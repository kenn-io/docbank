package packstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestLooseDurableWriteSyncsConcurrentDirectoryCreation(t *testing.T) {
	require := require.New(t)
	content := []byte("concurrent shard creation")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	shard := filepath.Dir(store.layout.LoosePath(hashForTest(content)))
	opts := WriteOptions{Durability: DurablePublication, Dedup: VerifyFullHash}
	creatorWaiting := make(chan struct{})
	resumeCreator := make(chan struct{})
	creatorDone := make(chan struct{})
	var creatorErr error
	var releaseOnce sync.Once
	releaseCreator := func() { releaseOnce.Do(func() { close(resumeCreator) }) }
	var startedCreator, blockedSync, rootSynced atomic.Bool
	originalLstat := lstatLooseDirectory
	originalSync := pack.SyncDir
	originalPublish := publishLooseFile
	t.Cleanup(func() {
		lstatLooseDirectory = originalLstat
		pack.SyncDir = originalSync
		publishLooseFile = originalPublish
	})
	t.Cleanup(func() {
		releaseCreator()
		if startedCreator.Load() {
			<-creatorDone
		}
	})
	lstatLooseDirectory = func(path string) (os.FileInfo, error) {
		info, err := originalLstat(path)
		if path == shard && os.IsNotExist(err) && startedCreator.CompareAndSwap(false, true) {
			// Let a second real writer create the missing shard, but pause
			// its parent sync before returning our original missing result.
			go func() {
				_, creatorErr = store.WriteBytes(t.Context(), content, opts)
				close(creatorDone)
			}()
			select {
			case <-creatorWaiting:
			case <-creatorDone:
				return nil, errors.Join(creatorErr, errors.New("concurrent creator returned before parent sync"))
			}
		}
		return info, err
	}
	pack.SyncDir = func(path string) error {
		if path == store.layout.Root() && blockedSync.CompareAndSwap(false, true) {
			close(creatorWaiting)
			<-resumeCreator
		}
		err := originalSync(path)
		if path == store.layout.Root() && err == nil {
			rootSynced.Store(true)
		}
		return err
	}
	publishLooseFile = func(src, dst string) error {
		assert.True(t, rootSynced.Load(), "publication must not depend on the other writer's pending parent sync")
		return originalPublish(src, dst)
	}

	result, err := store.WriteBytes(t.Context(), content, opts)
	require.True(startedCreator.Load(), "the concurrent directory creation must be exercised")
	releaseCreator()
	<-creatorDone
	require.NoError(creatorErr)
	require.NoError(err)
	assert.True(t, result.Created)
	stored, err := os.ReadFile(result.Path)
	require.NoError(err)
	assert.Equal(t, content, stored)
}

func TestLooseDurableWriteSharesStagingAndShardParentSync(t *testing.T) {
	content := []byte("shared parent durability")
	hash := hashForTest(content)
	for _, stagingDir := range []string{"tmp", hash.String()[:2]} {
		for _, operation := range []string{"write", "duplicate", "repair"} {
			t.Run(stagingDir+"/"+operation, func(t *testing.T) {
				require := require.New(t)
				layout, err := NewLayout(t.TempDir(), LayoutOptions{
					Staging: StagingStoreDirectory, StagingDir: stagingDir,
				})
				require.NoError(err)
				store, err := NewLooseStore(layout)
				require.NoError(err)
				// These entries may be residue from an interrupted write;
				// existence alone does not establish their durability.
				require.NoError(os.MkdirAll(layout.LooseStagingDir(hash), 0o700))
				require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
				if operation != "write" {
					stored := content
					if operation == "repair" {
						stored = []byte("damaged")
					}
					require.NoError(os.WriteFile(layout.LoosePath(hash), stored, 0o600))
				}
				originalSync := pack.SyncDir
				originalPublish := publishLooseFile
				originalRepair := publishLooseRepairFile
				var rootSyncs int
				pack.SyncDir = func(path string) error {
					if filepath.Clean(path) == layout.Root() {
						rootSyncs++
					}
					return originalSync(path)
				}
				publishLooseFile = func(src, dst string) error {
					assert.Positive(t, rootSyncs, "parent must be durable before publication")
					return originalPublish(src, dst)
				}
				publishLooseRepairFile = func(src, dst string, identity os.FileInfo) (looseRepairPublishResult, error) {
					assert.Positive(t, rootSyncs, "parent must be durable before repair recovery can need staging")
					return originalRepair(src, dst, identity)
				}
				t.Cleanup(func() {
					pack.SyncDir = originalSync
					publishLooseFile = originalPublish
					publishLooseRepairFile = originalRepair
				})

				var result WriteResult
				if operation == "repair" {
					result, err = store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
						Hash: hash, Size: int64(len(content)),
					}, RepairOptions{Durability: DurablePublication})
				} else {
					// No expected hash: duplicates must go through staging too.
					result, err = store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
						Durability: DurablePublication, Dedup: VerifyFullHash,
					})
				}
				require.NoError(err)
				assert.Equal(t, operation != "duplicate", result.Created)
				if operation == "duplicate" {
					assert.LessOrEqual(t, rootSyncs, 1, "staging must not add another parent sync to dedup verification")
				} else {
					assert.Equal(t, 1, rootSyncs, "one parent sync covers both pre-existing child directories")
				}
				stored, err := os.ReadFile(result.Path)
				require.NoError(err)
				assert.Equal(t, content, stored)
				assert.Empty(t, matchingFiles(t, layout.LooseStagingDir(hash), ".staging-"))
			})
		}
	}
}

func TestLooseDurableWriteRequiresExistingDirectoryParentSync(t *testing.T) {
	content := []byte("sync parent before publishing")
	hash := hashForTest(content)
	for _, opts := range []LayoutOptions{
		{Staging: StagingSameDirectory},
		{Staging: StagingStoreDirectory, StagingDir: "tmp"},
		{Staging: StagingStoreDirectory, StagingDir: hash.String()[:2]},
		{Staging: StagingStoreDirectory, StagingDir: "."},
	} {
		t.Run(stagingName(opts.Staging)+"/"+opts.StagingDir, func(t *testing.T) {
			require := require.New(t)
			layout, err := NewLayout(t.TempDir(), opts)
			require.NoError(err)
			store, err := NewLooseStore(layout)
			require.NoError(err)
			stagingDir := layout.LooseStagingDir(hash)
			require.NoError(os.MkdirAll(stagingDir, 0o700))
			require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
			parent := filepath.Dir(stagingDir)
			originalSync := pack.SyncDir
			syncErr := errors.New("injected parent sync failure")
			var parentSyncs int
			pack.SyncDir = func(path string) error {
				if filepath.Clean(path) == parent {
					parentSyncs++
					if parentSyncs == 1 {
						return syncErr
					}
				}
				return originalSync(path)
			}
			t.Cleanup(func() { pack.SyncDir = originalSync })
			writeOpts := WriteOptions{Durability: DurablePublication, Dedup: VerifyFullHash}

			result, err := store.WriteBytes(t.Context(), content, writeOpts)
			require.ErrorIs(err, syncErr)
			assert.False(t, result.Created)
			assert.NoFileExists(t, layout.LoosePath(hash))
			assert.Empty(t, matchingFiles(t, stagingDir, ".staging-"))

			result, err = store.WriteBytes(t.Context(), content, writeOpts)
			require.NoError(err)
			assert.True(t, result.Created)
			assert.Equal(t, 2, parentSyncs, "retry must establish parent durability again")
			stored, err := os.ReadFile(result.Path)
			require.NoError(err)
			assert.Equal(t, content, stored)
		})
	}
}

func TestLooseStreamDuplicateSyncsOnlyRetainedFile(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		for _, verification := range []DedupVerification{VerifyTypeAndSize, VerifyFullHash} {
			t.Run(fmt.Sprintf("compressed=%t/verification=%d", compressed, verification), func(t *testing.T) {
				store := newLooseStoreForTest(t, StagingStoreDirectory)
				content := bytes.Repeat([]byte("duplicate stream\n"), 1024)
				opts := WriteOptions{
					Durability:  DurablePublication,
					Dedup:       verification,
					Compression: LooseCompressionOptions{Enabled: compressed},
				}
				first, err := store.WriteBytes(t.Context(), content, opts)
				require.NoError(t, err)
				if compressed {
					require.Equal(t, LooseEncodingZstd, first.Encoding)
				} else {
					require.Equal(t, LooseEncodingRaw, first.Encoding)
				}
				before, err := os.ReadFile(first.Path)
				require.NoError(t, err)

				originalSync := syncLooseFile
				t.Cleanup(func() { syncLooseFile = originalSync })
				var retainedSyncs int
				var retainedErr error
				syncLooseFile = func(file *os.File) error {
					if file.Name() != first.Path {
						return errors.New("discarded staging file must not be synced")
					}
					retainedSyncs++
					if retainedErr != nil {
						return retainedErr
					}
					return originalSync(file)
				}
				reader := bytes.NewReader(content)
				duplicate, err := store.Write(t.Context(), reader, opts)
				require.NoError(t, err)
				assert.Zero(t, reader.Len())
				first.Created = false
				assert.Equal(t, first, duplicate)
				assert.Positive(t, retainedSyncs)
				after, err := os.ReadFile(first.Path)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				assert.Empty(t, matchingFiles(t, store.layout.LooseStagingDir(first.Hash), ".staging-"))

				retainedErr = errors.New("retained file sync failed")
				_, err = store.Write(t.Context(), bytes.NewReader(content), opts)
				require.ErrorIs(t, err, retainedErr)
				assert.Empty(t, matchingFiles(t, store.layout.LooseStagingDir(first.Hash), ".staging-"))
			})
		}
	}
}

func TestLooseStreamDuplicateSerializesWithRepair(t *testing.T) {
	for _, compression := range []struct{ before, after bool }{
		{false, false}, {false, true}, {true, false}, {true, true},
	} {
		for _, verification := range []DedupVerification{VerifyTypeAndSize, VerifyFullHash} {
			t.Run(fmt.Sprintf("compressed=%t->%t/verification=%d", compression.before, compression.after, verification), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store := newLooseStoreForTest(t, StagingStoreDirectory)
					content := bytes.Repeat([]byte("repaired duplicate stream\n"), 1024)
					// Wait must be able to observe a repair blocked on this stripe.
					index := looseHashLockIndex(hashForTest(content))
					originalStripe := looseWriteStripes[index]
					looseWriteStripes[index] = make(chan struct{}, 1)
					t.Cleanup(func() { looseWriteStripes[index] = originalStripe })
					opts := WriteOptions{
						Durability:  DurablePublication,
						Dedup:       verification,
						Compression: LooseCompressionOptions{Enabled: compression.before},
					}
					first, err := store.WriteBytes(t.Context(), content, opts)
					require.NoError(t, err)
					require.Equal(t, compression.before, first.Encoding == LooseEncodingZstd)
					before, err := snapshotPathIdentity(first.Path)
					require.NoError(t, err)

					verificationPaused := make(chan struct{})
					resumeVerification := make(chan struct{})
					releaseVerification := sync.OnceFunc(func() { close(resumeVerification) })
					defer releaseVerification()
					originalSync := pack.SyncDir
					t.Cleanup(func() { pack.SyncDir = originalSync })
					var paused atomic.Bool
					pack.SyncDir = func(path string) error {
						if path == store.layout.Root() && paused.CompareAndSwap(false, true) {
							close(verificationPaused)
							<-resumeVerification
						}
						return originalSync(path)
					}

					var duplicate, repaired WriteResult
					var writeErr, repairErr error
					writeDone := make(chan struct{})
					go func() {
						duplicate, writeErr = store.Write(t.Context(), bytes.NewReader(content), opts)
						close(writeDone)
					}()
					<-verificationPaused
					repairDone := make(chan struct{})
					go func() {
						repaired, repairErr = store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
							Hash: first.Hash,
							Size: first.Size,
						}, RepairOptions{
							Durability:  DurablePublication,
							Compression: LooseCompressionOptions{Enabled: compression.after},
						})
						close(repairDone)
					}()
					synctest.Wait()
					select {
					case <-repairDone:
						assert.Fail(t, "repair must wait for duplicate verification")
					default:
					}
					releaseVerification()
					<-writeDone
					<-repairDone
					require.NoError(t, writeErr)
					require.NoError(t, repairErr)
					first.Created = false
					assert.Equal(t, first, duplicate)
					require.True(t, repaired.Created)
					require.Equal(t, compression.after, repaired.Encoding == LooseEncodingZstd)
					after, err := snapshotPathIdentity(repaired.Path)
					require.NoError(t, err)
					assert.False(t, os.SameFile(before, after), "repair must replace the verified object")
					_, exists, err := store.Verify(first.Hash, first.Size, VerifyFullHash, DurablePublication)
					require.NoError(t, err)
					assert.True(t, exists)
					assert.Empty(t, matchingFiles(t, store.layout.LooseStagingDir(first.Hash), ".staging-"))
				})
			})
		}
	}
}
