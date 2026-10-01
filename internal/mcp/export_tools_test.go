package mcp

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/exporter"
	"go.kenn.io/docbank/internal/store"
)

func TestNativeExportOptIn(t *testing.T) {
	for _, options := range []ServerOptions{
		{}, {AllowProcessing: true}, {AllowPackageWrites: true}, {AllowPhotoEdits: true},
		{AllowProcessing: true, AllowPackageWrites: true, AllowPhotoEdits: true},
		{AllowExportWrites: true},
	} {
		var calls atomic.Int32
		lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
			calls.Add(1)
			return nil, errDaemonUnavailable
		}, func(*daemonconn.Connection) error { return nil })
		server := newServerWithOptionsAndDaemon(testImplementation(), options, lease)
		for _, transport := range []string{"stdio", "http"} {
			listed := decodeResult(t, exportExchange(t, server, transport, "tools/list", nil))
			names := listedToolNames(t, listed)
			require.Contains(t, names, "get_export_status")
			tools := listedToolsByName(t, listed)
			for _, name := range []string{
				"preview_export", "start_export", "cancel_export", "release_export", "download_export",
			} {
				if options.AllowExportWrites {
					require.Contains(t, names, name)
					annotations := objectField(t, tools[name], "annotations")
					require.Equal(t, false, annotations["readOnlyHint"])
					require.Equal(t, false, annotations["idempotentHint"])
					destructive := name == "cancel_export" || name == "release_export" ||
						name == "download_export"
					require.Equal(t, destructive,
						annotations["destructiveHint"])
				} else {
					require.NotContains(t, names, name)
					raw := exportExchange(t, server, transport, "tools/call", map[string]any{
						"name": name, "arguments": map[string]any{"job_id": uuid.New().String()},
					})
					require.NotZero(t, decodeWireError(t, raw).Code)
				}
			}
		}
		require.Zero(t, calls.Load())
	}
}

func exportExchange(
	t *testing.T, server *Server, transport, method string, params map[string]any,
) []byte {
	t.Helper()
	raw := requestFor(method, params)
	if transport == "stdio" {
		return exchangeRaw(t, server, raw)
	}
	handler, err := server.HTTPTransportHandler(HTTPOptions{BearerToken: "synthetic-export-key"})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7341/mcp", bytes.NewReader(raw))
	name, _ := params["name"].(string)
	request.Header = protocolHeaders(method, name)
	request.Header.Set("Authorization", "Bearer synthetic-export-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Body.Bytes()
}

func nativeExportArguments() map[string]any {
	return map[string]any{
		"source_operation_id": uuid.New().String(), "plan_operation_id": uuid.New().String(),
		"members": []any{map[string]any{
			"node_id": 7, "version_id": testVersionID, "sha256": strings.Repeat("a", 64), "size": 0,
		}},
	}
}

func TestNativeExportPreviewInput(t *testing.T) {
	var calls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(daemon.Close)
	lease := exportTestLease(t, daemon.URL)
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowExportWrites: true}, lease)
	for _, field := range []string{"source_operation_id", "plan_operation_id", "members"} {
		for _, value := range []any{nil, "invalid"} {
			args := nativeExportArguments()
			args[field] = value
			assertExportInvalid(t, server, args)
		}
		args := nativeExportArguments()
		delete(args, field)
		assertExportInvalid(t, server, args)
	}
	for _, field := range []string{"node_id", "version_id", "sha256", "size", "revision"} {
		for _, value := range []any{nil, -1, "invalid"} {
			args := nativeExportArguments()
			members, ok := args["members"].([]any)
			require.True(t, ok)
			member, ok := members[0].(map[string]any)
			require.True(t, ok)
			member[field] = value
			assertExportInvalid(t, server, args)
		}
	}
	for _, members := range []any{
		[]any{}, []any{nil},
		[]bundle.Member{{NodeID: 7, VersionID: testVersionID, SHA256: strings.Repeat("A", 64)}},
		[]bundle.Member{{NodeID: 7, VersionID: testVersionID, SHA256: strings.Repeat("a", 64),
			Size: bundle.MaxRoleBytes + 1}},
		[]bundle.Member{{NodeID: 7, VersionID: testVersionID, SHA256: strings.Repeat("a", 64)},
			{NodeID: 7, VersionID: testVersionID, SHA256: strings.Repeat("a", 64)}},
		make([]bundle.Member, 1001),
	} {
		args := nativeExportArguments()
		args["members"] = members
		assertExportInvalid(t, server, args)
	}
	args := nativeExportArguments()
	args["extra"] = true
	assertExportInvalid(t, server, args)
	require.Zero(t, calls.Load(), "malformed selections must not reserve source slots")
}

