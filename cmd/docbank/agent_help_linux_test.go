package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestRunProcessKeepsTerminalProgress(t *testing.T) {
	terminal := actionTerminal(t)
	mode := backupProgressPlain
	probe := processProbe(t, func(cmd *cobra.Command, _ []string) error {
		mode = newBackupProgressRenderer(cmd.ErrOrStderr(), backupProgressAuto).outputMode()
		return nil
	})
	resetFlags(rootCmd)
	require.Equal(t, 0, runProcess([]string{probe.Name()}, io.Discard, terminal))
	assert.Equal(t, backupProgressBar, mode)
}

func TestRunProcessRecognizesStderrOnlyTerminal(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	terminal := actionTerminal(t)
	events := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case events <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"queued"}`)
	}))
	t.Cleanup(server.Close)
	probe := processProbe(t, func(cmd *cobra.Command, _ []string) error {
		reportAction(cmd, daemonconn.New(server.URL, "synthetic-key"), "error_shown", "", "", false)
		return nil
	})
	probe.SetIn(bytes.NewReader(nil))
	resetFlags(rootCmd)
	require.Equal(t, 0, runProcess([]string{probe.Name()}, io.Discard, terminal))
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("stderr-only terminal was hidden from action reporting")
	}
}
