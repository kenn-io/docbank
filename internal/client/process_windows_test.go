//go:build windows

package client

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTerminateProcessTreatsMissingProcessAsStopped(t *testing.T) {
	require.NoError(t, terminateProcess(math.MaxInt32))
}
