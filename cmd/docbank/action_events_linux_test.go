package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"golang.org/x/sys/unix"
)

func actionTerminal(t *testing.T) *os.File {
	t.Helper()
	// Give the command real terminal stdout, without depending on the test runner's terminal.
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("terminal unavailable: %v", err)
	}
	t.Cleanup(func() { require.NoError(t, master.Close()) })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("terminal unavailable: %v", err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skipf("terminal unavailable: %v", err)
	}
	terminal, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("terminal unavailable: %v", err)
	}
	t.Cleanup(func() { require.NoError(t, terminal.Close()) })
	return terminal
}

func TestCLICompletedSearchAndContentActions(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	terminal := actionTerminal(t)
	const content = "synthetic document"
	const version = "11111111-1111-4111-8111-111111111111"
	digest := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(digest[:])
	events := make(chan string, 10)
	var fail bool
	var added int
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/daemon/telemetry/events":
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			events <- string(body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"queued"}`)
		case fail:
			http.Error(w, "synthetic failure", http.StatusInternalServerError)
		case r.URL.Path == "/api/v1/ingest":
			assert.NoError(t, json.MarshalWrite(w, api.IngestReport{Added: added}))
		case r.URL.Path == "/api/v1/processing/profiles":
			assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"semantic"}}}))
		case r.URL.Path == "/api/v1/info":
			assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: "22222222-2222-4222-8222-222222222222"}))
		case r.URL.Path == "/api/v1/search/similar":
			assert.NoError(t, json.MarshalWrite(w, api.DocumentSimilarReport{State: "ready", Source: api.DocumentSimilarSource{NodeID: 42, ContentVersionID: version}, BindingID: "semantic", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}))
		case r.URL.Path == "/api/v1/search":
			assert.NoError(t, json.MarshalWrite(w, api.SearchReport{Limit: 20}))
		case r.URL.Path == "/api/v1/nodes/42":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Kind: "file", CurrentVersionID: version, BlobHash: hash, Size: int64(len(content))}))
		case strings.HasSuffix(r.URL.Path, "/content"):
			w.Header().Set(api.ContentVersionHeader, version)
			w.Header().Set(api.BlobHashHeader, hash)
			w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(content)))
			w.Header().Set("Trailer", "Content-Digest")
			_, _ = io.WriteString(w, content)
			w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil); resetFlags(rootCmd) })
	for _, tc := range []struct {
		name  string
		args  []string
		event string
	}{
		{"add", []string{"add", "synthetic.txt", "--json"}, `{"event":"document_added","properties":{"surface":"cli"}}`},
		{"search", []string{"search", "synthetic", "--mode", "lexical"}, `{"event":"search_run","properties":{"surface":"cli","search_type":"text"}}`},
		{"similar", []string{"search", "--similar-to", "id:42", "--profile", "private", "--source-version", version}, `{"event":"search_run","properties":{"surface":"cli","search_type":"semantic"}}`},
		{"cat", []string{"cat", "id:42"}, `{"event":"document_opened","properties":{"surface":"cli"}}`},
		{"get", []string{"get", "id:42", filepath.Join(t.TempDir(), "download.txt")}, `{"event":"document_opened","properties":{"surface":"cli"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, failed := range []bool{true, false} {
				fail = failed
				added = 3
				resetFlags(rootCmd)
				rootCmd.SetOut(terminal)
				rootCmd.SetErr(io.Discard)
				if tc.name == "cat" || tc.name == "get" {
					rootCmd.SetOut(io.Discard)
					rootCmd.SetErr(terminal)
				}
				rootCmd.SetArgs(tc.args)
				err := rootCmd.ExecuteContext(context.Background())
				// Action reporting waits for the daemon before the command returns.
				if fail {
					require.Error(t, err)
					assert.Empty(t, events)
				} else {
					require.NoError(t, err)
					require.Len(t, events, 1)
					assert.JSONEq(t, tc.event, <-events)
					assert.Empty(t, events)
					if tc.name == "add" {
						added = 0
						resetFlags(rootCmd)
						rootCmd.SetArgs(tc.args)
						require.NoError(t, rootCmd.ExecuteContext(context.Background()))
						assert.Empty(t, events)
					}
				}
			}
		})
	}
}

func TestActionReportingStandardStreams(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	terminal := actionTerminal(t)
	reports := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reports <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"queued"}`)
	}))
	defer server.Close()
	c := daemonconn.New(server.URL, "synthetic-key")
	for _, tc := range []struct {
		stream string
		want   int
	}{
		{stream: "stdin", want: 1},
		{stream: "none"},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			cmd.SetIn(strings.NewReader(""))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if tc.stream == "stdin" {
				cmd.SetIn(terminal)
			}
			reportAction(cmd, c, "document_opened", "", false)
			// reportAction posts synchronously, so the request is complete here.
			if tc.want > 0 {
				require.Len(t, reports, 1)
				<-reports
			}
			assert.Empty(t, reports)
		})
	}
}
