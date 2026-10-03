package mcp

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

func TestExportInspectionLargePlan(t *testing.T) {
	f := newNativeExportFixture(t)
	var members []bundle.Member
	for i := range 1001 {
		node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(),
			fmt.Sprintf("synthetic-%04d.bin", i), strings.Repeat("a", 64), 12, "application/octet-stream")
		require.NoError(t, err)
		members = append(members, bundle.Member{NodeID: node.ID, VersionID: node.CurrentVersionID,
			SHA256: node.BlobHash, Size: node.Size})
	}
	source, err := f.catalog.CreateExportSource(t.Context(), "master", bundle.SourceRequest{
		OperationID: uuid.New().String(), Kind: "upload", Total: len(members),
		MemberHash: store.ExportMemberHash(members),
	}, nil)
	require.NoError(t, err)
	require.NoError(t, f.catalog.PutExportChunk(t.Context(), "master", source.ID, 0, members[:1000]))
	require.NoError(t, f.catalog.PutExportChunk(t.Context(), "master", source.ID, 1, members[1000:]))
	source, err = f.catalog.SealExportSource(t.Context(), "master", source.ID)
	require.NoError(t, err)
	plan, err := f.catalog.CreateExportPlan(t.Context(), "master", bundle.PlanRequest{
		OperationID: uuid.New().String(), SourceID: source.ID, MemberHash: source.MemberHash,
		Roles:        []bundle.RolePolicy{{Role: "original"}},
		VolumeLimits: &bundle.VolumeLimits{RoleBytes: 1024, Roles: 2}, DuplicatePolicy: "preserve",
	})
	require.NoError(t, err)
	server := newServerWithOptionsAndDaemon(testImplementation(), ServerOptions{}, f.lease)
	output := exportCall(t, server, "get_export_plan", map[string]any{"plan_id": plan.ID})
	got := objectField(t, output, "plan")
	require.EqualValues(t, 1001, got["total"])
	require.Equal(t, "upload", objectField(t, got, "source")["kind"])
	require.EqualValues(t, 501, got["volumes"])
	problems := objectField(t, exportCall(t, server, "get_export_problems",
		map[string]any{"plan_id": plan.ID}), "problems")
	require.EqualValues(t, 0, problems["total"])
	require.Empty(t, problems["items"])
}

func TestExportInspectionRetainedHeaderFields(t *testing.T) {
	// Exercise optional wire fields without rebuilding mailbox and attachment workflows.
	plan := bundle.Plan{Format: bundle.Format, ID: testVersionID, VaultID: testVersionID,
		Toolchain: "synthetic-toolchain", Fingerprint: strings.Repeat("a", 64), Total: 1,
		DocumentRows: 2, CreatedAt: "2020-01-01T00:00:00Z", ExpiresAt: "2020-01-01T00:10:00Z",
		Source: bundle.Source{ID: testVersionID, RequestSHA256: strings.Repeat("b", 64),
			Kind: "saved_query", State: "sealed", MemberHash: strings.Repeat("c", 64), Total: 1,
			CreatedAt: "2020-01-01T00:00:00Z", ExpiresAt: "2020-01-01T00:10:00Z",
			SavedQueryID: testVersionID, SavedQueryRevision: 3, QueryFingerprint: strings.Repeat("d", 64)},
		Roles: []bundle.RolePolicy{
			{Role: "original"},
			{Role: "text", AllowUnavailable: true, ProfileFingerprint: strings.Repeat("e", 64)},
			{Role: "pages", AllowUnavailable: true, RecipeSHA256: strings.Repeat("f", 64)},
			{Role: "email_pdf", AllowUnavailable: true, ProfileFingerprint: strings.Repeat("e", 64)},
			{Role: "attachment_original", AllowUnavailable: true},
			{Role: "attachment_pdf", AllowUnavailable: true, RecipeSHA256: strings.Repeat("f", 64)},
		}, Counts: &bundle.OutputCounts{Messages: 1, Attachments: 1, Unavailable: 5,
			UnavailableInventories: 1}, DuplicatePolicy: "collapse_exact_content",
	}
	for _, kind := range []string{"saved_query", "mailbox_collection", "query", "snapshot"} {
		t.Run(kind, func(t *testing.T) {
			selected := plan
			selected.Source.Kind = kind
			if kind != "saved_query" {
				selected.Source.SavedQueryID = ""
				selected.Source.SavedQueryRevision = 0
			}
			if kind == "mailbox_collection" {
				selected.Source.CollectionID = testVersionID
			}
			var calls int
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/exports/plans/"+testVersionID, r.URL.Path)
				assert.Equal(t, "synthetic-export-key", r.Header.Get("X-Api-Key"))
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, selected))
			}))
			t.Cleanup(daemon.Close)
			server := newServerWithOptionsAndDaemon(
				testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL),
			)
			output := exportCall(t, server, "get_export_plan", map[string]any{"plan_id": testVersionID})
			raw, err := json.Marshal(output["plan"])
			require.NoError(t, err)
			var got bundle.Plan
			require.NoError(t, json.Unmarshal(raw, &got))
			require.Equal(t, selected, got, "a past admission deadline must not prevent reads")
			require.Equal(t, 1, calls)
		})
	}
}

