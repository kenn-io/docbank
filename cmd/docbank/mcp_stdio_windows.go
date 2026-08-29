package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows os.File.Close cancels pending pipe I/O. Give the connection its own
// handles so cancellation preserves the caller's stdin and stdout handles.
func duplicateMCPStdio(file *os.File) (*os.File, func(), error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return nil, nil, errors.New("accessing MCP stdio handle")
	}
	var handle windows.Handle
	controlErr := raw.Control(func(fd uintptr) {
		process := windows.CurrentProcess()
		err = windows.DuplicateHandle(process, windows.Handle(fd), process, &handle,
			0, false, windows.DUPLICATE_SAME_ACCESS)
	})
	if controlErr != nil {
		return nil, nil, errors.New("accessing MCP stdio handle")
	}
	if err != nil {
		return nil, nil, errors.New("duplicating MCP stdio handle")
	}
	return os.NewFile(uintptr(handle), file.Name()), func() {}, nil
}
