//go:build !windows

package main

import (
	"errors"
	"io"
	"syscall"
)

func isClosedOutputPipe(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE)
}
