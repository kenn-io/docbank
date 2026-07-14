//go:build windows

package client

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessTerminationTreatsMissingProcessAsStopped(t *testing.T) {
	require.NoError(t, requestProcessStop(math.MaxInt32))
	require.NoError(t, forceTerminateProcess(math.MaxInt32))
}
