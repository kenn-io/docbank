package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func actionTerminal(t *testing.T) *os.File {
	t.Helper()
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	allocated, _, _ := kernel.NewProc("AllocConsole").Call()
	if allocated != 0 {
		t.Cleanup(func() { _, _, _ = kernel.NewProc("FreeConsole").Call() })
	}
	console, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, console.Close()) })
	return console
}
