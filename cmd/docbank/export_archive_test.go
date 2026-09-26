package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func TestExportArchiveDownloadPublishesOnlyVerifiedBundle(t *testing.T) {
	jobID := uuid.New().String()
	body := []byte("synthetic PDF bytes")
	hash := sha256.Sum256(body)
	versionID := uuid.New().String()
	document := bundle.Document{NodeID: 2, VersionID: versionID, SHA256: hex.EncodeToString(hash[:]),
		Size: int64(len(body)), Name: "synthetic.pdf", Path: "/synthetic.pdf", MediaType: "application/pdf"}
	document.Roles = []bundle.Role{{Role: "original", Status: "available",
		Path: "documents/2/" + versionID + "/original", SHA256: document.SHA256, Size: document.Size}}
	plan := bundle.Plan{Format: bundle.Format, ID: uuid.New().String(), VaultID: uuid.New().String(),
		Toolchain: "synthetic", Total: 1, RoleEntries: 1, RoleBytes: document.Size,
		Roles: []bundle.RolePolicy{{Role: "original"}}}
	walk := func(visit func(bundle.Document) error) error { return visit(document) }
	var err error
	plan.Fingerprint, err = bundle.Fingerprint(plan, walk)
	require.NoError(t, err)
	file, err := os.Create(filepath.Join(t.TempDir(), "source.zip"))
	require.NoError(t, err)
	receipt, err := bundle.Write(t.Context(), file, plan, walk,
		func(bundle.Role) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }, nil)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	archive, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	corrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/api/v1/exports/jobs/" + jobID + "/archive"; r.URL.Path != want {
			t.Errorf("archive path = %q, want %q", r.URL.Path, want)
			http.Error(w, "wrong archive path", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		w.Header().Set("Docbank-Archive-Sha256", receipt.SHA256)
		w.Header().Set("Docbank-Plan-Fingerprint", plan.Fingerprint)
		if corrupt {
			changed := bytes.Clone(archive)
			changed[0] ^= 1
			_, _ = w.Write(changed)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	connection := daemonconn.New(server.URL, "synthetic-key")
	output := filepath.Join(t.TempDir(), "download.zip")
	receipt, err = downloadExportArchive(t.Context(), connection, jobID, output, false)
	require.NoError(t, err)
	require.Equal(t, plan.Fingerprint, receipt.PlanFingerprint)
	got, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, archive, got)

	corrupt = true
	missing := filepath.Join(t.TempDir(), "not-published.zip")
	_, err = downloadExportArchive(t.Context(), connection, jobID, missing, false)
	require.ErrorContains(t, err, "digest")
	_, statErr := os.Stat(missing)
	require.True(t, os.IsNotExist(statErr))
	_, err = downloadExportArchive(t.Context(), connection, strings.Repeat("x", 36), missing, false)
	require.Error(t, err)
}

func TestExportArchiveCLIUsesLiveDaemonAndDeniesWithdrawnSource(t *testing.T) {
	_ = setupVaultHome(t)
	sourcePath := writeSourceFile(t, "synthetic.pdf", statMetadataPDF())
	_, err := runCLI(t, "add", sourcePath, "--dest", "/")
	require.NoError(t, err)
	connection, err := daemonconn.Ensure(context.Background())
	require.NoError(t, err)
	api := connection.API()
	node, err := api.ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{
		Query: &apiclient.ResolvePathQuery{Path: "/synthetic.pdf"}})
	require.NoError(t, err)
	source, err := api.CreateExportSource(t.Context(), &apiclient.CreateExportSourceRequestOptions{
		Body: &bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit",
			Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID,
				SHA256: node.BlobHash, Size: node.Size}}}})
	require.NoError(t, err)
	plan, err := api.CreateExportPlan(t.Context(), &apiclient.CreateExportPlanRequestOptions{
		Body: &bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID,
			MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}}})
	require.NoError(t, err)
	job, err := api.CreateExportJob(t.Context(), &apiclient.CreateExportJobRequestOptions{
		Body: &bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID,
			Fingerprint: plan.Fingerprint}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		current, readErr := api.GetExportJob(t.Context(), &apiclient.GetExportJobRequestOptions{
			PathParams: &apiclient.GetExportJobPath{ID: job.ID}})
		return readErr == nil && current.State == "completed"
	}, 15*time.Second, 100*time.Millisecond)
	output := filepath.Join(t.TempDir(), "native-pdf-export.zip")
	message, err := runCLI(t, "export", "archive", job.ID, "--output", output)
	require.NoError(t, err, message)
	require.Contains(t, message, plan.Fingerprint)
	file, err := os.Open(output)
	require.NoError(t, err)
	info, err := file.Stat()
	require.NoError(t, err)
	verified, err := bundle.Verify(t.Context(), file, info.Size(), plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, job.Fingerprint, verified.PlanFingerprint)
	require.NoError(t, file.Close())

	_, err = runCLI(t, "rm", "/synthetic.pdf")
	require.NoError(t, err)
	withdrawn := filepath.Join(t.TempDir(), "withdrawn.zip")
	_, err = runCLI(t, "export", "archive", job.ID, "--output", withdrawn)
	require.Error(t, err)
	_, statErr := os.Stat(withdrawn)
	require.True(t, os.IsNotExist(statErr))
}
