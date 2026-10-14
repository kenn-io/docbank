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
	requests := make(chan string, 8)
	contentRequests := make(chan string, 8)
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
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.MarshalWrite(w, response))
		case "/api/v1/versions/" + node.CurrentVersionID + "/content":
			assert.Equal(t, http.MethodGet, r.Method)
			contentRequests <- r.URL.Path
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
	output := filepath.Join(t.TempDir(), "retrieved.bin")
	for _, selector := range []string{"/inbox/statement.bin", "id:42"} {
		args := []string{"get", selector, output, "--json"}
		if selector == "id:42" {
			require.NoError(t, os.WriteFile(output, []byte("existing destination"), 0o600))
			args = append(args, "--overwrite")
		}
		out, err := runCLI(t, args...)
		require.NoError(t, err, out)
		var receipt getReceipt
		require.NoError(t, json.Unmarshal([]byte(out), &receipt))
		assert.Equal(t, getReceipt{NodeID: node.ID, VersionID: node.CurrentVersionID, BlobHash: node.BlobHash, Size: node.Size, Output: output}, receipt)
		assert.NotContains(t, out, "download:")
		assert.Equal(t, selector == "id:42", getOverwrite)
		published, err := os.ReadFile(output)
		require.NoError(t, err)
		assert.Equal(t, content, string(published))
		stages, err := filepath.Glob(filepath.Join(filepath.Dir(output), "docbank-get-*"))
		require.NoError(t, err)
		assert.Empty(t, stages)
		if selector == "id:42" {
			assert.Equal(t, "/api/v1/nodes/42?", <-requests)
		} else {
			assert.Equal(t, "/api/v1/path?/inbox/statement.bin", <-requests)
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
	for _, overwrite := range []bool{false, true} {
		t.Run(strconv.FormatBool(overwrite), func(t *testing.T) {
			dir := t.TempDir()
			staged, output := filepath.Join(dir, "staged"), filepath.Join(dir, "output")
			require.NoError(t, os.WriteFile(staged, []byte("download"), 0o600))
			require.NoError(t, os.WriteFile(output, []byte("existing"), 0o600))
			syncErr := errors.New("injected destination directory sync failure")
			originalSync := syncGetDestinationDir
			syncGetDestinationDir = func(got string) error {
				assert.True(t, overwrite, "no-replace failure must return before syncing")
				assert.Equal(t, filepath.Dir(output), got)
				return syncErr
			}
			t.Cleanup(func() { syncGetDestinationDir = originalSync })
			err := publishGetFile(staged, output, overwrite)
			if overwrite {
				require.ErrorIs(t, err, syncErr)
				require.ErrorContains(t, err, "file published")
				require.ErrorContains(t, err, "injected destination directory sync failure")
			} else {
				require.ErrorIs(t, err, os.ErrExist)
				require.ErrorContains(t, err, "publish without replacing")
			}
		})
	}
}

func TestGetIntegrityFailureNeverPublishesPartialDestination(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(strconv.FormatBool(valid), func(t *testing.T) {
			dir := t.TempDir()
			staging, err := makePrivateStagingDirAt(dir, "docbank-get-")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, staging.removeAll()) })
			const content = "complete downloaded bytes"
			const versionID = "7b0dbd90-5730-4436-b082-b692790725ff"
			digest := sha256.Sum256([]byte(content))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.ContentVersionHeader, versionID)
				w.Header().Set(api.BlobHashHeader, hex.EncodeToString(digest[:]))
				w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(content)))
				w.Header().Set("Trailer", "Content-Digest")
				_, writeErr := io.WriteString(w, content)
				assert.NoError(t, writeErr)
				if valid {
					w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
				}
			}))
			t.Cleanup(server.Close)
			stream, err := daemonconn.New(server.URL, "synthetic-key").VersionContent(t.Context(), versionID)
			require.NoError(t, err)
			output := filepath.Join(dir, "download")
			renderer := newBackupProgressRenderer(io.Discard, backupProgressAuto)
			_, err = stageAndPublishGet(context.Background(), staging, stream, stream.Size, output, false, renderer)
			if valid {
				require.NoError(t, err)
				copied, readErr := os.ReadFile(output)
				require.NoError(t, readErr)
				assert.Equal(t, content, string(copied))
			} else {
				require.ErrorIs(t, err, daemonconn.ErrIntegrity)
				_, statErr := os.Lstat(output)
				assert.ErrorIs(t, statErr, os.ErrNotExist)
			}
		})
	}
}
