package providerutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedCommandStopsOnCancellation(t *testing.T) {
	if mode, marker := managedCommandHelperArguments(); mode != "" {
		runManagedCommandHelper(mode, marker)
		return
	}

	directory := t.TempDir()
	marker := filepath.Join(directory, "heartbeat")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"-test.run=^TestManagedCommandStopsOnCancellation$", "--", "process", marker)
	managed, err := NewManagedCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	runResult := make(chan error, 1)
	go func() { runResult <- managed.Run() }()

	waitForFile(t, marker)
	cancel()
	select {
	case err := <-runResult:
		if err == nil {
			t.Fatal("canceled managed command succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("managed command did not stop after cancellation")
	}

	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	after, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("process remained alive after cancellation: heartbeat grew from %d to %d bytes",
			before.Size(), after.Size())
	}
}

func managedCommandHelperArguments() (string, string) {
	for index, argument := range os.Args {
		if argument == "--" && index+2 < len(os.Args) {
			return os.Args[index+1], os.Args[index+2]
		}
	}
	return "", ""
}

func runManagedCommandHelper(mode, marker string) {
	switch mode {
	case "process":
		file, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := file.WriteString("x"); err != nil {
				_ = file.Close()
				os.Exit(2)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := file.Close(); err != nil {
			os.Exit(2)
		}
	default:
		_, _ = fmt.Fprintln(os.Stderr, "unknown managed-command helper mode")
		os.Exit(2)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if information, err := os.Stat(path); err == nil && information.Size() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}
