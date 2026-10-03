package mcp

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
)

func TestExportInspectionWorkflow(t *testing.T) {
	f := newNativeExportFixture(t)
	var members []bundle.Member
	var nodes []int64
	for i := range 51 {
		member := f.addFile(t, fmt.Sprintf("synthetic-%02d.bin", i), "synthetic original")
		members = append(members, member)
		nodes = append(nodes, member.NodeID)
	}
	source, err := f.connection.API().CreateExportSource(t.Context(),
		&apiclient.CreateExportSourceRequestOptions{Body: &bundle.SourceRequest{
			OperationID: uuid.New().String(), Kind: "nodes", NodeIDs: nodes,
		}})
	require.NoError(t, err)
	plan, err := f.connection.API().CreateExportPlan(t.Context(),
		&apiclient.CreateExportPlanRequestOptions{Body: &bundle.PlanRequest{
			OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
			Roles:        []bundle.RolePolicy{{Role: "original"}, {Role: "text", AllowUnavailable: true}},
			VolumeLimits: &bundle.VolumeLimits{RoleBytes: 1024, Roles: 2}, DuplicatePolicy: "preserve",
		}})
	require.NoError(t, err)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	for _, transport := range []string{"stdio", "http"} {
		listed := decodeResult(t, exportExchange(t, server, transport, "tools/list", nil))
		for _, name := range []string{"get_export_plan", "get_export_problems"} {
			require.Contains(t, listedToolNames(t, listed), name)
			annotations := objectField(t, listedToolsByName(t, listed)[name], "annotations")
			require.Equal(t, true, annotations["readOnlyHint"])
			require.Equal(t, true, annotations["idempotentHint"])
			require.Equal(t, false, annotations["destructiveHint"])
			require.Equal(t, false, annotations["openWorldHint"])
		}
		result := decodeResult(t, exportExchange(t, server, transport, "tools/call", map[string]any{
			"name": "get_export_plan", "arguments": map[string]any{"plan_id": plan.ID},
		}))
		output := objectField(t, result, "structuredContent")
		raw, err := json.Marshal(output["plan"])
		require.NoError(t, err)
		var got bundle.Plan
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Equal(t, *plan, got)
		require.Equal(t, "private", output["cacheScope"])
		require.EqualValues(t, 0, output["ttlMs"])
	}
	for _, after := range []int{0, 50, 51} {
		args := map[string]any{"plan_id": plan.ID}
		if after != 0 {
			args["after"] = after
		}
		output := exportCall(t, server, "get_export_problems", args)
		raw, err := json.Marshal(output["problems"])
		require.NoError(t, err)
		var page bundle.OutputProblems
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Equal(t, plan.ID, page.PlanID)
		require.Equal(t, plan.Fingerprint, page.Fingerprint)
		require.Equal(t, after, page.After)
		require.Equal(t, 51, page.Total)
		require.Len(t, page.Items, min(50, 51-after))
		if after == 0 {
			require.Equal(t, 50, page.Next)
		} else {
			require.Zero(t, page.Next)
		}
		for i, item := range page.Items {
			require.Equal(t, members[after+i].NodeID, item.NodeID)
			require.Equal(t, members[after+i].VersionID, item.VersionID)
			require.Equal(t, "text", item.Role)
		}
	}
	require.Equal(t, "export_conflict", exportCall(t, server, "get_export_problems",
		map[string]any{"plan_id": plan.ID, "after": 52})["code"])
	require.Equal(t, "not_found", exportCall(t, server, "get_export_plan",
		map[string]any{"plan_id": uuid.New().String()})["code"])
	before := exportCall(t, server, "get_export_problems", map[string]any{"plan_id": plan.ID})
	header := exportCall(t, server, "get_export_plan", map[string]any{"plan_id": plan.ID})
	node, err := f.catalog.NodeByID(t.Context(), members[0].NodeID)
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), node.ID, node.Revision,
		strings.Repeat("b", 64), 1, "application/octet-stream")
	require.NoError(t, err)
	require.Equal(t, header, exportCall(t, server, "get_export_plan",
		map[string]any{"plan_id": plan.ID}))
	require.Equal(t, before, exportCall(t, server, "get_export_problems",
		map[string]any{"plan_id": plan.ID}))
}

func TestExportInspectionInputs(t *testing.T) {
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(daemon.Close)
	server := newServerWithOptionsAndDaemon(
		testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL),
	)
	for _, name := range []string{"get_export_plan", "get_export_problems"} {
		cases := []map[string]any{{}, {"plan_id": nil}, {"plan_id": "bad"},
			{"plan_id": "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}, {"plan_id": testVersionID, "extra": true}}
		if name == "get_export_problems" {
			for _, after := range []any{nil, "0", -1, 2600001, 0.5} {
				cases = append(cases, map[string]any{"plan_id": testVersionID, "after": after})
			}
		}
		for _, args := range cases {
			raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
				"name": name, "arguments": args,
			}))
			require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code, name)
		}
	}
	require.Zero(t, calls.Load())
}
