//go:build linux

package packstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLooseWriteFullHashRejectsEqualSizeReplacementAfterVerification(t *testing.T) {
	content := bytes.Repeat([]byte("verified existing content\n"), 256)
	for _, encoding := range []struct {
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
		for _, api := range []struct {
			name  string
			write func(context.Context, *LooseStore, []byte, WriteOptions) (WriteResult, error)
		}{
			{
				name: "Write",
				write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) (WriteResult, error) {
					return store.Write(ctx, bytes.NewReader(content), opts)
				},
			},
			{
				name: "WriteBytes",
				write: func(ctx context.Context, store *LooseStore, content []byte, opts WriteOptions) (WriteResult, error) {
					return store.WriteBytes(ctx, content, opts)
				},
			},
		} {
			t.Run(encoding.name+"/"+api.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				store := newLooseStoreForTest(t, StagingSameDirectory)
				opts := WriteOptions{
					Durability:   AtomicPublication,
					Dedup:        VerifyFullHash,
					ExpectedHash: hashForTest(content),
					ExpectedSize: int64(len(content)),
					SizeKnown:    true,
					Compression:  encoding.compression,
				}
				created, err := store.WriteBytes(t.Context(), content, opts)
				require.NoError(err)
				require.Equal(encoding.want, created.Encoding)
				physical, err := os.ReadFile(created.Path)
				require.NoError(err)
				replacement := bytes.Repeat([]byte{0xa5}, len(physical))

				replacementOutcome := installEqualSizeReplacementAtFinalSnapshot(t, created.Path, replacement)

				result, err := api.write(t.Context(), store, content, opts)

				require.Error(err)
				assert.True(errors.Is(err, ErrContentMismatch) || errors.Is(err, errIdentityChanged), err)
				assert.False(result.Created)
				assert.True(replacementOutcome.installed)
				assert.True(replacementOutcome.pinLiveAtReplacement)
				assert.True(replacementOutcome.replacementIdentityChanged)
				assert.Equal(replacement, mustReadFile(t, created.Path))
			})
		}
	}
}

func TestLooseDurableTypeAndSizeRejectsEqualSizeReplacementAfterSync(t *testing.T) {
	content := bytes.Repeat([]byte("durably synced existing content\n"), 256)
	for _, encoding := range []struct {
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
		t.Run(encoding.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newLooseStoreForTest(t, StagingSameDirectory)
			createOpts := WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: hashForTest(content),
				ExpectedSize: int64(len(content)),
				SizeKnown:    true,
				Compression:  encoding.compression,
			}
			created, err := store.WriteBytes(t.Context(), content, createOpts)
			require.NoError(err)
			require.Equal(encoding.want, created.Encoding)
			physical, err := os.ReadFile(created.Path)
			require.NoError(err)
			replacement := bytes.Repeat([]byte{0x5a}, len(physical))
			replacementOutcome := installEqualSizeReplacementAtFinalSnapshot(t, created.Path, replacement)

			result, err := store.WriteBytes(t.Context(), content, WriteOptions{
				Durability:   DurablePublication,
				Dedup:        VerifyTypeAndSize,
				ExpectedHash: createOpts.ExpectedHash,
				ExpectedSize: createOpts.ExpectedSize,
				SizeKnown:    true,
				Compression:  encoding.compression,
			})

			require.Error(err)
			assert.True(errors.Is(err, ErrContentMismatch) || errors.Is(err, errIdentityChanged), err)
			assert.False(result.Created)
			assert.True(replacementOutcome.installed)
			assert.True(replacementOutcome.pinLiveAtReplacement)
			assert.True(replacementOutcome.replacementIdentityChanged)
			assert.Equal(replacement, mustReadFile(t, created.Path))
		})
	}
}

type equalSizeReplacementOutcome struct {
	installed                  bool
	pinLiveAtReplacement       bool
	replacementIdentityChanged bool
}

type observedLooseVerificationPin struct {
	looseVerificationIdentityPin
	live *bool
}

func (p *observedLooseVerificationPin) Close() error {
	err := p.looseVerificationIdentityPin.Close()
	*p.live = false
	return err
}

func installEqualSizeReplacementAtFinalSnapshot(t *testing.T, path string, replacement []byte) *equalSizeReplacementOutcome {
	t.Helper()
	originalSnapshot := snapshotLoosePathIdentity
	originalOpenPin := openLooseVerificationIdentityPin
	var snapshots int
	var originalIdentity fs.FileInfo
	var pinLive bool
	outcome := &equalSizeReplacementOutcome{}
	openLooseVerificationIdentityPin = func(gotPath string) (looseVerificationIdentityPin, fs.FileInfo, error) {
		pin, info, err := originalOpenPin(gotPath)
		if err != nil || filepath.Clean(gotPath) != filepath.Clean(path) {
			return pin, info, err
		}
		pinLive = true
		return &observedLooseVerificationPin{looseVerificationIdentityPin: pin, live: &pinLive}, info, nil
	}
	snapshotLoosePathIdentity = func(gotPath string) (fs.FileInfo, error) {
		info, snapshotErr := originalSnapshot(gotPath)
		if snapshotErr != nil || filepath.Clean(gotPath) != filepath.Clean(path) {
			return info, snapshotErr
		}
		snapshots++
		if snapshots == 1 {
			originalIdentity = info
		}
		if snapshots != 2 {
			return info, nil
		}
		outcome.installed = true
		require.NoError(t, os.Remove(gotPath))
		require.NoError(t, os.WriteFile(gotPath, replacement, 0o600))
		replacementInfo, snapshotErr := originalSnapshot(gotPath)
		require.NoError(t, snapshotErr)
		outcome.pinLiveAtReplacement = pinLive
		outcome.replacementIdentityChanged = !os.SameFile(originalIdentity, replacementInfo)
		if !pinLive {
			// Linux may recycle the released inode number before a pathname-only
			// recheck. Model that permitted ABA deterministically. A live pin keeps
			// the original inode allocated, so the real replacement identity must
			// be returned and rejected instead.
			return originalIdentity, nil
		}
		return replacementInfo, snapshotErr
	}
	t.Cleanup(func() {
		snapshotLoosePathIdentity = originalSnapshot
		openLooseVerificationIdentityPin = originalOpenPin
	})
	return outcome
}
