package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestExportArchiveDownloadToolsAreReadOnlyByDefault(t *testing.T) {
	tools := catalogNames(toolCatalog(false, false))
	require.Contains(t, tools, "open_export_archive")
	require.Contains(t, tools, "download_export_archive")
	require.Contains(t, catalogNames(toolCatalog(false, false, true)), "open_export_archive")
}

func TestExportArchiveRegistryBoundsProcessSpoolCapacity(t *testing.T) {
	registry := newExportArchiveRegistry()
	require.ErrorIs(t, registry.reserve(maxMCPExportArchiveBytes+1), errExportArchiveCapacity)
	require.NoError(t, registry.reserve(maxMCPExportArchiveBytes))
	require.ErrorIs(t, registry.reserve(1), errExportArchiveCapacity)
	registry.abort(maxMCPExportArchiveBytes)
	require.NoError(t, registry.reserve(1))
	registry.abort(1)
	registry.closeAll()
	require.ErrorIs(t, registry.reserve(1), errExportArchiveHandle)
}

func TestMCPExportArchiveVerifiesBeforeHandleAndRechecksVisibilityPerChunk(t *testing.T) {
	const jobID = "33333333-3333-4333-8333-333333333333"
	archive, receipt := syntheticMCPArchive(t)
	var withdrawn atomic.Bool
	var tamperMode atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-key" {
			t.Errorf("export archive request missing owner credential")
			http.Error(w, "missing owner credential", http.StatusUnauthorized)
			return
		}
		advertised := receipt
		if tamperMode.Load() == 2 {
			changed := bytes.Clone(archive)
			changed[len(changed)/2] ^= 1
			digest := sha256.Sum256(changed)
			advertised.SHA256 = hex.EncodeToString(digest[:])
		}
		switch r.URL.Path {
		case "/api/v1/exports/jobs/" + jobID + "/archive/authority":
			if withdrawn.Load() {
				http.Error(w, `{"code":"visibility_changed"}`, http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, advertised); err != nil {
				t.Errorf("write synthetic export receipt: %v", err)
			}
		case "/api/v1/exports/jobs/" + jobID + "/archive":
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", strconv.FormatInt(receipt.Size, 10))
			w.Header().Set("Docbank-Archive-Sha256", advertised.SHA256)
			w.Header().Set("Docbank-Plan-Fingerprint", advertised.PlanFingerprint)
			if tamperMode.Load() != 0 {
				changed := bytes.Clone(archive)
				changed[len(changed)/2] ^= 1
				_, _ = w.Write(changed)
				return
			}
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	registry := newExportArchiveRegistry()
	t.Cleanup(registry.closeAll)
	input := []byte(`{"job_id":"` + jobID + `"}`)
	tamperMode.Store(1)
	_, err := openMCPExportArchive(t.Context(), lease, registry, input)
	require.Error(t, err)
	require.Empty(t, registry.spools, "tampered bytes must never earn a handle")
	tamperMode.Store(2)
	_, err = openMCPExportArchive(t.Context(), lease, registry, input)
	require.Error(t, err)
	require.Empty(t, registry.spools, "self-consistent transport headers cannot validate a corrupt ZIP")
	tamperMode.Store(0)
	opened, err := openMCPExportArchive(t.Context(), lease, registry, input)
	require.NoError(t, err)
	require.Equal(t, receipt.Size, opened.Size)
	require.Equal(t, receipt.SHA256, opened.SHA256)
	first, err := downloadMCPExportArchive(t.Context(), lease, registry,
		[]byte(`{"handle":"`+opened.Handle+`","offset":0,"max_bytes":64}`))
	require.NoError(t, err)
	decoded, err := base64.StdEncoding.DecodeString(first.DataBase64)
	require.NoError(t, err)
	require.Equal(t, archive[:64], decoded)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, lease)
	openedWire := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "open_export_archive", "arguments": map[string]any{"job_id": jobID},
	})))
	require.NotEqual(t, true, openedWire["isError"])
	issued := objectField(t, openedWire, "structuredContent")
	issuedHandle, ok := issued["handle"].(string)
	require.True(t, ok)
	chunkWire := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "download_export_archive", "arguments": map[string]any{
			"handle": issuedHandle, "offset": 0, "max_bytes": 64, "close": true,
		},
	})))
	require.NotEqual(t, true, chunkWire["isError"])
	require.Equal(t, true, objectField(t, chunkWire, "structuredContent")["closed"])
	withdrawn.Store(true)
	_, err = downloadMCPExportArchive(t.Context(), lease, registry,
		[]byte(`{"handle":"`+opened.Handle+`","offset":64,"max_bytes":64}`))
	require.Error(t, err)
	require.Empty(t, registry.spools, "withdrawal must destroy the private spool")
}

func syntheticMCPArchive(t *testing.T) ([]byte, bundle.Receipt) {
	t.Helper()
	body := []byte("synthetic PDF bytes\n")
	hash := sha256.Sum256(body)
	versionID := "44444444-4444-4444-8444-444444444444"
	doc := bundle.Document{NodeID: 7, VersionID: versionID, SHA256: hex.EncodeToString(hash[:]),
		Size: int64(len(body)), Name: "review.pdf", Path: "/synthetic/review.pdf", MediaType: "application/pdf"}
	doc.Roles = []bundle.Role{{Role: "original", Status: "available", Path: "documents/7/" + versionID + "/original",
		SHA256: doc.SHA256, Size: doc.Size}}
	plan := bundle.Plan{Format: bundle.Format, ID: "22222222-2222-4222-8222-222222222222",
		VaultID: "11111111-1111-4111-8111-111111111111", Toolchain: "synthetic", Total: 1,
		RoleEntries: 1, RoleBytes: doc.Size, Roles: []bundle.RolePolicy{{Role: "original"}}}
	walk := func(visit func(bundle.Document) error) error { return visit(doc) }
	var err error
	plan.Fingerprint, err = bundle.Fingerprint(plan, walk)
	require.NoError(t, err)
	file, err := os.Create(filepath.Join(t.TempDir(), "synthetic-export.zip"))
	require.NoError(t, err)
	receipt, err := bundle.Write(t.Context(), file, plan, walk,
		func(bundle.Role) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(string(body))), nil }, nil)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	data, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	return data, receipt
}
