package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/store"
)

func TestMCPNativeExportWorkflow(t *testing.T) {
	f := newNativeExportFixture(t)
	old := f.addFile(t, "synthetic.txt", "synthetic original\n")
	empty := f.addFile(t, "empty.txt", "")
	f.addFile(t, "unselected.txt", "unselected bytes")
	node, err := f.catalog.NodeByID(t.Context(), old.NodeID)
	require.NoError(t, err)
	newHash, newSize, err := f.blobs.Write(strings.NewReader("replacement original\n"))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), old.NodeID, node.Revision,
		newHash, newSize, "text/plain")
	require.NoError(t, err)
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowExportWrites: true}, f.lease)
	var members []bundle.Member
	for _, id := range []int64{old.NodeID, empty.NodeID} {
		result := exportCall(t, server, "list_document_versions", map[string]any{"node_id": id})
		var versions listDocumentVersionsOutput
		data, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &versions))
		for _, version := range versions.Items {
			members = append(members, bundle.Member{
				NodeID: version.NodeID, VersionID: version.ContentVersionID,
				SHA256: version.BlobHash, Size: version.Size,
			})
		}
	}
	require.Len(t, members, 3)
	// A newer head after selection must not substitute either retained version.
	node, err = f.catalog.NodeByID(t.Context(), old.NodeID)
	require.NoError(t, err)
	newHash, newSize, err = f.blobs.Write(strings.NewReader("unselected newer head\n"))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), old.NodeID, node.Revision,
		newHash, newSize, "text/plain")
	require.NoError(t, err)
	plan := previewNativeExport(t, server, members)
	jobID := startNativeExport(t, server, plan)
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	status := exportCall(t, server, "get_export_status", map[string]any{"job_id": jobID})
	require.Equal(t, "completed", objectField(t, status, "job")["state"])
	destination := filepath.Join(t.TempDir(), "originals.zip")
	args := map[string]any{"job_id": jobID, "destination_path": destination}
	result := exportCall(t, server, "download_export", args)
	require.Equal(t, "published", result["state"])
	require.Equal(t, false, result["cleanup_failed"])
	archive, err := os.Open(destination)
	require.NoError(t, err)
	info, err := archive.Stat()
	require.NoError(t, err)
	verified, err := bundle.Verify(t.Context(), archive, info.Size(), plan.Fingerprint)
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.Equal(t, verified.SHA256, objectField(t, result, "receipt")["sha256"])
	z, err := zip.OpenReader(destination)
	require.NoError(t, err)
	var originals []string
	for _, entry := range z.File {
		if filepath.Base(entry.Name) != "original" {
			continue
		}
		r, err := entry.Open()
		require.NoError(t, err)
		content, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		originals = append(originals, string(content))
	}
	require.NoError(t, z.Close())
	require.ElementsMatch(t, []string{"synthetic original\n", "replacement original\n", ""}, originals)
	before, err := os.ReadFile(destination)
	require.NoError(t, err)
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "download_export", "arguments": args,
	}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
	after, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, before, after)
	args["overwrite"] = true
	require.Equal(t, "published", exportCall(t, server, "download_export", args)["state"])
	_, err = f.catalog.ExportJob(t.Context(), "master", jobID)
	require.NoError(t, err, "download keeps the job")
	file, _, release, err := f.worker.Lease(t.Context(), "master", jobID)
	require.NoError(t, err)
	jobArgs := map[string]any{"job_id": jobID}
	require.Equal(t, "export_retained", exportCall(t, server, "release_export", jobArgs)["code"])
	require.NoError(t, file.Close())
	release()
	require.Equal(t, true, exportCall(t, server, "release_export", jobArgs)["released"])
	_, err = f.catalog.ExportJob(t.Context(), "master", jobID)
	require.ErrorIs(t, err, store.ErrNotFound)
	after, err = os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, before, after, "release preserves local file")
}

func nativeExportArchive(t *testing.T) (bundle.Job, []byte) {
	t.Helper()
	f := newNativeExportFixture(t)
	member := f.addFile(t, "synthetic.txt", "synthetic original\n")
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowExportWrites: true}, f.lease)
	plan := previewNativeExport(t, server, []bundle.Member{member})
	id := startNativeExport(t, server, plan)
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	job, err := f.catalog.ExportJob(t.Context(), "master", id)
	require.NoError(t, err)
	file, _, release, err := f.worker.Lease(t.Context(), "master", id)
	require.NoError(t, err)
	archive, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	release()
	return job, archive
}

const nativeExportTicket = "synthetic-native-export-ticket"

