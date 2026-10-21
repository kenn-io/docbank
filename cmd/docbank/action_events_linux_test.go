package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
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
	var failedItems []api.IngestFailure
	kind := "file"
	badDigest := false
	var concurrentDestination string
	var hits []api.SearchHit
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
			assert.NoError(t, json.MarshalWrite(w, api.IngestReport{Added: added, Failed: failedItems}))
		case r.URL.Path == "/api/v1/processing/profiles":
			assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{Name: "private", EmbeddingBindings: []string{"semantic"}}}))
		case r.URL.Path == "/api/v1/info":
			assert.NoError(t, json.MarshalWrite(w, api.VaultInfo{VaultID: "22222222-2222-4222-8222-222222222222"}))
		case r.URL.Path == "/api/v1/search/similar":
			assert.NoError(t, json.MarshalWrite(w, api.DocumentSimilarReport{State: "ready", Source: api.DocumentSimilarSource{NodeID: 42, ContentVersionID: version}, BindingID: "semantic", Coverage: api.DocumentSearchCoverage{State: "complete", ScopedDocuments: 1, CompleteDocuments: 1}}))
		case r.URL.Path == "/api/v1/search":
			assert.NoError(t, json.MarshalWrite(w, api.SearchReport{Limit: 20, Hits: hits}))
		case r.URL.Path == "/api/v1/nodes/42":
			assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Kind: kind, CurrentVersionID: version, BlobHash: hash, Size: int64(len(content))}))
		case strings.HasSuffix(r.URL.Path, "/content"):
			if concurrentDestination != "" {
				assert.NoError(t, os.WriteFile(concurrentDestination, []byte("concurrent"), 0o600))
			}
			w.Header().Set(api.ContentVersionHeader, version)
			w.Header().Set(api.BlobHashHeader, hash)
			w.Header().Set(api.BlobSizeHeader, strconv.Itoa(len(content)))
			w.Header().Set("Trailer", "Content-Digest")
			_, _ = io.WriteString(w, content)
			if !badDigest {
				w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil); resetFlags(rootCmd) })
	const searchRun = `{"event":"search_run","properties":{"surface":"cli","search_type":%q}}`
	const searchEmpty = `{"event":"search_empty","properties":{"surface":"cli","search_type":%q}}`
	opened := `{"event":"document_opened","properties":{"surface":"cli"}}`
	const shown = `{"event":"error_shown","properties":{"surface":"cli","kind":%q}}`
	execute := func(failed, stderr bool, args []string) error {
		fail = failed
		resetFlags(rootCmd)
		rootCmd.SetOut(terminal)
		rootCmd.SetErr(io.Discard)
		if stderr {
			rootCmd.SetOut(io.Discard)
			rootCmd.SetErr(terminal)
		}
		rootCmd.SetArgs(args)
		return rootCmd.ExecuteContext(context.Background())
	}
	for _, tc := range []struct {
		name  string
		args  []string
		event string
		// failure is the error_shown report when the daemon fails the command.
		failure string
		// stderr marks commands whose output goes to the error stream.
		stderr bool
		// emptyEvent is also reported because the fake daemon finds no hits.
		emptyEvent string
		// nonEmptyRerun reruns with a hit, which reports only the search.
		nonEmptyRerun bool
		// nothingAddedRerun reruns an add that stores nothing, which reports nothing.
		nothingAddedRerun bool
	}{
		{name: "add", args: []string{"add", "synthetic.txt", "--json"}, failure: fmt.Sprintf(shown, "add"), event: `{"event":"document_added","properties":{"surface":"cli"}}`, nothingAddedRerun: true},
		{name: "search", args: []string{"search", "synthetic", "--mode", "lexical"}, failure: fmt.Sprintf(shown, "search"), event: fmt.Sprintf(searchRun, "text"), emptyEvent: fmt.Sprintf(searchEmpty, "text"), nonEmptyRerun: true},
		{name: "similar", args: []string{"search", "--similar-to", "id:42", "--profile", "private", "--source-version", version}, failure: fmt.Sprintf(shown, "search"), event: fmt.Sprintf(searchRun, "semantic"), emptyEvent: fmt.Sprintf(searchEmpty, "semantic")},
		{name: "cat", args: []string{"cat", "id:42"}, failure: fmt.Sprintf(shown, "open"), event: opened, stderr: true},
		{name: "get", args: []string{"get", "id:42", filepath.Join(t.TempDir(), "download.txt")}, failure: fmt.Sprintf(shown, "open"), event: opened, stderr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(failed bool, args []string) error { return execute(failed, tc.stderr, args) }
			added = 3
			// Action reporting waits for the daemon before the command returns.
			require.Error(t, run(true, tc.args))
			require.Len(t, events, 1)
			assert.JSONEq(t, tc.failure, <-events)

			require.NoError(t, run(false, tc.args))
			want := []string{tc.event}
			if tc.emptyEvent != "" {
				want = append(want, tc.emptyEvent)
			}
			require.Len(t, events, len(want))
			for _, event := range want {
				assert.JSONEq(t, event, <-events)
			}

			if tc.nonEmptyRerun {
				hits = []api.SearchHit{{Node: api.Node{ID: 42, Kind: "file", Name: "synthetic.txt"}}}
				t.Cleanup(func() { hits = nil })
				require.NoError(t, run(false, tc.args))
				require.Len(t, events, 1)
				assert.JSONEq(t, tc.event, <-events)
			}
			if tc.nothingAddedRerun {
				added = 0
				require.NoError(t, run(false, tc.args))
			}
			assert.Empty(t, events)
		})
	}
	t.Run("add invalid progress", func(t *testing.T) {
		err := execute(false, false, []string{"add", "synthetic.txt", "--progress", "invalid"})
		require.ErrorContains(t, err, `invalid --progress value "invalid"`)
		assert.Empty(t, events)
	})
	for _, cause := range []error{io.ErrClosedPipe, syscall.EPIPE} {
		t.Run("add closed pipe "+cause.Error(), func(t *testing.T) {
			added = 3
			resetFlags(rootCmd)
			rootCmd.SetErr(terminal)
			rootCmd.SetOut(reportTestWriter(func([]byte) (int, error) { return 0, cause }))
			rootCmd.SetArgs([]string{"add", "synthetic.txt", "--json"})
			require.ErrorIs(t, rootCmd.ExecuteContext(context.Background()), cause)
			require.Len(t, events, 1)
			assert.JSONEq(t, `{"event":"document_added","properties":{"surface":"cli"}}`, <-events)
			assert.Empty(t, events)
		})
	}
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("add partial success closed pipe %t", closed), func(t *testing.T) {
			added = 1
			failedItems = []api.IngestFailure{{Path: "synthetic-failed.txt", Error: "synthetic failure"}}
			defer func() { failedItems = nil }()
			resetFlags(rootCmd)
			rootCmd.SetErr(terminal)
			rootCmd.SetOut(io.Discard)
			if closed {
				rootCmd.SetOut(reportTestWriter(func([]byte) (int, error) { return 0, io.ErrClosedPipe }))
			}
			rootCmd.SetArgs([]string{"add", "synthetic.txt", "synthetic-failed.txt", "--json"})
			require.ErrorContains(t, rootCmd.ExecuteContext(context.Background()), "1 file(s) failed to import")
			require.Len(t, events, 2)
			assert.JSONEq(t, `{"event":"document_added","properties":{"surface":"cli"}}`, <-events)
			assert.JSONEq(t, fmt.Sprintf(shown, "add"), <-events)
			assert.Empty(t, events)
		})
	}
	for _, tc := range []struct {
		name    string
		args    []string
		message string
	}{
		{name: "document search usage", args: []string{"search", "synthetic", "--mode", "hybrid", "--profile", "missing", "--source-version", version}, message: "not executable"},
		{name: "similar search usage", args: []string{"search", "--similar-to", "id:42", "--profile", "private", "--source-version", "22222222-2222-4222-8222-222222222222", "--version", "22222222-2222-4222-8222-222222222222"}, message: "current live file version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := execute(false, false, tc.args)
			require.ErrorContains(t, err, tc.message)
			assert.Equal(t, exitUsage, commandExitCode(err, true))
			assert.Empty(t, events)
		})
	}
	t.Run("cat digest failure", func(t *testing.T) {
		badDigest = true
		defer func() { badDigest = false }()
		err := execute(false, true, []string{"cat", "id:42"})
		require.ErrorIs(t, err, daemonconn.ErrIntegrity)
		require.Len(t, events, 1)
		assert.JSONEq(t, fmt.Sprintf(shown, "open"), <-events)
	})
	t.Run("cat directory", func(t *testing.T) {
		kind = "dir"
		defer func() { kind = "file" }()
		err := execute(false, true, []string{"cat", "id:42"})
		require.ErrorIs(t, err, store.ErrNotFile)
		require.Len(t, events, 1)
		assert.JSONEq(t, fmt.Sprintf(shown, "open"), <-events)
	})
	for _, tc := range []struct {
		name   string
		cause  error
		report bool
	}{
		{name: "cat closed pipe", cause: io.ErrClosedPipe},
		{name: "cat EPIPE", cause: syscall.EPIPE},
		{name: "cat other write failure", cause: errors.New("synthetic write failure"), report: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(rootCmd)
			rootCmd.SetErr(terminal)
			rootCmd.SetOut(reportTestWriter(func([]byte) (int, error) { return 0, tc.cause }))
			rootCmd.SetArgs([]string{"cat", "id:42"})
			require.ErrorIs(t, rootCmd.ExecuteContext(context.Background()), tc.cause)
			if tc.report {
				require.Len(t, events, 1)
				assert.JSONEq(t, fmt.Sprintf(shown, "open"), <-events)
			}
			assert.Empty(t, events)
		})
	}
	for _, tc := range []struct {
		name   string
		cause  error
		report bool
	}{
		{name: "search closed pipe", cause: io.ErrClosedPipe},
		{name: "search EPIPE", cause: syscall.EPIPE},
		{name: "search other write failure", cause: errors.New("synthetic write failure"), report: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(rootCmd)
			rootCmd.SetErr(terminal)
			rootCmd.SetOut(reportTestWriter(func([]byte) (int, error) { return 0, tc.cause }))
			rootCmd.SetArgs([]string{"search", "synthetic", "--mode", "lexical", "--json"})
			require.ErrorIs(t, rootCmd.ExecuteContext(context.Background()), tc.cause)
			if tc.report {
				require.Len(t, events, 1)
				assert.JSONEq(t, fmt.Sprintf(shown, "search"), <-events)
			}
			assert.Empty(t, events)
		})
	}
	t.Run("get concurrent destination", func(t *testing.T) {
		concurrentDestination = filepath.Join(t.TempDir(), "download.txt")
		defer func() { concurrentDestination = "" }()
		err := execute(false, true, []string{"get", "id:42", concurrentDestination})
		require.ErrorIs(t, err, os.ErrExist)
		assert.Empty(t, events)
	})
}

