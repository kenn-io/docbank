package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestExportMCPReadToolsAreDefaultAndWritesRequireOwnOptIn(t *testing.T) {
	readOnly := catalogNames(toolCatalog(false, false))
	require.Contains(t, readOnly, "preview_export_plan")
	require.Contains(t, readOnly, "get_export_job")
	for _, name := range []string{"create_export_source", "create_export_plan", "start_export_job", "cancel_export_job"} {
		require.NotContains(t, readOnly, name)
	}
	writable := catalogNames(toolCatalog(false, false, true))
	for _, name := range []string{"create_export_source", "create_export_plan", "start_export_job", "cancel_export_job"} {
		require.Contains(t, writable, name)
	}
	require.NotContains(t, catalogNames(toolCatalog(false, true)), "create_export_source",
		"load-file write opt-in must not grant export writes")
	require.True(t, ServerOptions{AllowExportWrites: true}.AllowExportWrites)
}

func TestExportMCPToolsDriveExactSyntheticPDFJob(t *testing.T) {
	const (
		sourceID  = "11111111-1111-4111-8111-111111111111"
		planID    = "22222222-2222-4222-8222-222222222222"
		jobID     = "33333333-3333-4333-8333-333333333333"
		versionID = "44444444-4444-4444-8444-444444444444"
	)
	memberHash := strings.Repeat("a", 64)
	fingerprint := strings.Repeat("b", 64)
	blobHash := strings.Repeat("c", 64)
	archiveHash := strings.Repeat("d", 64)
	source := bundle.Source{ID: sourceID, Kind: "explicit", State: "sealed", MemberHash: memberHash, Total: 1}
	plan := bundle.Plan{ID: planID, Source: source, Fingerprint: fingerprint, Total: 1}
	job := bundle.ExportJob{ID: jobID, PlanID: planID, Fingerprint: fingerprint, State: "queued", Sequence: 1}
	var requests atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-key" {
			t.Errorf("export request missing API key")
			http.Error(w, "missing API key", http.StatusUnauthorized)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/exports/sources":
			var input bundle.SourceRequest
			if err := json.UnmarshalRead(r.Body, &input); err != nil || input.OperationID != sourceID ||
				input.Kind != "explicit" || len(input.Members) != 1 || input.Members[0].VersionID != versionID {
				t.Errorf("wrong frozen source request: %+v, %v", input, err)
				http.Error(w, "wrong source", http.StatusBadRequest)
				return
			}
			result = source
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/exports/plans":
			var input bundle.PlanRequest
			if err := json.UnmarshalRead(r.Body, &input); err != nil || input.OperationID != planID ||
				input.SourceID != sourceID || input.MemberHash != memberHash || len(input.Roles) != 1 ||
				input.Roles[0].Role != "original" {
				t.Errorf("wrong export plan request: %+v, %v", input, err)
				http.Error(w, "wrong plan", http.StatusBadRequest)
				return
			}
			result = plan
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/exports/plans/"+planID+"/preview":
			result = bundle.PlanPreview{PlanID: planID, Fingerprint: fingerprint, MemberHash: memberHash,
				Total: 1, Roles: []bundle.RoleSummary{{Role: "original", AvailableMembers: 1, Files: 1, Bytes: 13}}}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/exports/jobs":
			var input bundle.JobRequest
			if err := json.UnmarshalRead(r.Body, &input); err != nil || input.OperationID != jobID ||
				input.PlanID != planID || input.Fingerprint != fingerprint {
				t.Errorf("wrong export job request: %+v, %v", input, err)
				http.Error(w, "wrong job", http.StatusBadRequest)
				return
			}
			result = job
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/exports/jobs/"+jobID:
			complete := job
			complete.State = "completed"
			complete.Sequence = 2
			complete.Receipt = &bundle.Receipt{Format: bundle.Format, PlanFingerprint: fingerprint,
				SHA256: archiveHash, Size: 200, Entries: 3}
			result = complete
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/exports/jobs/"+jobID+"/cancel":
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			t.Errorf("unexpected export request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.MarshalWrite(w, result); err != nil {
			t.Errorf("write synthetic export response: %v", err)
		}
	}))
	defer daemon.Close()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		return daemonconn.New(daemon.URL, "synthetic-key"), nil
	}, func(*daemonconn.Connection) error { return nil })
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{AllowExportWrites: true}, lease)
	call := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{"name": name, "arguments": arguments}))
		result := decodeResult(t, response)
		require.NotEqual(t, true, result["isError"], string(response))
		return objectField(t, result, "structuredContent")
	}
	selected := map[string]any{"node_id": 7, "version_id": versionID, "sha256": blobHash, "size": 13}
	sourceOutput := call("create_export_source", map[string]any{"operation_id": sourceID, "members": []any{selected}})
	require.Equal(t, sourceID, sourceOutput["source_id"])
	require.Equal(t, memberHash, sourceOutput["member_hash"])
	planOutput := call("create_export_plan", map[string]any{"operation_id": planID, "source_id": sourceID,
		"member_hash": memberHash, "roles": []any{map[string]any{"role": "original"}}})
	require.Equal(t, fingerprint, planOutput["fingerprint"])
	preview := call("preview_export_plan", map[string]any{"plan_id": planID})
	require.Equal(t, fingerprint, preview["fingerprint"])
	queued := call("start_export_job", map[string]any{"operation_id": jobID, "plan_id": planID, "fingerprint": fingerprint})
	require.Equal(t, "queued", queued["state"])
	completed := call("get_export_job", map[string]any{"job_id": jobID})
	require.Equal(t, archiveHash, objectField(t, completed, "receipt")["sha256"])
	canceled := call("cancel_export_job", map[string]any{"job_id": jobID})
	require.Equal(t, true, canceled["accepted"])
	require.EqualValues(t, 6, requests.Load())
}
