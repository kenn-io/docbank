package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/push"
)

func TestPushCommandValidatesBeforeContactingDaemon(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"unsupported URL", []string{"folder", "--to", "file:///tmp/daemon", "--name", "laptop", "--dest", "/archive"}, "daemon origin"},
		{"credentials in URL", []string{"folder", "--to", "http://secret@127.0.0.1:7777", "--name", "laptop", "--dest", "/archive"}, "daemon origin"},
		{"invalid name", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "Laptop", "--dest", "/archive"}, "lowercase"},
		{"relative destination", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "archive"}, "absolute virtual path"},
		{"invalid settle", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "/archive", "--settle-time", "0s"}, "must be positive"},
		{"capture requires watch", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "/archive", "--capture-on-change"}, "--capture-on-change requires --watch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := newPushCmd()
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(test.args)
			err := cmd.Execute()
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestPushCommandRequiresKey(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "")
	cmd := newPushCmd()
	cmd.SetArgs([]string{t.TempDir(), "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "/archive"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, cmd.Execute(), "DOCBANK_API_KEY")
}

func TestPushConnectionRefusesRedirects(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	server := httptest.NewServer(keyChallenge("synthetic-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})))
	defer server.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("synthetic bytes"), 0600))
	cmd := newPushCmd()
	cmd.SetArgs([]string{dir, "--to", server.URL, "--name", "laptop", "--dest", "/archive"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.Error(t, cmd.Execute())
	assert.False(t, reached)
}

func TestPushCommandKeyFileOverridesEnvironment(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "unused-environment-key")
	keyPath := filepath.Join(t.TempDir(), "archive.key")
	require.NoError(t, os.WriteFile(keyPath, []byte("synthetic-file-key\n"), 0600))
	var receivedKey string
	server := httptest.NewServer(keyChallenge("synthetic-file-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusServiceUnavailable)
	})))
	defer server.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("synthetic"), 0600))
	cmd := newPushCmd()
	cmd.SetArgs([]string{dir, "--to", server.URL, "--name", "laptop", "--dest", "/archive", "--api-key-file", keyPath})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.Error(t, cmd.Execute())
	assert.Equal(t, "synthetic-file-key", receivedKey)
}

// keyChallenge answers the daemon's API key challenge for key and passes every
// other request to next.
func keyChallenge(key string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != daemonauth.KeyChallengePath {
			next.ServeHTTP(w, r)
			return
		}
		nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]string{"proof": daemonauth.KeyProof(key, nonce)})
	})
}

func TestPushCaptureOnChangeRejectsBeforeCredentialLookup(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "")
	cmd := newPushCmd()
	cmd.SetArgs([]string{t.TempDir(), "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "/archive", "--capture-on-change", "--api-key-file", filepath.Join(t.TempDir(), "missing.key")})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, cmd.Execute(), "--capture-on-change requires --watch")
}

func TestPushMarkdownBrainDefaultsAndOverrides(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	for _, tc := range []struct {
		name             string
		flags            []string
		settle, interval time.Duration
		invalid          bool
	}{
		{"archive", nil, 30 * time.Second, 5 * time.Second, false},
		{"managed", []string{"--markdown-brain"}, 2 * time.Second, 5 * time.Second, false},
		{"explicit", []string{"--markdown-brain", "--settle-time", "9s", "--scan-interval", "11s"}, 9 * time.Second, 11 * time.Second, false},
		{"zero settle", []string{"--markdown-brain", "--settle-time", "0s"}, 0, 0, true},
		{"negative interval", []string{"--markdown-brain", "--scan-interval", "-1s"}, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(keyChallenge("synthetic-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })))
			defer server.Close()
			cmd := newPushCmd()
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			args := []string{t.TempDir(), "--to", server.URL, "--name", "research", "--dest", "/notes"}
			cmd.SetArgs(append(args, tc.flags...))
			err := cmd.Execute()
			require.Error(t, err)
			if tc.invalid {
				require.ErrorContains(t, err, "must be positive")
				return
			}
			settle, _ := cmd.Flags().GetDuration("settle-time")
			interval, _ := cmd.Flags().GetDuration("scan-interval")
			require.Equal(t, tc.settle, settle)
			require.Equal(t, tc.interval, interval)
		})
	}
}
func TestPushMarkdownBrainCapabilityFailureDoesNotFallback(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	archival := 0
	managed := 0
	server := httptest.NewServer(keyChallenge("synthetic-key", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/note-publications") {
			managed++
		} else {
			archival++
		}
		w.WriteHeader(http.StatusNotFound)
	})))
	defer server.Close()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("# synthetic"), 0600))
	cmd := newPushCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	var out, diagnostic bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{root, "--to", server.URL, "--name", "research", "--dest", "/notes", "--markdown-brain"})
	err := cmd.Execute()
	require.ErrorContains(t, err, "managed note publication")
	require.Equal(t, 1, managed)
	require.Zero(t, archival)
	require.NotContains(t, out.String()+diagnostic.String(), "synthetic-key")
	require.NotContains(t, out.String()+diagnostic.String(), root)
}

func TestPushMarkdownBrainStatusOutput(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	s := push.PublicationStatus{Event: "conflict", Observed: 5, Accepted: 2, Unchanged: 1, Failed: 2, Degraded: true, VerificationLag: 3 * time.Second, Current: &api.NotePublicationStatus{Present: 7, Absent: 8, Incomplete: true, Conflicts: 2, LastCompleteAt: "2026-11-07T00:00:00Z"}, Errors: []push.PublicationFailure{{Code: "published_source_diverged", SourceRef: "a.md"}}, ErrorsOmitted: 9}
	require.NoError(t, writePublicationStatus(&out, s))
	text := out.String()
	for _, want := range []string{"inventory conflict: observed 5, accepted 2, unchanged 1, failed 2", "polling-degraded true", "lag 3s", "current: present 7, absent 8", "incomplete true", "last-complete 2026-11-07T00:00:00Z", "archive changed \"a.md\"", "reasons omitted 9"} {
		require.Contains(t, text, want)
	}
	require.NotContains(t, text, "indexed")
	require.NotContains(t, text, "graph-ready")
	sentinel := errors.New("synthetic output stopped")
	require.ErrorIs(t, writePublicationStatus(publicationFailWriter{err: sentinel}, s), sentinel)
}

type publicationFailWriter struct{ err error }

func (w publicationFailWriter) Write([]byte) (int, error) { return 0, w.err }
