package main

import (
	"archive/zip"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestExportPreflightDoesNotContactDaemon(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "absent-vault")
	t.Setenv("DOCBANK_HOME", vault)
	valid := `{"source_operation_id":"11111111-1111-4111-8111-111111111111","plan_operation_id":"22222222-2222-4222-8222-222222222222","members":[{"node_id":1,"version_id":"33333333-3333-4333-8333-333333333333","sha256":"` + strings.Repeat("a", 64) + `","size":0}]}`
	member := valid[strings.Index(valid, `[{`)+1 : len(valid)-2]
	for _, tc := range []struct{ name, body, field string }{
		{"malformed", "{", "JSON"},
		{"unknown", strings.Replace(valid, `"size":0`, `"unknown":0`, 1), "JSON"},
		{"source ID", strings.Replace(valid, "11111111-1111-4111-8111-111111111111", "bad", 1), "source_operation_id"},
		{"plan ID", strings.Replace(valid, "22222222-2222-4222-8222-222222222222", "bad", 1), "plan_operation_id"},
		{"node", strings.Replace(valid, `"node_id":1`, `"node_id":0`, 1), "members[0].node_id"},
		{"version", strings.Replace(valid, "33333333-3333-4333-8333-333333333333", "33333333-3333-1333-8333-333333333333", 1), "members[0].version_id"},
		{"hash", strings.Replace(valid, strings.Repeat("a", 64), strings.Repeat("A", 64), 1), "members[0].sha256"},
		{"size", strings.Replace(valid, `"size":0`, `"size":-1`, 1), "members[0].size"},
		{"revision", strings.Replace(valid, `"size":0`, `"revision":-1`, 1), "members[0].revision"},
		{"limit", strings.Replace(valid, `"size":0`, `"size":53687091201`, 1), "bytes"},
		{"empty", strings.Replace(valid, valid[strings.Index(valid, `[{`):len(valid)-1], "[]", 1), "members"},
		{"null list", strings.Replace(valid, "["+member+"]", "null", 1), "members"},
		{"duplicate", strings.Replace(valid, "["+member+"]", "["+member+","+member+"]", 1), "members[1]"},
		{"too many", strings.Replace(valid, "["+member+"]", "["+strings.Repeat(member+",", 1000)+member+"]", 1), "members"},
		{"oversized", strings.Repeat(" ", 1<<20+1), "1 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := writeSourceFile(t, "selection.json", tc.body)
			_, err := runCLI(t, "export", "preview", "--request", input)
			require.ErrorContains(t, err, tc.field)
			require.Equal(t, exitUsage, commandExitCode(err, true))
			require.NoDirExists(t, vault)
		})
	}
	for _, args := range [][]string{{"preview"}, {"start", "bad"}, {"status", "bad"}, {"cancel", "bad"}, {"release", "bad"}, {"download", "bad", "out.zip"}} {
		_, err := runCLI(t, append([]string{"export"}, args...)...)
		require.Equal(t, exitUsage, commandExitCode(err, true))
		require.NoDirExists(t, vault)
	}
}

