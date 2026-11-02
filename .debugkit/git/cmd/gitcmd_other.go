//go:build js || plan9 || wasip1

package gitcmd

import "os/exec"

func prepareGitCommand(*exec.Cmd, bool) {}
