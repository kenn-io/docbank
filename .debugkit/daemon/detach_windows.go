//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// windowsDetachedProcess (DETACHED_PROCESS) starts the daemon without a console.
// Hidden consoles expose CONIN$, which can make terminal-probing libraries block
// forever when nothing is present to answer terminal queries.
const windowsDetachedProcess = 0x00000008

func detachChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | windowsDetachedProcess
}
