package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func main() {
	os.Exit(runProcess(os.Args[1:], os.Stdout, os.Stderr))
}

func runProcess(args []string, stdout, stderr io.Writer) int {
	diagnostics := &cliDiagnosticWriter{Writer: stderr}
	rootCmd.SetArgs(args)
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(diagnostics)
	if cmd, err := executeCommand(); err != nil {
		code := commandExitCode(err, commandStarted)
		hint := strings.TrimPrefix(commandErrorHint(cmd, err, code, commandStarted), "hint: ")
		hint = strings.Join(strings.Fields(hint), " ")
		message := cliErrorMessage(err)
		if diagnostics.wrote && !diagnostics.lineStart {
			_, _ = fmt.Fprintln(stderr)
		}
		if errorFlagEnabled(cmd, "json") {
			payload := struct {
				Error cliProcessError `json:"error"`
			}{cliProcessError{Message: message, Code: exitCodeName(code), Exit: code, Hint: hint}}
			_ = json.MarshalEncode(jsontext.NewEncoder(stderr, jsontext.AllowInvalidUTF8(true)), payload)
		} else {
			_, _ = fmt.Fprintln(stderr, "error:", message)
			if hint != "" {
				prefix := "hint:"
				if errorFlagEnabled(cmd, "agent") {
					prefix = "Hint:"
				}
				_, _ = fmt.Fprintln(stderr, prefix, hint)
			}
		}
		return code
	}
	return exitSuccess
}

// Diagnostics may end at a prompt or a progress redraw instead of a newline.
// Keep the final error record on its own line without buffering those streams.
type cliDiagnosticWriter struct {
	io.Writer

	wrote     bool
	lineStart bool
}

type fileDescriptor interface {
	Fd() uintptr
}

func (w *cliDiagnosticWriter) Fd() uintptr {
	if file, ok := w.Writer.(fileDescriptor); ok {
		return file.Fd()
	}
	// Buffers and other non-file writers have no terminal descriptor.
	return ^uintptr(0)
}

func (w *cliDiagnosticWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.wrote = true
		w.lineStart = p[n-1] == '\n'
	}
	return n, err
}

type cliProcessError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Exit    int    `json:"exit"`
	Hint    string `json:"hint,omitempty"`
}

// Only flags successfully parsed for this command select a machine mode.
// In particular, --json=false and a positional --json after -- do not.
func errorFlagEnabled(cmd *cobra.Command, name string) bool {
	if cmd == nil || !cmd.Flags().Changed(name) {
		return false
	}
	enabled, err := cmd.Flags().GetBool(name)
	return err == nil && enabled
}
