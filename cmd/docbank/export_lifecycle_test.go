package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
	"uuid"
)

func TestExportLifecycleCLIRejectsUnknownAndOversizedRequestBeforeDaemon(t *testing.T) {
	unknown := filepath.Join(t.TempDir(), "unknown.json")
	require.NoError(t, os.WriteFile(unknown, []byte(`{"operation_id":"synthetic","unexpected":true}`), 0o600))
	_, err := runCLI(t, "export", "source", "--input", unknown)
	require.Error(t, err)
	large := filepath.Join(t.TempDir(), "large.json")
	require.NoError(t, os.WriteFile(large, bytes.Repeat([]byte("x"), maxExportRequestBytes+1), 0o600))
	_, err = runCLI(t, "export", "source", "--input", large)
	require.ErrorContains(t, err, "64 MiB")
	_, err = runCLI(t, "export", "status", "not-a-job")
	require.ErrorContains(t, err, "canonical UUIDv4")
}

func TestExportLifecycleCLIProducesFrozenPDFArchive(t *testing.T) {
	_ = setupVaultHome(t)
	sourcePath := writeSourceFile(t, "synthetic.pdf", statMetadataPDF())
	_, err := runCLI(t, "add", sourcePath, "--dest", "/")
	require.NoError(t, err)
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := connection.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{
		Query: &apiclient.ResolvePathQuery{Path: "/synthetic.pdf"},
	})
	require.NoError(t, err)
	input := func(name string, request any) string {
		t.Helper()
		data, marshalErr := json.Marshal(request)
		require.NoError(t, marshalErr)
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		return path
	}
	sourceInput := input("source.json", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit",
		Members: []bundle.Member{{NodeID: node.ID, VersionID: node.CurrentVersionID,
			SHA256: node.BlobHash, Size: node.Size}}})
	output, err := runCLI(t, "export", "source", "--input", sourceInput)
	require.NoError(t, err, output)
	var source bundle.Source
	require.NoError(t, json.Unmarshal([]byte(output), &source))
	require.NotEmpty(t, source.MemberHash)

	planInput := input("plan.json", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID,
		MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	output, err = runCLI(t, "export", "plan", "--input", planInput)
	require.NoError(t, err, output)
	var plan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(output), &plan))
	require.NotEmpty(t, plan.Fingerprint)
	output, err = runCLI(t, "export", "preview", plan.ID)
	require.NoError(t, err, output)
	var preview bundle.PlanPreview
	require.NoError(t, json.Unmarshal([]byte(output), &preview))
	require.Equal(t, plan.Fingerprint, preview.Fingerprint)

	jobInput := input("job.json", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: plan.ID,
		Fingerprint: plan.Fingerprint})
	output, err = runCLI(t, "export", "start", "--input", jobInput)
	require.NoError(t, err, output)
	var job bundle.ExportJob
	require.NoError(t, json.Unmarshal([]byte(output), &job))
	require.NotEmpty(t, job.ID)
	require.Eventually(t, func() bool {
		status, statusErr := runCLI(t, "export", "status", job.ID)
		if statusErr != nil {
			return false
		}
		var current bundle.ExportJob
		return json.Unmarshal([]byte(status), &current) == nil && current.State == "completed"
	}, 15*time.Second, 100*time.Millisecond)
	archive := filepath.Join(t.TempDir(), "native-pdf.zip")
	output, err = runCLI(t, "export", "archive", job.ID, "--output", archive)
	require.NoError(t, err, output)
	file, err := os.Open(archive)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	info, err := file.Stat()
	require.NoError(t, err)
	receipt, err := bundle.Verify(t.Context(), file, info.Size(), plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, plan.Fingerprint, receipt.PlanFingerprint)

	// The same exact retained PDF can be selected for a frozen report. Its
	// original bytes stay in the export; the report records its search coverage.
	reportInput := input("report.json", report.Request{Version: 2,
		SelectedDocuments: []report.Identity{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash}},
		Timezone:          "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "synthetic", Syntax: "simple",
			Dates: report.DateRange{Start: "2000-01-01", End: "2100-12-31"}}}})
	reportPath := filepath.Join(t.TempDir(), "native-pdf-report.zip")
	output, err = runCLI(t, "search-export", "create", "--input", reportInput, "--output", reportPath)
	require.NoError(t, err, output)
	reportID := regexp.MustCompile(`export ([0-9a-f]{48})`).FindStringSubmatch(output)
	require.Len(t, reportID, 2, output)
	summary, err := connection.GetTermReport(t.Context(), reportID[1])
	require.NoError(t, err)
	require.Equal(t, "complete", summary.State)
	require.Equal(t, int64(1), summary.Coverage.Scoped)
	reportFile, err := os.Open(reportPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reportFile.Close()) }()
	reportInfo, err := reportFile.Stat()
	require.NoError(t, err)
	budget := report.NewBudget(report.DefaultBudgetBytes)
	defer func() { require.NoError(t, budget.Close()) }()
	_, err = report.VerifyBundle(t.Context(), budget, reportFile, reportInfo.Size())
	require.NoError(t, err)
	csvPath := filepath.Join(t.TempDir(), "native-pdf-report.csv")
	_, err = runCLI(t, "search-export", "download", reportID[1], "--format", "csv", "--output", csvPath)
	require.NoError(t, err)
	csv, err := os.ReadFile(csvPath)
	require.NoError(t, err)
	require.Contains(t, string(csv), "synthetic")
}
