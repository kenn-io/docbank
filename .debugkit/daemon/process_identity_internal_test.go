package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRuntimeRecordBlocksStartForeign(t *testing.T) {
	pid := startLivePIDHelper(t)
	assert.False(t, runtimeRecordBlocksStart(RuntimeRecord{PID: pid}, ProcessScopeForeign))
}
