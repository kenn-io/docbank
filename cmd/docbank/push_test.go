package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/daemonauth"
)

func TestPushCommandValidatesBeforeContactingDaemon(t *testing.T) {
	t.Setenv("DOCBANK_API_KEY", "synthetic-key")
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"missing required", []string{"folder"}, "required flag"},
		{"unsupported URL", []string{"folder", "--to", "file:///tmp/daemon", "--name", "laptop", "--dest", "/archive"}, "daemon origin"},
		{"credentials in URL", []string{"folder", "--to", "http://secret@127.0.0.1:7777", "--name", "laptop", "--dest", "/archive"}, "daemon origin"},
		{"invalid policy", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "/archive", "--duplicates", "merge"}, "link, skip, or create"},
		{"invalid name", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "Laptop", "--dest", "/archive"}, "lowercase"},
		{"relative destination", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "archive"}, "absolute virtual path"},
		{"invalid settle", []string{"folder", "--to", "http://127.0.0.1:1", "--name", "laptop", "--dest", "/archive", "--settle-time", "0s"}, "must be positive"},
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
