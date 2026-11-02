//go:build darwin || dragonfly || freebsd || openbsd

package fsname

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestBSDRemote(t *testing.T) {
	typeName := func(name string) []byte {
		var b [16]byte
		copy(b[:], name)
		return b[:]
	}
	tests := []struct {
		name     string
		flags    uint64
		typeName []byte
		want     bool
	}{
		{"local apfs", unix.MNT_LOCAL, typeName("apfs"), false},
		{"local ufs", unix.MNT_LOCAL, typeName("ufs"), false},
		{"local name filling the array", unix.MNT_LOCAL, []byte("abcdefghijklmnop"), false},
		{"nfs without MNT_LOCAL", 0, typeName("nfs"), true},
		{"smbfs without MNT_LOCAL", 0, typeName("smbfs"), true},
		{"macfuse marked local", unix.MNT_LOCAL, typeName("macfuse"), true},
		{"osxfuse marked local", unix.MNT_LOCAL, typeName("osxfuse"), true},
		{"fusefs marked local", unix.MNT_LOCAL, typeName("fusefs"), true},
		{"fusefs subtype marked local", unix.MNT_LOCAL, typeName("fusefs.sshfs"), true},
		{"fuse marked local", unix.MNT_LOCAL, typeName("fuse"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, bsdRemote(tt.flags, tt.typeName))
		})
	}
}
