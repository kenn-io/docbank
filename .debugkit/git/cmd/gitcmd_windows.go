//go:build windows

package gitcmd

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"

	"go.kenn.io/kit/consolewindow"
)

func prepareGitCommand(cmd *exec.Cmd, hideConsoleWindow bool) {
	if hideConsoleWindow {
		consolewindow.Hide(cmd)
	}
	cmd.Cancel = func() error {
		systemDir, err := windows.GetSystemDirectory()
		if err == nil {
			killCtx, cancel := context.WithTimeout(context.Background(), gitCommandWaitDelay)
			defer cancel()
			kill := exec.CommandContext(killCtx, filepath.Join(systemDir, "taskkill.exe"),
				"/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
			consolewindow.Hide(kill)
			if err = kill.Run(); err == nil {
				return nil
			}
		}
		return cmd.Process.Kill()
	}
}
