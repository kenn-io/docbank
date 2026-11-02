// Package consolewindow controls console windows for child processes.
package consolewindow

import "os/exec"

// Hide prevents a non-interactive child from opening a console window on Windows.
// Call it before starting cmd when its output is captured or discarded.
// It preserves existing process flags and does nothing on other platforms.
// Windows ignores this flag with DETACHED_PROCESS or CREATE_NEW_CONSOLE;
// use daemon.StartDetached for detached daemons.
func Hide(cmd *exec.Cmd) {
	hide(cmd)
}