func assertExportInvalid(t *testing.T, server *Server, args map[string]any) {
	t.Helper()
	raw := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "preview_export", "arguments": args,
	}))
	require.EqualValues(t, jsonrpc.CodeInvalidParams, decodeWireError(t, raw).Code)
}

func exportTestLease(t *testing.T, url string) *daemonLease {
	t.Helper()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		connection := daemonconn.New(url, "synthetic-export-key")
		t.Cleanup(func() { require.NoError(t, connection.Close()) })
		return connection, nil
	}, func(c *daemonconn.Connection) error { return c.Close() })
	return lease
}

type nativeExportFixture struct {
	catalog    *store.Store
	blobs      *blob.Store
	worker     *exporter.Worker
	connection *daemonconn.Connection
	lease      *daemonLease
}

func newNativeExportFixture(t *testing.T) *nativeExportFixture {
	t.Helper()
	root := t.TempDir()
	t.Setenv("DOCBANK_HOME", root)
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	gate := api.NewOperationGate()
	worker, err := exporter.New(catalog, blobs, root, gate)
	require.NoError(t, err)
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-export-key"
	daemon := api.NewServer(api.Deps{
		Store: catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg, Gate: gate, Exports: worker,
	})
	t.Cleanup(daemon.Close)
	httpServer := httptest.NewServer(daemon.Handler())
	t.Cleanup(httpServer.Close)
	connection := daemonconn.New(httpServer.URL, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	return &nativeExportFixture{catalog, blobs, worker, connection, exportTestLease(t, httpServer.URL)}
}

func (f *nativeExportFixture) addFile(t *testing.T, name, content string) bundle.Member {
	t.Helper()
	hash, size, err := f.blobs.Write(strings.NewReader(content))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), name, hash, size, "text/plain")
	require.NoError(t, err)
	return bundle.Member{NodeID: node.ID, VersionID: node.CurrentVersionID, SHA256: hash, Size: size}
}

func exportCall(t *testing.T, server *Server, name string, args any) map[string]any {
	t.Helper()
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": name, "arguments": args,
	})))
	return objectField(t, result, "structuredContent")
}

func previewNativeExport(t *testing.T, server *Server, members []bundle.Member) bundle.Plan {
	t.Helper()
	output := exportCall(t, server, "preview_export", map[string]any{
		"source_operation_id": uuid.New().String(), "plan_operation_id": uuid.New().String(),
		"members": members,
	})
	var plan bundle.Plan
	data, err := json.Marshal(output["plan"])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &plan))
	require.NotEmpty(t, plan.ID)
	require.Equal(t, "private", output["cacheScope"])
	return plan
}

func startNativeExport(t *testing.T, server *Server, plan bundle.Plan) string {
	t.Helper()
	id := uuid.New().String()
	output := exportCall(t, server, "start_export", bundle.JobRequest{
		OperationID: id, PlanID: plan.ID, Fingerprint: plan.Fingerprint,
	})
	require.Equal(t, id, objectField(t, output, "job")["id"])
	return id
}

