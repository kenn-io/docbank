package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/mcp"
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
	home := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "backup-repo")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(
		"[backup]\nrepo = \""+filepath.ToSlash(repoPath)+"\"\n"), 0o600))
	t.Setenv("DOCBANK_HOME", home)
	startTestDaemon(t, home)
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
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	mcpContext, stopMCP := context.WithCancel(t.Context())
	mcpDone := make(chan error, 1)
	go func() { mcpDone <- mcp.NewServer().Run(mcpContext, serverTransport) }()
	t.Cleanup(func() {
		stopMCP()
		<-mcpDone
	})
	mcpClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-export-client"}, nil)
	mcpSession, err := mcpClient.Connect(mcpContext, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mcpSession.Close() })
	callMCP := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result, callErr := mcpSession.CallTool(mcpContext, &sdkmcp.CallToolParams{Name: name, Arguments: args})
		require.NoError(t, callErr)
		require.False(t, result.IsError)
		encoded, encodeErr := json.Marshal(result.StructuredContent)
		require.NoError(t, encodeErr)
		var output map[string]any
		require.NoError(t, json.Unmarshal(encoded, &output))
		return output
	}
	mcpPreview := callMCP("preview_export_plan", map[string]any{"plan_id": plan.ID})
	require.Equal(t, preview.Fingerprint, mcpPreview["fingerprint"])
	require.Equal(t, preview.MemberHash, mcpPreview["member_hash"])

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
	mcpJob := callMCP("get_export_job", map[string]any{"job_id": job.ID})
	require.Equal(t, "completed", mcpJob["state"])
	require.Equal(t, plan.Fingerprint, mcpJob["fingerprint"])
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
	reportRequest := report.Request{Version: 2,
		SelectedDocuments: []report.Identity{{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash}},
		Timezone:          "UTC", CoverageMode: "available_only",
		Terms: []report.Term{{Number: 1, Expression: "synthetic", Syntax: "simple",
			Dates: report.DateRange{Start: "2000-01-01", End: "2100-12-31"}}}}
	reportInput := input("report.json", reportRequest)
	reportPath := filepath.Join(t.TempDir(), "native-pdf-report.zip")
	output, err = runCLI(t, "search-export", "create", "--input", reportInput, "--output", reportPath)
	require.NoError(t, err, output)
	reportID := regexp.MustCompile(`export ([0-9a-f]{48})`).FindStringSubmatch(output)
	require.Len(t, reportID, 2, output)
	summary, err := connection.GetTermReport(t.Context(), reportID[1])
	require.NoError(t, err)
	require.Equal(t, "complete", summary.State)
	require.Equal(t, int64(1), summary.Coverage.Scoped)
	mcpSummary := callMCP("get_report_summary", map[string]any{"report_id": reportID[1]})
	require.Equal(t, summary.ID, mcpSummary["id"])
	require.Equal(t, summary.BundleSHA256, mcpSummary["bundle_sha256"])
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
	_, err = runCLI(t, "backup", "init")
	require.NoError(t, err)
	output, err = runCLI(t, "backup", "create", "--tag", "native-pdf-workflow", "--json")
	require.NoError(t, err, output)
	var snapshot api.BackupSnapshot
	require.NoError(t, json.Unmarshal([]byte(output), &snapshot))
	require.NotEmpty(t, snapshot.ID)
	output, err = runCLI(t, "backup", "verify", snapshot.ID, "--json")
	require.NoError(t, err, output)
	var backupProof api.BackupVerifyReport
	require.NoError(t, json.Unmarshal([]byte(output), &backupProof))
	require.Empty(t, backupProof.Problems)

	restoredHome := filepath.Join(t.TempDir(), "restored")
	output, err = runCLI(t, "backup", "restore", snapshot.ID, "--target", restoredHome, "--json")
	require.NoError(t, err, output)
	var restoreProof api.BackupRestoreReport
	require.NoError(t, json.Unmarshal([]byte(output), &restoreProof))
	require.True(t, restoreProof.Proof.ContentVerified)
	require.True(t, restoreProof.Proof.SQLiteIntegrity)
	require.True(t, restoreProof.Proof.ManifestStats)

	t.Setenv("DOCBANK_HOME", restoredHome)
	startTestDaemon(t, restoredHome)
	restoredConnection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	_, err = restoredConnection.GetTermReport(t.Context(), reportID[1])
	require.ErrorContains(t, err, "report_unavailable", "a cache handle is not a retained artifact")
	history, err := restoredConnection.API().ListTermReportHistory(t.Context(), &apiclient.ListTermReportHistoryRequestOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, history.Items)
	require.Equal(t, reportRequest.SelectedDocuments, history.Items[0].Request.SelectedDocuments)
	require.Equal(t, "complete", history.Items[0].Summary.State)
	require.Equal(t, summary.Counts, history.Items[0].Summary.Counts)
	restoredSummary, err := restoredConnection.CreateTermReport(t.Context(), history.Items[0].Request)
	require.NoError(t, err)
	require.Equal(t, summary.Counts, restoredSummary.Counts)
	restoredCSV := filepath.Join(t.TempDir(), "restored-report.csv")
	_, err = runCLI(t, "search-export", "download", restoredSummary.ID, "--format", "csv", "--output", restoredCSV)
	require.NoError(t, err)
	restoredCSVBytes, err := os.ReadFile(restoredCSV)
	require.NoError(t, err)
	require.Equal(t, csv, restoredCSVBytes)
	_, err = runCLI(t, "export", "archive", job.ID, "--output", filepath.Join(t.TempDir(), "expired-job.zip"))
	require.ErrorContains(t, err, "not found", "a completed job is not a retained archive after restore")
	restoredNode, err := restoredConnection.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{
		Query: &apiclient.ResolvePathQuery{Path: "/synthetic.pdf"},
	})
	require.NoError(t, err)
	require.Equal(t, node.CurrentVersionID, restoredNode.CurrentVersionID)
	require.Equal(t, node.BlobHash, restoredNode.BlobHash)
	restoredSourceInput := input("restored-source.json", bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit",
		Members: []bundle.Member{{NodeID: restoredNode.ID, VersionID: restoredNode.CurrentVersionID,
			SHA256: restoredNode.BlobHash, Size: restoredNode.Size}}})
	output, err = runCLI(t, "export", "source", "--input", restoredSourceInput)
	require.NoError(t, err, output)
	var restoredSource bundle.Source
	require.NoError(t, json.Unmarshal([]byte(output), &restoredSource))
	restoredPlanInput := input("restored-plan.json", bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: restoredSource.ID,
		MemberHash: restoredSource.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	output, err = runCLI(t, "export", "plan", "--input", restoredPlanInput)
	require.NoError(t, err, output)
	var restoredPlan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(output), &restoredPlan))
	restoredJobInput := input("restored-job.json", bundle.JobRequest{OperationID: uuid.New().String(), PlanID: restoredPlan.ID,
		Fingerprint: restoredPlan.Fingerprint})
	output, err = runCLI(t, "export", "start", "--input", restoredJobInput)
	require.NoError(t, err, output)
	var restoredJob bundle.ExportJob
	require.NoError(t, json.Unmarshal([]byte(output), &restoredJob))
	require.Eventually(t, func() bool {
		status, statusErr := runCLI(t, "export", "status", restoredJob.ID)
		if statusErr != nil {
			return false
		}
		var current bundle.ExportJob
		return json.Unmarshal([]byte(status), &current) == nil && current.State == "completed"
	}, 15*time.Second, 100*time.Millisecond)
	restoredArchive := filepath.Join(t.TempDir(), "restored-pdf.zip")
	_, err = runCLI(t, "export", "archive", restoredJob.ID, "--output", restoredArchive)
	require.NoError(t, err)
	restoredArchiveFile, err := os.Open(restoredArchive)
	require.NoError(t, err)
	defer func() { require.NoError(t, restoredArchiveFile.Close()) }()
	restoredArchiveInfo, err := restoredArchiveFile.Stat()
	require.NoError(t, err)
	restoredReceipt, err := bundle.Verify(t.Context(), restoredArchiveFile, restoredArchiveInfo.Size(), restoredPlan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, restoredPlan.Fingerprint, restoredReceipt.PlanFingerprint)
}
