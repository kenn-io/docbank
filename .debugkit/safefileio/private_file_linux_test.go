package safefileio

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestLinuxFilesystemHasExternalAccessPolicy(t *testing.T) {
	aafs := uint32(unix.AAFS_MAGIC)
	tests := []struct {
		name           string
		remote         bool
		filesystemType int64
		want           bool
	}{
		{"remote file system", true, unix.EXT4_SUPER_MAGIC, true},
		{"AppArmor securityfs", false, int64(aafs), true},
		{"AppArmor securityfs, 32-bit f_type", false, int64(int32(aafs)), true},
		{"local ext4", false, unix.EXT4_SUPER_MAGIC, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, linuxFilesystemHasExternalAccessPolicy(tt.remote, tt.filesystemType))
		})
	}
}
