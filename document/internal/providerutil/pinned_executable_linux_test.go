package providerutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPinnedExecutableConcurrentLaunch(t *testing.T) {
	if os.Args[len(os.Args)-1] == "pinned-executable-helper" {
		return
	}
	source, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(source)
	require.NoError(t, err)
	digest := sha256.Sum256(content)
	pinned, err := LoadPinnedExecutable(source, hex.EncodeToString(digest[:]), int64(len(content)))
	require.NoError(t, err)
	for worker := range 8 {
		t.Run(fmt.Sprint(worker), func(t *testing.T) {
			t.Parallel()
			for range 32 {
				path, cleanup, err := pinned.Materialize()
				require.NoError(t, err)
				out, runErr := exec.CommandContext(t.Context(), path, "-test.run=^TestPinnedExecutableConcurrentLaunch$", "--", "pinned-executable-helper").CombinedOutput()
				require.NoError(t, cleanup())
				require.NoError(t, runErr, string(out))
			}
		})
	}
}
