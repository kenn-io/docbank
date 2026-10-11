//go:build unix

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPushInterruptPrintsSummary sends a real SIGINT while a request is in
// flight. The command must stop cleanly and still print its counts.
func TestPushInterruptPrintsSummary(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	for _, test := range []struct {
		name    string
		watch   bool
		wantErr bool
	}{
		{name: "watch stops without error", watch: true},
		{name: "one-shot reports interruption", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			inFlight := make(chan struct{}, 1)
			server := httptest.NewServer(keyChallenge("synthetic-key", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				select {
				case inFlight <- struct{}{}:
				default:
				}
				<-r.Context().Done()
			})))
			defer server.Close()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("synthetic"), 0600))
			args := []string{dir, "--to", server.URL, "--name", "laptop", "--dest", "/archive"}
			if test.watch {
				args = append(args, "--watch", "--settle-time", "1ms", "--scan-interval", "10ms")
			}
			cmd := newPushCmd()
			cmd.SetArgs(args)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			done := make(chan error, 1)
			go func() { done <- cmd.Execute() }()
			select {
			case <-inFlight:
			case <-time.After(30 * time.Second):
				t.Fatal("push did not reach the daemon")
			}
			require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("push did not stop after SIGINT")
			}
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Contains(t, out.String(), "added 0, updated 0, linked 0, unchanged 0, duplicate-skipped 0, failed 0")
		})
	}
}
