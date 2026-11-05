package providerutil

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
)

// managedCommandReady is the line the blocking helper prints once it runs.
const managedCommandReady = "managed-command-helper-ready"

func TestManagedCommandStopsOnCancellation(t *testing.T) {
	if mode := managedCommandHelperMode(); mode != "" {
		runManagedCommandHelper(mode)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stdinReader, stdinWriter := testPipe(t)
	stdoutReader, stdoutWriter := testPipe(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"-test.run=^TestManagedCommandStopsOnCancellation$", "--", "block")
	command.Stdin = stdinReader
	command.Stdout = stdoutWriter
	managed, err := NewManagedCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	runResult := make(chan error, 1)
	go func() { runResult <- managed.Run() }()

	waitForHelperReady(t, stdoutReader, runResult)
	// The running helper now holds its own copy of stdin. Dropping ours
	// leaves the helper as the only reader.
	if err := stdinReader.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-runResult; err == nil {
		t.Fatal("canceled managed command succeeded")
	}
	// The helper blocks reading stdin until it is killed. A write succeeds
	// only while some process still holds the read end open.
	if _, err := stdinWriter.WriteString("x"); err == nil {
		t.Fatal("process remained alive after cancellation: its stdin is still open")
	}
}

func managedCommandHelperMode() string {
	for index, argument := range os.Args {
		if argument == "--" && index+1 < len(os.Args) {
			return os.Args[index+1]
		}
	}
	return ""
}

func runManagedCommandHelper(mode string) {
	switch mode {
	case "block":
		if _, err := fmt.Fprintln(os.Stdout, managedCommandReady); err != nil {
			os.Exit(2)
		}
		// Run until the test closes stdin or the managed command kills us.
		_, _ = io.Copy(io.Discard, os.Stdin)
	default:
		_, _ = fmt.Fprintln(os.Stderr, "unknown managed-command helper mode")
		os.Exit(2)
	}
}

// waitForHelperReady blocks until the helper reports that it is running. It
// fails the test instead if the command finishes first.
func waitForHelperReady(t *testing.T, stdout io.Reader, finished <-chan error) {
	t.Helper()
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == managedCommandReady {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case err := <-finished:
		t.Fatalf("helper process finished before reporting readiness: %v", err)
	}
}

func testPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})
	return reader, writer
}