func TestExportInspectionBoundedPage(t *testing.T) {
	reason := strings.Repeat("quoted \"text\"\n", 3800)
	page := bundle.OutputProblems{PlanID: testVersionID, Fingerprint: strings.Repeat("a", 64),
		After: 17, Next: 18, Total: 19, Items: []bundle.OutputProblem{{NodeID: 7,
			VersionID: testVersionID, PartPath: "1.2", Role: "attachment_original", Reason: reason}}}
	raw, err := json.Marshal(page)
	require.NoError(t, err)
	require.LessOrEqual(t, len(raw), bundle.MaxMemberBytes)
	var calls int
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/exports/plans/"+testVersionID+"/problems", r.URL.Path)
		assert.Equal(t, "17", r.URL.Query().Get("after"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(daemon.Close)
	server := newServerWithOptionsAndDaemon(
		testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL),
	)
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "get_export_problems", "arguments": map[string]any{"plan_id": testVersionID, "after": 17},
	})))
	output := objectField(t, result, "structuredContent")
	encoded, err := json.Marshal(output["problems"])
	require.NoError(t, err)
	var got bundle.OutputProblems
	require.NoError(t, json.Unmarshal(encoded, &got))
	require.Equal(t, page, got)
	encoded, err = json.Marshal(result)
	require.NoError(t, err)
	require.Less(t, len(encoded), 1<<20)
	require.Equal(t, 1, calls, "a continuation must not be fetched automatically")
}

func TestExportInspectionReadErrors(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"export_expired", 410}, {"export_limit", 413}, {"export_conflict", 409}, {"not_found", 404},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var calls int
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/api/v1/exports/plans/"+testVersionID+"/problems", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"code":%q,"title":"synthetic private detail"}`,
					tc.status, tc.code)
			}))
			t.Cleanup(daemon.Close)
			server := newServerWithOptionsAndDaemon(
				testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL),
			)
			output := exportCall(t, server, "get_export_problems", map[string]any{"plan_id": testVersionID})
			require.Equal(t, tc.code, output["code"])
			require.NotContains(t, output["message"], "synthetic private detail")
			require.Equal(t, 1, calls)
		})
	}
}

func TestExportInspectionBadResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"id":`} {
		t.Run(body, func(t *testing.T) {
			var calls int
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(daemon.Close)
			server := newServerWithOptionsAndDaemon(
				testImplementation(), ServerOptions{}, exportTestLease(t, daemon.URL),
			)
			for _, name := range []string{"get_export_plan", "get_export_problems"} {
				raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
					"name": name, "arguments": map[string]any{"plan_id": testVersionID},
				}))
				if body == `{}` {
					require.Contains(t, decodeWireError(t, raw).Message,
						"does not conform to the published tool schema")
				} else {
					require.EqualValues(t, jsonrpc.CodeInternalError, decodeWireError(t, raw).Code)
				}
			}
			require.Equal(t, 2, calls, "neither invalid nor partial responses may be retried")
		})
	}
}