func TestGetStagingFailureReportsOpen(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	terminal := actionTerminal(t)
	events := make(chan string, 10)
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/daemon/telemetry/events", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		events <- string(body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"queued"}`)
	})
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(dir, 0o700)) })
	output := filepath.Join(dir, "download.txt")
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil); resetFlags(rootCmd) })
	resetFlags(rootCmd)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(terminal)
	rootCmd.SetArgs([]string{"get", "id:42", output})
	err := rootCmd.ExecuteContext(context.Background())
	require.ErrorIs(t, err, os.ErrPermission)
	require.ErrorContains(t, err, "creating private download staging")
	assert.NoFileExists(t, output)
	require.Len(t, events, 1)
	assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"open"}}`, <-events)
}

func TestActionReportingStandardStreams(t *testing.T) {
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	terminal := actionTerminal(t)
	reports := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		reports <- string(body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"status":"queued"}`)
	}))
	t.Cleanup(server.Close)
	c := daemonconn.New(server.URL, "synthetic-key")
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	for _, tc := range []struct {
		stream string
		want   int
	}{
		{stream: "stdin", want: 1},
		{stream: "none"},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			cmd.SetIn(strings.NewReader(""))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if tc.stream == "stdin" {
				cmd.SetIn(terminal)
			}
			reportAction(cmd, c, "document_opened", "", "", false)
			// reportAction posts synchronously, so the request is complete here.
			if tc.want > 0 {
				require.Len(t, reports, 1)
				<-reports
			}
			assert.Empty(t, reports)
		})
	}
}