func TestExportCLIHistoricalVersionsAndExplicitRelease(t *testing.T) {
	waitCLIExport := func(t *testing.T, id string) {
		t.Helper()
		var job bundle.Job
		require.Eventually(t, func() bool {
			out, err := runCLI(t, "export", "status", id, "--json")
			return err == nil && json.Unmarshal([]byte(out), &job) == nil && (job.State == "completed" || job.State == "failed")
		}, 30*time.Second, 25*time.Millisecond)
		require.Equal(t, "completed", job.State, job.Failure)
	}

	assertExportOriginals := func(t *testing.T, path string, old, current bundle.Member) {
		t.Helper()
		z, err := zip.OpenReader(path)
		require.NoError(t, err)
		defer func() { _ = z.Close() }()
		want := map[string]string{
			fmt.Sprintf("documents/%d/%s/original", old.NodeID, old.VersionID):         "original version\n",
			fmt.Sprintf("documents/%d/%s/original", current.NodeID, current.VersionID): "replacement version\n",
		}
		require.Len(t, z.File, 5)
		for _, f := range z.File {
			if !strings.HasPrefix(f.Name, "documents/") {
				continue
			}
			r, err := f.Open()
			require.NoError(t, err)
			content, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			require.Equal(t, want[f.Name], string(content))
			delete(want, f.Name)
		}
		require.Empty(t, want)
	}

	setupVaultHome(t)
	source := writeSourceFile(t, "synthetic.txt", "original version\n")
	_, err := runCLI(t, "add", source, "--dest", "/")
	require.NoError(t, err)
	client, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := client.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/synthetic.txt"}})
	require.NoError(t, err)
	old := bundle.Member{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}
	_, err = runCLI(t, "put", writeSourceFile(t, "replacement.txt", "replacement version\n"), "/synthetic.txt", "--progress", "plain")
	require.NoError(t, err)
	node, err = client.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/synthetic.txt"}})
	require.NoError(t, err)
	current := bundle.Member{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: node.BlobHash, Size: node.Size}
	_, err = runCLI(t, "add", writeSourceFile(t, "unselected.txt", "not selected\n"), "--dest", "/")
	require.NoError(t, err)
	request := exportPreviewRequest{SourceOperationID: uuid.New().String(), PlanOperationID: uuid.New().String(), Members: []bundle.Member{old, current}}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	input := writeSourceFile(t, "selection.json", string(encoded))
	output, err := runCLI(t, "export", "preview", "--request", input, "--json")
	require.NoError(t, err)
	var plan bundle.Plan
	require.NoError(t, json.Unmarshal([]byte(output), &plan))
	require.Equal(t, 2, plan.Total)
	require.Equal(t, []bundle.RolePolicy{{Role: "original"}}, plan.Roles)
	replayed, err := runCLI(t, "export", "preview", "--request", input, "--json")
	require.NoError(t, err)
	require.Equal(t, output, replayed)
	first, second, third := uuid.New().String(), uuid.New().String(), uuid.New().String()
	start := func(id string) (string, error) {
		return runCLI(t, "export", "start", plan.ID, "--fingerprint", plan.Fingerprint, "--operation-id", id, "--json")
	}
	for _, id := range []string{first, second} {
		_, err = start(id)
		require.NoError(t, err)
		waitCLIExport(t, id)
	}
	_, err = start(third)
	require.ErrorContains(t, err, "export_limit")
	_, err = start(first)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "native.zip")
	output, err = runCLI(t, "export", "download", first, destination, "--json")
	require.NoError(t, err)
	var receipt bundle.Receipt
	require.NoError(t, json.Unmarshal([]byte(output), &receipt))
	require.Equal(t, plan.Fingerprint, receipt.PlanFingerprint)
	assertExportOriginals(t, destination, old, current)
	_, err = runCLI(t, "export", "download", first, destination)
	require.ErrorContains(t, err, "--overwrite")
	_, err = runCLI(t, "export", "download", first, destination, "--overwrite")
	require.NoError(t, err)
	// A broken output stream must report that publication already succeeded.
	reader, writer := io.Pipe()
	require.NoError(t, reader.Close())
	defer func() { _ = writer.Close() }()
	cmd := &cobra.Command{}
	cmd.SetOut(writer)
	cmd.SetContext(t.Context())
	saved := filepath.Join(t.TempDir(), "saved.zip")
	err = downloadNativeExport(cmd, []string{first, saved})
	require.ErrorContains(t, err, "already saved")
	require.ErrorIs(t, err, io.ErrClosedPipe)
	assertExportOriginals(t, saved, old, current)
	stages, err := filepath.Glob(filepath.Join(filepath.Dir(saved), "docbank-native-export-*"))
	require.NoError(t, err)
	require.Empty(t, stages)
	_, err = runCLI(t, "export", "cancel", first)
	require.ErrorContains(t, err, "export_conflict")
	output = releaseCLIExport(t, first)
	require.JSONEq(t, fmt.Sprintf(`{"job_id":%q,"released":true}`, first), output)
	missing := filepath.Join(t.TempDir(), "missing.zip")
	_, err = runCLI(t, "export", "download", first, missing)
	require.Equal(t, exitNotFound, commandExitCode(err, true))
	require.NoFileExists(t, missing)
	entries, err := os.ReadDir(filepath.Dir(missing))
	require.NoError(t, err)
	require.Empty(t, entries)
	_, err = start(third)
	require.NoError(t, err)
	waitCLIExport(t, third)
	assertExportOriginals(t, destination, old, current)
}

func releaseCLIExport(t *testing.T, id string) string {
	t.Helper()
	var output string
	var err error
	// The client can finish downloading before the server releases its lease.
	require.Eventually(t, func() bool {
		output, err = runCLI(t, "export", "release", id, "--json")
		code, _ := daemonconn.ProblemCode(err)
		return code != "export_retained"
	}, 30*time.Second, 25*time.Millisecond)
	require.NoError(t, err)
	return output
}
