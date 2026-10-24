//go:build windows

package main

import (
	"errors"
	"io"
	"syscall"

	"golang.org/x/sys/windows"
)

func isClosedOutputPipe(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}