func exportArchiveHandler(t *testing.T, job bundle.Job, archive []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/exports/jobs/"+job.ID+"/download":
			assert.NoError(t, json.MarshalWrite(w, apiclient.TicketOutputBody{
				URL: "/api/daemon/web-download/file?ticket=" + nativeExportTicket, Receipt: *job.Receipt,
			}))
		case r.Method == http.MethodGet && r.URL.Path == "/api/daemon/web-download/file":
			_, _ = w.Write(archive)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/exports/jobs/"+job.ID:
			assert.NoError(t, json.MarshalWrite(w, job))
		default:
			t.Errorf("unexpected export side effect: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func TestNativeExportDownloadFailures(t *testing.T) {
	job, archive := nativeExportArchive(t)
	for _, mode := range []string{
		"unfinished", "ticket receipt", "corrupt", "corrupt and cleanup", "disconnect",
	} {
		t.Run(mode, func(t *testing.T) {
			selected := job
			body := bytes.Clone(archive)
			want := "export_integrity"
			if mode == "unfinished" {
				selected.State, want = "running", "export_conflict"
			}
			if mode == "corrupt" || mode == "corrupt and cleanup" {
				index := bytes.Index(body, []byte("synthetic original"))
				require.NotEqual(t, -1, index)
				body[index] ^= 1
			}
			if mode == "corrupt and cleanup" {
				cleanup := cleanupExportStage
				t.Cleanup(func() { cleanupExportStage = cleanup })
				cleanupExportStage = func(stage *filepublish.Stage) error {
					return errors.Join(cleanup(stage), errors.New("synthetic cleanup failure"))
				}
			}
			serve := exportArchiveHandler(t, selected, body)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "ticket receipt" && strings.HasSuffix(r.URL.Path, "/download") {
					ticket := apiclient.TicketOutputBody{
						URL: "/api/daemon/web-download/file?ticket=" + nativeExportTicket, Receipt: *job.Receipt,
					}
					ticket.Receipt.Entries++
					w.Header().Set("Content-Type", "application/json")
					assert.NoError(t, json.MarshalWrite(w, ticket))
					return
				}
				if mode == "disconnect" && r.URL.Path == "/api/daemon/web-download/file" {
					w.Header().Set("Content-Length", "999999")
					_, _ = w.Write(body[:32])
					_ = http.NewResponseController(w).Flush()
					c, _, err := http.NewResponseController(w).Hijack()
					if assert.NoError(t, err) {
						_ = c.Close()
					}
					return
				}
				serve(w, r)
			}))
			t.Cleanup(daemon.Close)
			var diagnostics bytes.Buffer
			var acquired atomic.Int32
			lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
				acquired.Add(1)
				connection := daemonconn.New(daemon.URL, "synthetic-export-key")
				t.Cleanup(func() { require.NoError(t, connection.Close()) })
				return connection, nil
			}, func(c *daemonconn.Connection) error { return c.Close() })
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{
				AllowExportWrites: true, Logger: slog.New(slog.NewTextHandler(&diagnostics, nil)),
			}, lease)
			dir := t.TempDir()
			destination := filepath.Join(dir, "export.zip")
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": "download_export", "arguments": map[string]any{
					"job_id": job.ID, "destination_path": destination,
				},
			}))
			if mode == "disconnect" {
				require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
				require.EqualValues(t, 1, acquired.Load(), "no automatic download retry")
				exportCall(t, server, "get_export_status", map[string]any{"job_id": job.ID})
				require.EqualValues(t, 2, acquired.Load(), "discard the failed transfer connection")
			} else {
				result := decodeResult(t, raw)
				require.Equal(t, true, result["isError"])
				require.Equal(t, want, objectField(t, result, "structuredContent")["code"])
			}
			require.NotContains(t, string(raw), nativeExportTicket)
			require.NotContains(t, diagnostics.String(), nativeExportTicket)
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries, "ordinary failure removes its stage and does not publish")
		})
	}
}

func TestNativeExportDownloadPublication(t *testing.T) {
	job, archive := nativeExportArchive(t)
	daemon := httptest.NewServer(exportArchiveHandler(t, job, archive))
	t.Cleanup(daemon.Close)
	for _, mode := range []string{
		"destination race", "directory sync", "cleanup", "primary and cleanup",
	} {
		t.Run(mode, func(t *testing.T) {
			publish, cleanup := publishExportFile, cleanupExportStage
			t.Cleanup(func() { publishExportFile, cleanupExportStage = publish, cleanup })
			destination := filepath.Join(t.TempDir(), "originals.zip")
			publishExportFile = func(stage, path string, overwrite bool) (bool, error) {
				if mode == "destination race" || mode == "primary and cleanup" {
					require.NoError(t, os.WriteFile(path, []byte("concurrent destination"), 0o600))
				}
				published, err := publish(stage, path, overwrite)
				if mode == "directory sync" && published && err == nil {
					return true, errors.New("synthetic directory sync failure")
				}
				return published, err
			}
			if mode == "cleanup" || mode == "primary and cleanup" {
				cleanupExportStage = func(stage *filepublish.Stage) error {
					return errors.Join(cleanup(stage), errors.New("synthetic cleanup failure"))
				}
			}
			var diagnostics bytes.Buffer
			server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{
				AllowExportWrites: true, Logger: slog.New(slog.NewTextHandler(&diagnostics, nil)),
			}, exportTestLease(t, daemon.URL))
			result := exportCall(t, server, "download_export", map[string]any{
				"job_id": job.ID, "destination_path": destination,
			})
			if mode == "cleanup" || mode == "primary and cleanup" {
				require.Contains(t, diagnostics.String(), "synthetic cleanup failure")
			}
			content, err := os.ReadFile(destination)
			require.NoError(t, err)
			if mode == "destination race" || mode == "primary and cleanup" {
				require.Equal(t, "export_local_io", result["code"])
				require.Contains(t, diagnostics.String(), "no-replace rename publication:")
				encoded, err := json.Marshal(result)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), destination)
				require.NotContains(t, string(encoded), "publication:")
				require.NotContains(t, string(encoded), "synthetic cleanup failure")
				require.Equal(t, "concurrent destination", string(content))
				return
			}
			require.Equal(t, archive, content)
			require.Equal(t, job.Receipt.SHA256, objectField(t, result, "receipt")["sha256"])
			require.Equal(t, mode == "cleanup", result["cleanup_failed"])
			if mode == "directory sync" {
				require.Equal(t, "published_durability_unknown", result["state"])
				require.Contains(t, diagnostics.String(), "synthetic directory sync failure")
			} else {
				require.Equal(t, "published", result["state"])
			}
		})
	}
}