func TestNativeExportLifecycle(t *testing.T) {
	f := newNativeExportFixture(t)
	member := f.addFile(t, "synthetic.txt", "synthetic original\n")
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowExportWrites: true}, f.lease)
	plan := previewNativeExport(t, server, []bundle.Member{member})
	require.Equal(t, []bundle.RolePolicy{{Role: "original"}}, plan.Roles)
	require.Equal(t, "explicit", plan.Source.Kind)
	first := startNativeExport(t, server, plan)
	idArgs := map[string]any{"job_id": first}
	require.Equal(t, "export_conflict", exportCall(t, server, "release_export", idArgs)["code"])
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	status := objectField(t, exportCall(t, server, "get_export_status", idArgs), "job")
	require.Equal(t, "completed", status["state"])
	require.Equal(t, "export_conflict", exportCall(t, server, "cancel_export", idArgs)["code"])
	second, err := f.connection.API().CreateExportJob(t.Context(),
		&apiclient.CreateExportJobRequestOptions{Body: &bundle.JobRequest{
			OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint,
		}})
	require.NoError(t, err)
	processed, err = f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	require.Equal(t, "export_limit", exportCall(t, server, "start_export", bundle.JobRequest{
		OperationID: uuid.New().String(), PlanID: plan.ID, Fingerprint: plan.Fingerprint,
	})["code"])
	require.Equal(t, true, exportCall(t, server, "release_export", idArgs)["released"])
	require.Equal(t, "not_found", exportCall(t, server, "release_export", idArgs)["code"])
	third := startNativeExport(t, server, plan)
	thirdArgs := map[string]any{"job_id": third}
	require.Equal(t, true, exportCall(t, server, "cancel_export", thirdArgs)["accepted"])
	require.Equal(t, "canceled",
		objectField(t, exportCall(t, server, "get_export_status", thirdArgs), "job")["state"])
	require.Equal(t, true, exportCall(t, server, "release_export", thirdArgs)["released"])
	require.Equal(t, true, exportCall(t, server, "release_export",
		map[string]any{"job_id": second.ID})["released"])
	reader, _, err := f.blobs.OpenStreamContext(t.Context(), member.SHA256)
	require.NoError(t, err)
	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, "synthetic original\n", string(content))
}

func TestNativeExportLifecycleReplay(t *testing.T) {
	f := newNativeExportFixture(t)
	first := f.addFile(t, "first.txt", "first synthetic original")
	second := f.addFile(t, "empty.txt", "")
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowExportWrites: true}, f.lease)
	args := nativeExportArguments()
	args["members"] = []bundle.Member{second, first}
	preview := exportCall(t, server, "preview_export", args)
	require.Equal(t, preview, exportCall(t, server, "preview_export", args))
	plan := objectField(t, preview, "plan")
	require.Equal(t, args["plan_operation_id"], plan["id"])
	args["members"] = []bundle.Member{first, second}
	require.Equal(t, "export_conflict", exportCall(t, server, "preview_export", args)["code"])
	start := map[string]any{
		"operation_id": uuid.New().String(), "plan_id": plan["id"], "fingerprint": plan["fingerprint"],
	}
	job := exportCall(t, server, "start_export", start)
	require.Equal(t, job, exportCall(t, server, "start_export", start))
	start["fingerprint"] = strings.Repeat("0", 64)
	require.Equal(t, "export_conflict", exportCall(t, server, "start_export", start)["code"])
	claim, err := f.catalog.ClaimExportJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, f.catalog.FinishExportJob(t.Context(), claim, nil, "", "export_failed"))
	id := map[string]any{"job_id": start["operation_id"]}
	status := objectField(t, exportCall(t, server, "get_export_status", id), "job")
	require.Equal(t, "failed", status["state"])
	require.Equal(t, "export_failed", status["failure"])
	require.Equal(t, true, exportCall(t, server, "cancel_export", id)["accepted"])
	require.Equal(t, true, exportCall(t, server, "release_export", id)["released"])
	// A separately owned job exists, but the master caller cannot release it.
	source, err := f.catalog.CreateExportSource(t.Context(), "synthetic-browser-owner",
		bundle.SourceRequest{OperationID: uuid.New().String(), Kind: "explicit",
			Members: []bundle.Member{first}}, nil)
	require.NoError(t, err)
	otherPlan, err := f.catalog.CreateExportPlan(t.Context(), "synthetic-browser-owner",
		bundle.PlanRequest{OperationID: uuid.New().String(), SourceID: source.ID,
			MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}}})
	require.NoError(t, err)
	otherJob, err := f.catalog.QueueExportJob(t.Context(), "synthetic-browser-owner",
		bundle.JobRequest{OperationID: uuid.New().String(), PlanID: otherPlan.ID,
			Fingerprint: otherPlan.Fingerprint})
	require.NoError(t, err)
	require.Equal(t, "not_found", exportCall(t, server, "release_export",
		map[string]any{"job_id": otherJob.ID})["code"])
}

