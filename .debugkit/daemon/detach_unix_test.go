//go:build !windows

package daemon

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDetachChildStartsNewSession(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh")
	detachChild(cmd)

	assert.True(t, cmd.SysProcAttr.Setsid, "child must start its own session")
}
