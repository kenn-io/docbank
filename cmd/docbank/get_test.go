package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonauth"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestGetCLIRequestsAndPublishesCurrentVersion(t *testing.T) {
	vault := t.TempDir()
	t.Setenv("DOCBANK_HOME", vault)
	const token = "synthetic-proof-token"
	const content = "download"
	sum := sha256.Sum256([]byte(content))
	node := api.Node{ID: 42, Name: "statement.bin", Kind: "file", Revision: 1, CurrentVersionID: "00000000-0000-4000-8000-000000000021", BlobHash: hex.EncodeToString(sum[:]), Size: int64(len(content))}
	output := filepath.Join(t.TempDir(), "retrieved.bin")
	requests := make(chan string, 8)
	contentRequests := make(chan string, 8)
	lateDestinations := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case kitdaemon.DefaultPingPath:
			assert.NoError(t, json.MarshalWrite(w, kitdaemon.PingInfo{OK: true, Service: daemonconn.Service, Version: version.Version, PID: os.Getpid()}))
		case daemonauth.ChallengePath:
			nonce, err := hex.DecodeString(r.URL.Query().Get("nonce"))
			assert.NoError(t, err)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"proof": daemonauth.Proof(token, nonce)}))
		case "/api/v1/path", "/api/v1/nodes/42":
			assert.Equal(t, http.MethodGet, r.Method)
			requests <- r.URL.Path + "?" + r.URL.Query().Get("path")
			response := node
			if r.URL.Query().Get("path") == "/" {
				response.Kind = "dir"
			}
			if r.URL.Path == "/api/v1/nodes/42" {
				response.TrashedAt = "2026-01-01T00:00:00Z"
			}
			if r.URL.Query().Get("path") == "/inbox/race.bin" {
				response.Name = "race.bin"
			}
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, response))
		case "/api/v1/versions/" + node.CurrentVersionID + "/content":
			assert.Equal(t, http.MethodGet, r.Method)
			contentRequests <- r.URL.Path
			select {
			case sentinel := <-lateDestinations:
				assert.NoError(t, os.WriteFile(output, []byte(sentinel), 0o600))
			default:
			}
			w.Header().Set(api.ContentVersionHeader, node.CurrentVersionID)
			w.Header().Set(api.BlobHashHeader, node.BlobHash)
			w.Header().Set(api.BlobSizeHeader, strconv.FormatInt(node.Size, 10))
			w.Header().Set("Trailer", "Content-Digest")
			_, err := io.WriteString(w, content)
			assert.NoError(t, err)
			w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	_, err := daemonconn.RuntimeStore(vault).Write(daemonconn.NewRecord(strings.TrimPrefix(server.URL, "http://"), "synthetic-key", token, ""))
	require.NoError(t, err)
	for _, test := range []struct {
		selector, lateDestination, expectedError string
		overwrite                                bool
	}{
		{selector: "/inbox/statement.bin"},
		{selector: "id:42", lateDestination: "existing destination", overwrite: true},
		{selector: "/inbox/race.bin", lateDestination: "appeared during download", expectedError: "pass --overwrite"},
	} {
		err := os.Remove(output)
		if !os.IsNotExist(err) {
			require.NoError(t, err)
		}
		if test.lateDestination != "" {
			lateDestinations <- test.lateDestination
		}
		args := []string{"get", test.selector, output, "--json"}
		if test.overwrite {
			args = append(args, "--overwrite")
		}
		out, err := runCLI(t, args...)
		if test.expectedError != "" {
			require.ErrorContains(t, err, test.expectedError)
			published, readErr := os.ReadFile(output)
			require.NoError(t, readErr)
			assert.Equal(t, test.lateDestination, string(published))
		} else {
			require.NoError(t, err, out)
			var receipt getReceipt
			require.NoError(t, json.Unmarshal([]byte(out), &receipt))
			assert.Equal(t, getReceipt{NodeID: node.ID, VersionID: node.CurrentVersionID, BlobHash: node.BlobHash, Size: node.Size, Output: output}, receipt)
			assert.NotContains(t, out, "download:")
			published, readErr := os.ReadFile(output)
			require.NoError(t, readErr)
			assert.Equal(t, content, string(published))
		}
		stages, err := filepath.Glob(filepath.Join(filepath.Dir(output), "docbank-get-*"))
		require.NoError(t, err)
		assert.Empty(t, stages)
		if test.selector == "id:42" {
			assert.Equal(t, "/api/v1/nodes/42?", <-requests)
		} else {
			assert.Equal(t, "/api/v1/path?"+test.selector, <-requests)
		}
		assert.Equal(t, "/api/v1/versions/"+node.CurrentVersionID+"/content", <-contentRequests)
	}
	require.NoError(t, os.WriteFile(output, []byte("keep me"), 0o600))
	_, err = runCLI(t, "get", "/inbox/statement.bin", output)
	require.ErrorContains(t, err, "pass --overwrite")
	published, readErr := os.ReadFile(output)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(published))
	require.Empty(t, requests)
	_, err = runCLI(t, "get", "/missing", filepath.Join(t.TempDir(), "out"), "--progress", "animated")
	require.ErrorContains(t, err, "invalid --progress value")
	require.Empty(t, requests)
	_, err = runCLI(t, "get", "/", filepath.Join(t.TempDir(), "root"))
	require.ErrorContains(t, err, "not a file")
	require.Len(t, requests, 1)
	assert.Equal(t, "/api/v1/path?/", <-requests)
	assert.Empty(t, contentRequests)
}

