//go:build unix

package packstore

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const sourcePinLimitChild = "KIT_PACKSTORE_SOURCE_PIN_LIMIT_CHILD"

func TestPackSourcePinLimitHonorsUnixSoftLimit(t *testing.T) {
	if os.Getenv(sourcePinLimitChild) == "1" {
		runPackSourcePinLimitChild(t)
		return
	}
	command := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestPackSourcePinLimitHonorsUnixSoftLimit$")
	command.Env = append(os.Environ(), sourcePinLimitChild+"=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func runPackSourcePinLimitChild(t *testing.T) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	t.Helper()
	var processLimit unix.Rlimit
	require.NoError(unix.Getrlimit(unix.RLIMIT_NOFILE, &processLimit))
	if processLimit.Cur < 160 {
		t.Skip("process soft file limit is already below the controlled fixture")
	}
	processLimit.Cur = 160
	require.NoError(unix.Setrlimit(unix.RLIMIT_NOFILE, &processLimit))

	layout := layoutForStoreTest(t)
	catalog := newMaintenanceCatalog()
	var order []Hash
	for index := range 40 {
		content := fmt.Appendf(nil, "soft-limit source %02d", index)
		hash := writeMaintenanceLoose(t, layout, content)
		catalog.addLoose(hash, layout.LoosePath(hash))
		order = append(order, hash)
	}
	catalog.setCandidateOrder(order)
	maintainer := newMaintainerForTest(t, catalog, layout, DefaultLimits())
	assert.Equal(16, maintainer.packedSourcePinLimit)

	stats, err := maintainer.Pack(t.Context(), PackOptions{})
	require.NoError(err)
	assert.Equal(40, stats.BlobsPacked)
	assert.Equal(3, stats.PacksSealed)
}
