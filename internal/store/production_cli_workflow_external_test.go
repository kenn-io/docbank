package store_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

// This opt-in proof drives the real CLI and daemon over a retained synthetic
// PDF. It exercises CLI reads, finalization, rendering, publication, and download.
func TestProductionCLIRealDaemonFinalizesAndDownloadsPackage(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_CLI_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon production CLI workflow proof")
	}
	vault, root, setID, revision, etag, namespaceID := store.ProductionRenderDaemonHTTPFixture(t)
	members, _, err := vault.ProductionMembers(t.Context(), setID, revision, "", 200)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	require.NoError(t, vault.Close())
	name := "docbank"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", binary,
		"go.kenn.io/docbank/cmd/docbank")
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, stopErr := daemonconn.Stop(ctx, root)
		require.NoError(t, stopErr)
	})
	cli := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, args...)
		command.Env = append(os.Environ(), "DOCBANK_HOME="+root)
		data, runErr := command.CombinedOutput()
		require.NoError(t, runErr, "%v: %s", args, data)
		return string(data)
	}
	revisionText := strconv.FormatInt(revision, 10)
	etagText := strconv.FormatInt(etag, 10)
	var draft redaction.Draft
	require.NoError(t, json.Unmarshal([]byte(cli("production", "drafts", "show", setID,
		revisionText, "--json")), &draft))
	require.Equal(t, etag, draft.ETag)
	var memberPage api.ProductionMemberPage
	require.NoError(t, json.Unmarshal([]byte(cli("production", "drafts", "members", setID,
		revisionText, "--limit", "1", "--json")), &memberPage))
	require.Len(t, memberPage.Items, 1)
	require.NotEmpty(t, memberPage.NextCursor)
	memberID := memberPage.Items[0].ID
	var secondMemberPage api.ProductionMemberPage
	require.NoError(t, json.Unmarshal([]byte(cli("production", "drafts", "members", setID,
		revisionText, "--limit", "1", "--cursor", memberPage.NextCursor, "--json")), &secondMemberPage))
	require.Len(t, secondMemberPage.Items, 1)
	require.NotEqual(t, memberID, secondMemberPage.Items[0].ID)
	require.Empty(t, secondMemberPage.NextCursor)
	var decisionPage api.ProductionDecisionPage
	require.NoError(t, json.Unmarshal([]byte(cli("production", "drafts", "decisions", setID,
		revisionText, "--limit", "1", "--json")), &decisionPage))
	require.Len(t, decisionPage.Items, 1)
	require.NotEmpty(t, decisionPage.NextCursor)
	var secondDecisionPage api.ProductionDecisionPage
	require.NoError(t, json.Unmarshal([]byte(cli("production", "drafts", "decisions", setID,
		revisionText, "--limit", "1", "--cursor", decisionPage.NextCursor, "--json")), &secondDecisionPage))
	require.Len(t, secondDecisionPage.Items, 1)
	require.NotEqual(t, decisionPage.Items[0].ID, secondDecisionPage.Items[0].ID)
	require.Empty(t, secondDecisionPage.NextCursor)
	var resolved api.ProductionResolvedMaskPage
	require.NoError(t, json.Unmarshal([]byte(cli("production", "drafts", "resolve", setID,
		revisionText, memberID, "1", "--etag", etagText, "--limit", "1", "--json")), &resolved))
	require.Equal(t, memberID, resolved.MemberID)
	require.Equal(t, members[0].ReviewBinding, resolved.ReviewBinding)

	const finalizeOperation = "79000000-0000-4000-8000-000000000061"
	const snapshotID = "79000000-0000-4000-8000-000000000062"
	finalizeArgs := []string{"production", "drafts", "finalize", setID, revisionText, "--etag", etagText,
		"--namespace-id", namespaceID, "--snapshot-id", snapshotID,
		"--operation-id", finalizeOperation, "--json"}
	finalizedJSON := cli(finalizeArgs...)
	var finalized api.ProductionFinalizationResult
	require.NoError(t, json.Unmarshal([]byte(finalizedJSON), &finalized))
	require.Equal(t, "finalized", finalized.Draft.State)
	require.JSONEq(t, finalizedJSON, cli(finalizeArgs...))

	const jobID = "79000000-0000-4000-8000-000000000063"
	const admitOperation = "79000000-0000-4000-8000-000000000064"
	admitArgs := []string{"production", "jobs", "admit", setID, revisionText, "--etag", etagText,
		"--job-id", jobID, "--operation-id", admitOperation, "--json"}
	admittedJSON := cli(admitArgs...)
	var admitted api.ProductionJobStatus
	require.NoError(t, json.Unmarshal([]byte(admittedJSON), &admitted))
	require.Equal(t, jobID, admitted.JobID)
	require.JSONEq(t, admittedJSON, cli(admitArgs...))
	deadline := time.Now().Add(2 * time.Minute)
	var status api.ProductionJobStatus
	for {
		require.NoError(t, json.Unmarshal([]byte(cli("production", "jobs", "status", setID, jobID,
			"--json")), &status))
		if status.State == "succeeded" || status.State == "failed" || status.State == "canceled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("production job did not finish: last state %q", status.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Equal(t, "succeeded", status.State)
	require.NotEmpty(t, status.ReceiptSHA256)

	const publishOperation = "79000000-0000-4000-8000-000000000065"
	publishArgs := []string{"production", "packages", "publish", jobID,
		"--profile", "export-dat-opt-images-v1", "--max-volume-bytes", "52428800",
		"--max-volume-documents", "10", "--operation-id", publishOperation, "--json"}
	publishedJSON := cli(publishArgs...)
	var published api.ProductionPackagePublished
	require.NoError(t, json.Unmarshal([]byte(publishedJSON), &published))
	require.Equal(t, publishOperation, published.OperationID)
	require.JSONEq(t, publishedJSON, cli(publishArgs...))

	destination := filepath.Join(t.TempDir(), "synthetic-package.zip")
	downloadJSON := cli("production", "packages", "download", jobID, publishOperation,
		destination, "--json")
	var downloaded struct {
		Path          string `json:"path"`
		ArchiveSHA256 string `json:"archive_sha256"`
		Size          int64  `json:"size"`
		VersionID     string `json:"version_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(downloadJSON), &downloaded))
	require.NotContains(t, downloadJSON, "url")
	require.Equal(t, destination, downloaded.Path)
	require.Equal(t, published.ArchiveSHA256, downloaded.ArchiveSHA256)
	require.Equal(t, published.VersionID, downloaded.VersionID)
	archive, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, published.Size, int64(len(archive)))
	sum := sha256.Sum256(archive)
	require.Equal(t, published.ArchiveSHA256, hex.EncodeToString(sum[:]))
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	require.NotEmpty(t, reader.File)
}
