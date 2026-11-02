//go:build windows

package consolewindow

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
