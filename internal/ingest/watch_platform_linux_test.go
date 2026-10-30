//go:build linux

package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestWatchLinuxStatxMissingIdentityBits(t *testing.T) {
	base := unix.Statx_t{Mask: unix.STATX_MNT_ID | unix.STATX_INO | unix.STATX_BTIME, Ino: 42}
	base.Btime.Sec = 100
	assert.True(t, matchableWatchIdentity(watchIdentityFromStatx(7, base)))
	for _, bit := range []uint32{unix.STATX_INO, unix.STATX_BTIME} {
		stat := base
		stat.Mask &^= bit
		assert.False(t, matchableWatchIdentity(watchIdentityFromStatx(7, stat)))
	}
}
