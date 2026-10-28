package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWatchFiletimeBirthConversion(t *testing.T) {
	const epochTicks uint64 = 116444736000000000
	const maxTicks uint64 = 2650467743999999999
	for _, ticks := range []uint64{0, 1, epochTicks - 1, epochTicks, maxTicks + 1, ^uint64(0)} {
		assert.False(t, watchBirthFromFiletimeTicks(ticks).present, "ticks=%d", ticks)
	}
	assert.Equal(t, watchBirthStamp{seconds: 1, present: true},
		watchBirthFromFiletimeTicks(epochTicks+10000000))
	assert.Equal(t, watchBirthStamp{seconds: 1, nanoseconds: 100, present: true},
		watchBirthFromFiletimeTicks(epochTicks+10000001))
	assert.Equal(t, watchBirthStamp{seconds: 253402300799, nanoseconds: 999999900, present: true},
		watchBirthFromFiletimeTicks(maxTicks))
}