func TestNativeExportWriteRecovery(t *testing.T) {
	for _, mode := range []string{"disconnect", "invalid response"} {
		for _, name := range []string{
			"preview_export", "start_export", "cancel_export", "release_export",
		} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				jobID := uuid.New().String()
				args := map[string]any{"job_id": jobID}
				path, method := "/api/v1/exports/jobs/"+jobID, http.MethodPost
				switch name {
				case "preview_export":
					args, path = nativeExportArguments(), "/api/v1/exports/plans"
				case "start_export":
					args = map[string]any{"operation_id": uuid.New().String(),
						"plan_id": uuid.New().String(), "fingerprint": strings.Repeat("a", 64)}
					path = "/api/v1/exports/jobs"
				case "cancel_export":
					path += "/cancel"
				case "release_export":
					method = http.MethodDelete
				}
				var attempts, sources atomic.Int32
				var bodies [][]byte
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if !assert.NoError(t, err) {
						return
					}
					if r.URL.Path == "/api/v1/exports/sources" {
						sources.Add(1)
						var source bundle.SourceRequest
						if !assert.NoError(t, json.Unmarshal(body, &source)) || !assert.Len(t, source.Members, 1) {
							return
						}
						assert.Equal(t, args["source_operation_id"], source.OperationID)
						assert.Equal(t, "explicit", source.Kind)
						assert.Zero(t, source.Members[0].Size)
						assert.NotContains(t, string(body), "revision")
						w.Header().Set("Content-Type", "application/json")
						assert.NoError(t, json.MarshalWrite(w, bundle.Source{
							ID: source.OperationID, MemberHash: strings.Repeat("b", 64),
						}))
						return
					}
					assert.Equal(t, path, r.URL.Path)
					assert.Equal(t, method, r.Method)
					bodies = append(bodies, body)
					attempts.Add(1)
					if mode == "invalid response" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, "{")
						return
					}
					hijacker, ok := w.(http.Hijacker)
					if !assert.True(t, ok) {
						return
					}
					connection, _, err := hijacker.Hijack()
					if !assert.NoError(t, err) {
						return
					}
					assert.NoError(t, connection.Close())
				}))
				t.Cleanup(daemon.Close)
				server := newServerWithOptionsAndDaemon(testImplementation(),
					ServerOptions{AllowExportWrites: true}, exportTestLease(t, daemon.URL))
				for i := int32(1); i <= 2; i++ {
					result := exportCall(t, server, name, args)
					require.Equal(t, "export_outcome_unknown", result["code"])
					require.Equal(t, i, attempts.Load(), "no automatic write retry")
				}
				require.Equal(t, bodies[0], bodies[1], "explicit replay preserves request")
				if name == "preview_export" {
					require.EqualValues(t, 2, sources.Load())
					var plan bundle.PlanRequest
					require.NoError(t, json.Unmarshal(bodies[0], &plan))
					require.Equal(t, args["plan_operation_id"], plan.OperationID)
					require.Equal(t, args["source_operation_id"], plan.SourceID)
					require.Equal(t, strings.Repeat("b", 64), plan.MemberHash)
					require.Equal(t, []bundle.RolePolicy{{Role: "original"}}, plan.Roles)
				}
			})
		}
	}
}

func TestNativeExportWriteRecoveryStoppedDaemon(t *testing.T) {
	var attempts, acquisitions atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(daemon.Close)
	oldDaemon := httptest.NewServer(http.NotFoundHandler())
	closed := daemonconn.New(oldDaemon.URL, "synthetic-export-key")
	t.Cleanup(func() { require.NoError(t, closed.Close()) })
	oldDaemon.Close()
	lease := newDaemonLeaseWith(func(context.Context) (*daemonconn.Connection, error) {
		if acquisitions.Add(1) == 1 {
			return closed, nil
		}
		connection := daemonconn.New(daemon.URL, "synthetic-export-key")
		t.Cleanup(func() { require.NoError(t, connection.Close()) })
		return connection, nil
	}, func(c *daemonconn.Connection) error { return c.Close() })
	server := newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowExportWrites: true}, lease)
	_, err := lease.acquire(t.Context())
	require.NoError(t, err)
	args := map[string]any{"job_id": uuid.New().String()}
	require.Equal(t, "export_outcome_unknown", exportCall(t, server, "release_export", args)["code"])
	require.Zero(t, attempts.Load())
	require.EqualValues(t, 1, acquisitions.Load())
	require.Equal(t, true, exportCall(t, server, "release_export", args)["released"])
	require.EqualValues(t, 1, attempts.Load())
	require.EqualValues(t, 2, acquisitions.Load())
}
