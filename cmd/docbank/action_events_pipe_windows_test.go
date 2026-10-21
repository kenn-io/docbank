//go:build windows

package main

import (
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestClosedOutputPipe(t *testing.T) {
	for _, cause := range []error{io.ErrClosedPipe, syscall.EPIPE, windows.ERROR_BROKEN_PIPE, windows.ERROR_NO_DATA} {
		assert.True(t, isClosedOutputPipe(fmt.Errorf("stdout: %w", cause)), "%v", cause)
	}
	assert.False(t, isClosedOutputPipe(windows.ERROR_ACCESS_DENIED))
	assert.False(t, isClosedOutputPipe(nil))
}
