package packstore

import (
	"bytes"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreLooseRepairsCorruptObjects(t *testing.T) {
	for _, compressible := range []bool{true, false} {
		content := bytes.Repeat([]byte("restored content\n"), 4096)
		encoding := LooseEncodingZstd
		name := "compressible"
		if !compressible {
			_, err := rand.NewChaCha8([32]byte{7}).Read(content)
			require.NoError(t, err)
			encoding = LooseEncodingRaw
			name = "incompressible"
		}
		hash := hashForTest(content)
		for _, existing := range []string{"raw", "zstd"} {
			t.Run(name+"/"+existing, func(t *testing.T) {
				root, err := os.OpenRoot(t.TempDir())
				require.NoError(t, err)
				defer func() { require.NoError(t, root.Close()) }()
				require.NoError(t, root.Mkdir(hash.String()[:2], 0o700))
				raw := filepath.Join(hash.String()[:2], hash.String())
				corrupt := raw
				if existing == "zstd" {
					corrupt += ".zst"
					// A failed restore must not remove another representation still
					// referenced by the database it was meant to replace.
					require.NoError(t, root.WriteFile(raw, content, 0o600))
				}
				require.NoError(t, root.WriteFile(corrupt, []byte("damaged"), 0o600))
				opts := WriteOptions{
					Durability: DurablePublication, Dedup: VerifyFullHash,
					ExpectedHash: hash, ExpectedSize: int64(len(content)), SizeKnown: true,
					Compression: LooseCompressionOptions{Enabled: true},
				}
				receipt, err := RestoreLoose(t.Context(), root, hash, bytes.NewReader(content), opts)
				require.NoError(t, err)
				assert.Equal(t, encoding, receipt.Encoding)
				layout, err := NewLayout(root.Name(), LayoutOptions{Staging: StagingSameDirectory})
				require.NoError(t, err)
				loose, err := NewLooseStore(layout)
				require.NoError(t, err)
				_, exists, err := loose.Verify(hash, int64(len(content)), VerifyFullHash, AtomicPublication)
				require.NoError(t, err)
				assert.True(t, exists)
				if encoding == LooseEncodingRaw {
					assert.NoFileExists(t, filepath.Join(root.Name(), raw+".zst"))
				} else if existing == "raw" {
					assert.NoFileExists(t, filepath.Join(root.Name(), raw))
				}
				if existing == "zstd" {
					retained, err := root.ReadFile(raw)
					require.NoError(t, err)
					assert.Equal(t, content, retained)
				}
			})
		}
	}
}
