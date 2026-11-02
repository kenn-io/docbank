//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	detachedProcess = 0x00000008
	createNoWindow  = 0x08000000
)

func TestDetachChildDetachesWithoutConsole(t *testing.T) {
	assert := assert.New(t)

	cmd := exec.CommandContext(t.Context(), "cmd.exe")
	detachChild(cmd)

	flags := cmd.SysProcAttr.CreationFlags
	assert.NotZero(flags&syscall.CREATE_NEW_PROCESS_GROUP, "child must run in its own process group")
	assert.NotZero(flags&detachedProcess, "child must start without an attached console")
	assert.Zero(flags&createNoWindow, "hidden consoles expose CONIN$ and can block terminal probes")
}

func TestDetachChildPreservesExistingSysProcAttr(t *testing.T) {
	assert := assert.New(t)

	const createSuspended = 0x00000004
	cmd := exec.CommandContext(t.Context(), "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	detachChild(cmd)

	flags := cmd.SysProcAttr.CreationFlags
	assert.NotZero(flags&createSuspended, "caller-set creation flags must be preserved")
	assert.NotZero(flags & detachedProcess)
}
