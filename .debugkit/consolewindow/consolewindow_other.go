//go:build !windows

package consolewindow

import "os/exec"

func hide(*exec.Cmd) {}
