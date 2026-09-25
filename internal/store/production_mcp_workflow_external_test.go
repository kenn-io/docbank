package store_test

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

// This opt-in proof drives the MCP stdio server and real daemon over a
// retained synthetic PDF without using CLI production operations.
func TestProductionMCPRealDaemonFinalizesAndDownloadsPackage(t *testing.T) {
	if os.Getenv("DOCBANK_PRODUCTION_MCP_REAL_DAEMON") != "1" {
		t.Skip("opt-in real daemon production MCP workflow proof")
	}
	vault, root, setID, revision, etag, namespaceID := store.ProductionRenderDaemonHTTPFixture(t)
	sourceMembers, _, err := vault.ProductionMembers(t.Context(), setID, revision, "", 200)
	require.NoError(t, err)
	require.NotEmpty(t, sourceMembers)
	sourceDecisions, _, err := vault.ProductionDecisions(t.Context(), setID, revision, "", 500)
	require.NoError(t, err)
	require.NotEmpty(t, sourceDecisions)
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
	client := newProductionMCPTestClient(t, binary, root)
	base := map[string]any{"set_id": setID, "revision": revision}
	draft := client.call(t, "get_production_draft", base)
	require.EqualValues(t, etag, objectValue(t, draft, "draft")["etag"])
	memberIDs := make([]string, 0, 2)
	var memberCursor string
	for {
		args := map[string]any{"set_id": setID, "revision": revision, "limit": 1}
		if memberCursor != "" {
			args["cursor"] = memberCursor
		}
		page := client.call(t, "list_production_members", args)
		for _, item := range arrayValue(t, page) {
			member, ok := item.(map[string]any)
			require.True(t, ok)
			id, ok := member["id"].(string)
			require.True(t, ok)
			memberIDs = append(memberIDs, id)
		}
		memberCursor, _ = page["next_cursor"].(string)
		if memberCursor == "" {
			break
		}
	}
	require.Len(t, memberIDs, 2)
	var decisionCursor string
	var decisionCount int
	for {
		args := map[string]any{"set_id": setID, "revision": revision, "limit": 1}
		if decisionCursor != "" {
			args["cursor"] = decisionCursor
		}
		page := client.call(t, "list_production_decisions", args)
		decisionCount += len(arrayValue(t, page))
		decisionCursor, _ = page["next_cursor"].(string)
		if decisionCursor == "" {
			break
		}
	}
	require.Equal(t, 2, decisionCount)
	resolved := client.call(t, "resolve_production_selection", map[string]any{
		"set_id": setID, "revision": revision, "etag": etag,
		"member_id": memberIDs[0], "page": 1, "limit": 1})
	require.Equal(t, memberIDs[0], resolved["member_id"])
	require.NotEmpty(t, resolved["review_binding"])

	const finalizeOperation = "79000000-0000-4000-8000-000000000201"
	const snapshotID = "79000000-0000-4000-8000-000000000202"
	finalizeArgs := map[string]any{"set_id": setID, "revision": revision, "etag": etag,
		"namespace_id": namespaceID, "snapshot_id": snapshotID, "operation_id": finalizeOperation}
	finalized := client.call(t, "finalize_production_draft", finalizeArgs)
	require.Equal(t, "finalized", objectValue(t, finalized, "draft")["state"])
	require.Equal(t, finalized, client.call(t, "finalize_production_draft", finalizeArgs))

	const jobID = "79000000-0000-4000-8000-000000000203"
	const admitOperation = "79000000-0000-4000-8000-000000000204"
	admitArgs := map[string]any{"set_id": setID, "revision": revision, "etag": etag,
		"job_id": jobID, "operation_id": admitOperation}
	admitted := client.call(t, "admit_production_job", admitArgs)
	require.Equal(t, jobID, objectValue(t, admitted, "job")["job_id"])
	require.Equal(t, admitted, client.call(t, "admit_production_job", admitArgs))
	deadline := time.Now().Add(2 * time.Minute)
	var status map[string]any
	for {
		status = objectValue(t, client.call(t, "get_production_job", map[string]any{
			"set_id": setID, "job_id": jobID}), "job")
		if status["state"] == "succeeded" || status["state"] == "failed" || status["state"] == "canceled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("production job did not finish: last state %v", status["state"])
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Equal(t, "succeeded", status["state"])
	require.NotEmpty(t, status["receipt_sha256"])

	const publishOperation = "79000000-0000-4000-8000-000000000205"
	publishArgs := map[string]any{"job_id": jobID, "operation_id": publishOperation,
		"profile_id": "export-dat-opt-images-v1", "max_volume_bytes": 52_428_800,
		"max_volume_documents": 10}
	published := client.call(t, "publish_production_package", publishArgs)
	require.Equal(t, publishOperation, published["operation_id"])
	require.Equal(t, published, client.call(t, "publish_production_package", publishArgs))

	destination := filepath.Join(t.TempDir(), "synthetic-package.zip")
	downloaded := client.call(t, "download_production_package", map[string]any{
		"job_id": jobID, "operation_id": publishOperation,
		"destination_path": destination, "overwrite": false})
	require.Equal(t, "published", downloaded["state"])
	require.Equal(t, destination, downloaded["destination_path"])
	require.Equal(t, published["archive_sha256"], downloaded["archive_sha256"])
	require.Equal(t, published["version_id"], downloaded["version_id"])
	require.NotContains(t, downloaded, "url")
	archive, err := os.ReadFile(destination)
	require.NoError(t, err)
	size, ok := published["size"].(float64)
	require.True(t, ok)
	require.Len(t, archive, int(size))
	sum := sha256.Sum256(archive)
	require.Equal(t, published["archive_sha256"], hex.EncodeToString(sum[:]))
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	require.NotEmpty(t, reader.File)
	proveProductionMCPLargeSelectionAndReview(t, client, setID, revision, sourceMembers[0], sourceDecisions[0])
}

func proveProductionMCPLargeSelectionAndReview(t *testing.T, client *productionMCPTestClient,
	setID string, revision int64, source redaction.Member, sourceDecision redaction.Decision) {
	t.Helper()
	const forkOperation = "79000000-0000-4000-8000-000000000206"
	forked := objectValue(t, client.call(t, "fork_production_draft", map[string]any{
		"set_id": setID, "revision": revision, "operation_id": forkOperation}), "draft")
	forkRevision := revision + 1
	require.EqualValues(t, forkRevision, forked["revision"])
	require.EqualValues(t, 1, forked["etag"])

	// A full 200-item page followed by a second page proves that an operator
	// can traverse a selection larger than the MCP member page ceiling.
	members := make([]api.ProductionMember, 0, 199)
	changes := make([]api.ProductionChange, 0, 199)
	for index := range 199 {
		member := source
		member.ID = fmt.Sprintf("79000000-0000-4000-8000-%012d", 300+index)
		member.Ordinal = int64(index + 3)
		member.Reviewed = false
		member.ReviewBinding = ""
		members = append(members, api.ProductionMember(member))
		decision := sourceDecision
		decision.ID = fmt.Sprintf("79000000-0000-4000-8000-%012d", 600+index)
		decision.MemberID = member.ID
		decision.Actor, decision.CreatedAt, decision.Revision = "", "", 0
		apiDecision := api.ProductionDecision(decision)
		changes = append(changes, api.ProductionChange{Kind: "decision", Decision: &apiDecision})
	}
	membersJSON, err := json.Marshal(members)
	require.NoError(t, err)
	const appendOperation = "79000000-0000-4000-8000-000000000207"
	appendArgs := map[string]any{"set_id": setID, "revision": forkRevision, "etag": 1,
		"operation_id": appendOperation, "members_json": string(membersJSON)}
	appended := client.call(t, "append_production_members", appendArgs)
	require.EqualValues(t, 2, appended["etag"])
	require.Equal(t, appended, client.call(t, "append_production_members", appendArgs))
	changesJSON, err := json.Marshal(changes)
	require.NoError(t, err)
	const changesOperation = "79000000-0000-4000-8000-000000000208"
	changed := client.call(t, "apply_production_changes", map[string]any{
		"set_id": setID, "revision": forkRevision, "etag": 2,
		"operation_id": changesOperation, "changes_json": string(changesJSON)})
	require.EqualValues(t, 3, changed["etag"])

	var memberCursor string
	seenMembers := make(map[string]struct{}, 201)
	for pageIndex := range 2 {
		args := map[string]any{"set_id": setID, "revision": forkRevision, "limit": 200}
		if memberCursor != "" {
			args["cursor"] = memberCursor
		}
		page := client.call(t, "list_production_members", args)
		if pageIndex == 0 {
			require.Len(t, arrayValue(t, page), 200)
		} else {
			require.Len(t, arrayValue(t, page), 1)
		}
		for _, item := range arrayValue(t, page) {
			member, ok := item.(map[string]any)
			require.True(t, ok)
			id, ok := member["id"].(string)
			require.True(t, ok)
			_, exists := seenMembers[id]
			require.False(t, exists)
			seenMembers[id] = struct{}{}
		}
		memberCursor, _ = page["next_cursor"].(string)
	}
	require.Len(t, seenMembers, 201)
	require.Empty(t, memberCursor)

	var decisionCursor string
	seenDecisions := make(map[string]struct{}, 201)
	for pageIndex := range 3 {
		args := map[string]any{"set_id": setID, "revision": forkRevision, "limit": 100}
		if decisionCursor != "" {
			args["cursor"] = decisionCursor
		}
		page := client.call(t, "list_production_decisions", args)
		if pageIndex < 2 {
			require.Len(t, arrayValue(t, page), 100)
		} else {
			require.Len(t, arrayValue(t, page), 1)
		}
		for _, item := range arrayValue(t, page) {
			decision, ok := item.(map[string]any)
			require.True(t, ok)
			id, ok := decision["id"].(string)
			require.True(t, ok)
			_, exists := seenDecisions[id]
			require.False(t, exists)
			seenDecisions[id] = struct{}{}
		}
		decisionCursor, _ = page["next_cursor"].(string)
	}
	require.Len(t, seenDecisions, 201)
	require.Empty(t, decisionCursor)

	draft := objectValue(t, client.call(t, "get_production_draft", map[string]any{
		"set_id": setID, "revision": forkRevision}), "draft")
	sealed := client.call(t, "seal_production_membership", map[string]any{
		"set_id": setID, "revision": forkRevision, "etag": 3,
		"operation_id": "79000000-0000-4000-8000-000000000209",
		"total":        201, "member_hash": draft["member_hash"]})
	require.EqualValues(t, 4, sealed["etag"])
	resolved := client.call(t, "resolve_production_selection", map[string]any{
		"set_id": setID, "revision": forkRevision, "etag": 4,
		"member_id": members[0].ID, "page": 1, "limit": 1})
	require.NotEmpty(t, resolved["review_binding"])
	reviewed := client.call(t, "review_production_member", map[string]any{
		"set_id": setID, "revision": forkRevision, "etag": 4,
		"member_id": members[0].ID, "binding": resolved["review_binding"],
		"operation_id": "79000000-0000-4000-8000-000000000210"})
	require.EqualValues(t, 5, reviewed["etag"])
}

type productionMCPTestClient struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	cmd    *exec.Cmd
	stderr bytes.Buffer
	nextID int
}

