package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(runProcess(os.Args[1:], os.Stdout, os.Stderr))
}

func runProcess(args []string, stdout, stderr io.Writer) int {
	rootCmd.SetArgs(args)
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	if err := Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		code := commandExitCode(err, commandStarted)
		cmd, _, _ := rootCmd.Find(args)
		if hint := commandErrorHint(cmd, err, code, commandStarted); hint != "" {
			_, _ = fmt.Fprintln(stderr, hint)
		}
		return code
	}
	return exitSuccess
}