func TestPublishGetFileForwardsOverwriteAndReportsErrors(t *testing.T) {
	for _, test := range []struct {
		name           string
		overwrite      bool
		existingOutput bool
		wantSyncError  bool
	}{
		{name: "existing destination", existingOutput: true},
		{name: "new destination sync failure", wantSyncError: true},
		{name: "replacement sync failure", overwrite: true, existingOutput: true, wantSyncError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			staged, output := filepath.Join(dir, "staged"), filepath.Join(dir, "output")
			require.NoError(t, os.WriteFile(staged, []byte("download"), 0o600))
			if test.existingOutput {
				require.NoError(t, os.WriteFile(output, []byte("existing"), 0o600))
			}
			syncErr := errors.New("injected destination directory sync failure")
			originalSync := syncGetDestinationDir
			syncCalled := false
			syncGetDestinationDir = func(got string) error {
				syncCalled = true
				assert.Equal(t, filepath.Dir(output), got)
				return syncErr
			}
			t.Cleanup(func() { syncGetDestinationDir = originalSync })
			err := publishGetFile(staged, output, test.overwrite)
			if test.wantSyncError {
				require.ErrorIs(t, err, syncErr)
				require.ErrorContains(t, err, "file published")
				require.ErrorContains(t, err, "injected destination directory sync failure")
				require.True(t, syncCalled)
				published, readErr := os.ReadFile(output)
				require.NoError(t, readErr)
				assert.Equal(t, "download", string(published))
			} else {
				require.ErrorIs(t, err, os.ErrExist)
				require.ErrorContains(t, err, "publish without replacing")
				assert.False(t, syncCalled)
			}
		})
	}
}

func TestGetIntegrityFailureNeverPublishesPartialDestination(t *testing.T) {
	dir := t.TempDir()
	staging, err := makePrivateStagingDirAt(dir, "docbank-get-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, staging.removeAll()) })
	content := "complete bytes without the required digest trailer"
	digest := sha256.Sum256([]byte(content))
	stream := &daemonconn.ContentStream{
		ReadCloser: io.NopCloser(strings.NewReader(content)),
		VersionID:  "7b0dbd90-5730-4436-b082-b692790725ff",
		BlobHash:   hex.EncodeToString(digest[:]),
		Size:       int64(len(content)),
	}
	output := filepath.Join(dir, "must-not-exist")
	renderer := newBackupProgressRenderer(io.Discard, backupProgressAuto)
	_, err = stageAndPublishGet(context.Background(), staging, stream, stream.Size,
		output, false, renderer)
	require.Error(t, err)
	require.ErrorIs(t, err, daemonconn.ErrIntegrity)
	_, statErr := os.Lstat(output)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
