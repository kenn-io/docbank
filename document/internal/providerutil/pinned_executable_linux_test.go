package providerutil_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/internal/providerutil"
)

func TestPinnedExecutableConcurrentLaunch(t *testing.T) {
	source, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(source)
	require.NoError(t, err)
	digest := sha256.Sum256(content)
	pinned, err := providerutil.LoadPinnedExecutable(source, hex.EncodeToString(digest[:]), int64(len(content)))
	require.NoError(t, err)
	for worker := range 8 {
		t.Run(strconv.Itoa(worker), func(t *testing.T) {
			t.Parallel()
			for range 32 {
				path, cleanup, err := pinned.Materialize()
				require.NoError(t, err)
				out, runErr := exec.CommandContext(t.Context(), path, "-test.run=^$").CombinedOutput()
				require.NoError(t, cleanup())
				require.NoError(t, runErr, string(out))
			}
		})
	}
}