func newProductionMCPTestClient(t *testing.T, binary, root string) *productionMCPTestClient {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary, "mcp", "--allow-processing")
	cmd.Env = append(os.Environ(), "DOCBANK_HOME="+root)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	client := &productionMCPTestClient{cmd: cmd, stdout: bufio.NewReader(stdout)}
	cmd.Stderr = &client.stderr
	require.NoError(t, cmd.Start())
	client.stdin = stdin
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return client
}

func (client *productionMCPTestClient) call(t *testing.T, name string, arguments map[string]any) map[string]any {
	t.Helper()
	client.nextID++
	request := map[string]any{"jsonrpc": "2.0", "id": client.nextID, "method": "tools/call",
		"params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}, "name": name, "arguments": arguments}}
	frame, err := json.Marshal(request)
	require.NoError(t, err)
	_, err = client.stdin.Write(append(frame, '\n'))
	require.NoError(t, err, client.stderr.String())
	reply, err := client.stdout.ReadBytes('\n')
	require.NoError(t, err, client.stderr.String())
	var envelope struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(reply, &envelope))
	require.Equal(t, client.nextID, envelope.ID)
	require.Empty(t, envelope.Error, "%s: %s", name, reply)
	require.NotEqual(t, true, envelope.Result["isError"], "%s: %s", name, reply)
	return objectValue(t, envelope.Result, "structuredContent")
}

func objectValue(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	require.True(t, ok, "%q: %#v", key, object[key])
	return value
}

func arrayValue(t *testing.T, object map[string]any) []any {
	t.Helper()
	value, ok := object["items"].([]any)
	require.True(t, ok, "items: %#v", object["items"])
	return value
}
