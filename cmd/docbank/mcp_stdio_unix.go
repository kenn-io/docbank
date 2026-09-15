//go:build unix

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Inherited blocking descriptors are not registered with Go's poller. Duplicate
// them in nonblocking mode so Close interrupts pending I/O, while leaving the
// caller's handles open and restoring their original mode after serving.
func duplicateMCPStdio(file *os.File) (*os.File, func(), error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, nil, errors.New("accessing MCP stdio descriptor")
	}
	var descriptor, flags int
	controlErr := raw.Control(func(fd uintptr) {
		flags, err = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if err == nil {
			descriptor, err = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
		}
	})
	if controlErr != nil {
		return nil, nil, errors.New("accessing MCP stdio descriptor")
	}
	if err != nil {
		return nil, nil, errors.New("duplicating MCP stdio descriptor")
	}
	if err := unix.SetNonblock(descriptor, true); err != nil {
		_ = unix.Close(descriptor)
		return nil, nil, errors.New("enabling MCP stdio polling")
	}
	restore := func() {
		_ = raw.Control(func(fd uintptr) { _ = unix.SetNonblock(int(fd), flags&unix.O_NONBLOCK != 0) })
	}
	return os.NewFile(uintptr(descriptor), file.Name()), restore, nil
}
