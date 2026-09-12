package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

func exportOwner(ctx context.Context) (string, error) {
	owner, ok := workspaceSnapshotOwner(ctx)
	if !ok {
		return "", NewError(401, "unauthorized", "authenticated export owner required")
	}
	if !browserSessionRequest(ctx) {
		owner = "master"
	}
	return owner, nil
}
func exportProblem(err error) *Error {
	switch {
	case errors.Is(err, bundle.ErrRetained):
		return NewError(409, "export_retained", err.Error())
	case errors.Is(err, bundle.ErrLimit), errors.Is(err, store.ErrQuerySnapshotTooLarge):
		return NewError(413, "export_limit", err.Error())
	case errors.Is(err, bundle.ErrExpired), errors.Is(err, store.ErrSnapshotGone):
		return NewError(410, "export_expired", err.Error())
	case errors.Is(err, bundle.ErrConflict), errors.Is(err, bundle.ErrFenced):
		return NewError(409, "export_conflict", err.Error())
	case errors.Is(err, bundle.ErrUnavailable):
		return NewError(422, "export_role_unavailable", err.Error())
	default:
		var problem *Error
		if errors.As(FromStoreError(err), &problem) && problem.Status < 500 {
			return problem
		}
		return NewError(500, "export_failed", "export operation failed")
	}
}

func registerExportRoutes(mux *http.ServeMux, api huma.API, d Deps, g *OperationGate, snapshots *store.QuerySnapshotService, downloads *webDownloadRegistry, sessions *webSessionRegistry) {
	type sourceOutput struct{ Body bundle.Source }
	huma.Register(api, huma.Operation{OperationID: "createExportSource", Method: http.MethodPost, Path: "/api/v1/exports/sources", Summary: "Freeze exact export membership or begin a chunk upload", MaxBodyBytes: 1 << 20}, func(ctx context.Context, in *struct {
		Body    bundle.SourceRequest
		RawBody []byte
	}) (*sourceOutput, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		var request bundle.SourceRequest
		if err = json.Unmarshal(in.RawBody, &request, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "validation", "invalid export source fields")
		}
		var resolver func(context.Context) ([]bundle.Member, error)
		var queryFingerprint string
		if request.Kind == "query" || request.Kind == "saved_query" || request.Kind == "snapshot" {
			resolver = func(ctx context.Context) ([]bundle.Member, error) {
				snapshotOwner, _ := workspaceSnapshotOwner(ctx)
				id, hash := request.SnapshotID, request.MemberHash
				if request.Kind != "snapshot" {
					var page store.SnapshotPage
					var err error
					if request.Kind == "query" && request.Query != nil {
						page, err = snapshots.Create(ctx, snapshotOwner, store.SnapshotRequest{Query: *request.Query})
					} else {
						_, page, err = snapshots.RunSaved(ctx, snapshotOwner, request.SavedQueryID, request.SavedQueryRevision, store.SnapshotRequest{})
					}
					if err != nil {
						return nil, err
					}
					id, hash = page.SnapshotID, page.MemberHash
					queryFingerprint = page.QueryFingerprint
				}
				members, err := snapshots.CopyMembersBounded(ctx, snapshotOwner, id, hash, bundle.MaxMembers)
				if err != nil {
					return nil, err
				}
				out := make([]bundle.Member, len(members))
				for i, m := range members {
					out[i] = bundle.Member{NodeID: m.NodeID, VersionID: m.ContentVersionID, SHA256: m.BlobHash, Size: m.Size}
				}
				return out, nil
			}
		}
		var source bundle.Source
		err = g.MutateContext(ctx, func() error {
			var err error
			if resolver == nil {
				source, err = d.Store.CreateExportSource(ctx, owner, request, nil)
			} else {
				source, err = d.Store.CreateResolvedExportSource(ctx, owner, request, func(ctx context.Context) ([]bundle.Member, string, error) {
					members, err := resolver(ctx)
					return members, queryFingerprint, err
				})
			}
			return err
		})
		if err != nil {
			return nil, exportProblem(err)
		}
		return &sourceOutput{Body: source}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "putExportChunk", Method: http.MethodPut, Path: "/api/v1/exports/sources/{id}/chunks/{index}", Summary: "Upload one idempotent exact-member chunk", MaxBodyBytes: 1 << 20}, func(ctx context.Context, in *struct {
		ID    string `path:"id"`
		Index int    `path:"index"`
		Body  struct {
			Members []bundle.Member `json:"members"`
		}
		RawBody []byte
	}) (*struct{}, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(in.RawBody, &in.Body, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "validation", "invalid export chunk")
		}
		err = g.MutateContext(ctx, func() error { return d.Store.PutExportChunk(ctx, owner, in.ID, in.Index, in.Body.Members) })
		if err != nil {
			return nil, exportProblem(err)
		}
		return &struct{}{}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "sealExportSource", Method: http.MethodPost, Path: "/api/v1/exports/sources/{id}/seal", Summary: "Verify and seal the complete uploaded membership", MaxBodyBytes: 1024}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct{}
	}) (*sourceOutput, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		var source bundle.Source
		err = g.MutateContext(ctx, func() error { var err error; source, err = d.Store.SealExportSource(ctx, owner, in.ID); return err })
		if err != nil {
			return nil, exportProblem(err)
		}
		return &sourceOutput{Body: source}, nil
	})
	type planOutput struct{ Body bundle.Plan }
	huma.Register(api, huma.Operation{OperationID: "createExportPlan", Method: http.MethodPost, Path: "/api/v1/exports/plans", Summary: "Freeze role availability and exact archive paths", MaxBodyBytes: 8192}, func(ctx context.Context, in *struct {
		Body    bundle.PlanRequest
		RawBody []byte
	}) (*planOutput, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(in.RawBody, &in.Body, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "validation", "invalid export plan")
		}
		var p bundle.Plan
		err = g.MutateContext(ctx, func() error { var err error; p, err = d.Store.CreateExportPlan(ctx, owner, in.Body); return err })
		if err != nil {
			return nil, exportProblem(err)
		}
		return &planOutput{Body: p}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getExportPlan", Method: http.MethodGet, Path: "/api/v1/exports/plans/{id}", Summary: "Read an immutable export plan header"}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*planOutput, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		p, err := d.Store.ExportPlan(ctx, owner, in.ID)
		if err != nil {
			return nil, exportProblem(err)
		}
		return &planOutput{Body: p}, nil
	})
	type jobOutput struct{ Body bundle.Job }
	type previewOutput struct{ Body bundle.PlanPreview }
	huma.Register(api, huma.Operation{OperationID: "getExportPlanPreview", Method: http.MethodGet, Path: "/api/v1/exports/plans/{id}/preview", Summary: "Summarize frozen export role availability"}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*previewOutput, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		p, err := d.Store.ExportPlanPreview(ctx, owner, in.ID)
		if err != nil {
			return nil, exportProblem(err)
		}
		return &previewOutput{Body: p}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "createExportJob", Method: http.MethodPost, Path: "/api/v1/exports/jobs", Summary: "Admit a durable verified export job", MaxBodyBytes: 4096}, func(ctx context.Context, in *struct {
		Body    bundle.JobRequest
		RawBody []byte
	}) (*jobOutput, error) {
		if d.Exports == nil {
			return nil, NewError(503, "export_unavailable", "export worker unavailable")
		}
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(in.RawBody, &in.Body, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "validation", "invalid export job")
		}
		var job bundle.Job
		admit := func() error {
			return g.MutateContext(ctx, func() error { var err error; job, err = d.Store.QueueExportJob(ctx, owner, in.Body); return err })
		}
		if browserSessionRequest(ctx) {
			active, e := sessions.withActiveOwner(owner, admit)
			err = e
			if !active && err == nil {
				err = bundle.ErrExpired
			}
		} else {
			err = admit()
		}
		if err != nil {
			return nil, exportProblem(err)
		}
		return &jobOutput{Body: job}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "getExportJob", Method: http.MethodGet, Path: "/api/v1/exports/jobs/{id}", Summary: "Read current durable export progress and receipt"}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*jobOutput, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		j, err := d.Store.ExportJob(ctx, owner, in.ID)
		if err != nil {
			return nil, exportProblem(err)
		}
		return &jobOutput{Body: j}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "cancelExportJob", Method: http.MethodPost, Path: "/api/v1/exports/jobs/{id}/cancel", Summary: "Cancel and fence a running export", MaxBodyBytes: 1024}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct{}
	}) (*struct{}, error) {
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if err = g.MutateContext(ctx, func() error { return d.Store.CancelExportJob(ctx, owner, in.ID) }); err != nil {
			return nil, exportProblem(err)
		}
		return &struct{}{}, nil
	})
	type ticketOutput struct {
		Body struct {
			URL     string         `json:"url"`
			Receipt bundle.Receipt `json:"receipt"`
		}
	}
	huma.Register(api, huma.Operation{OperationID: "downloadExportArchive", Method: http.MethodPost, Path: "/api/v1/exports/jobs/{id}/download", Summary: "Issue a one-use ticket for a reverified archive", MaxBodyBytes: 1024}, func(ctx context.Context, in *struct {
		ID      string `path:"id"`
		Body    bundle.DownloadRequest
		RawBody []byte
	}) (*ticketOutput, error) {
		if d.Exports == nil {
			return nil, NewError(503, "export_unavailable", "export worker unavailable")
		}
		owner, err := exportOwner(ctx)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(in.RawBody, &in.Body, json.RejectUnknownMembers(true)); err != nil {
			return nil, NewError(400, "validation", "invalid export download fields")
		}
		name, err := bundle.DownloadBasename(in.Body.Basename)
		if err != nil {
			return nil, NewError(400, "validation", err.Error())
		}
		file, receipt, release, err := d.Exports.Lease(ctx, owner, in.ID)
		if err != nil {
			return nil, exportProblem(err)
		}
		ticket := webDownloadTicket{name: name, mediaType: "application/zip", blobHash: receipt.SHA256, size: receipt.Size, owner: owner, archiveFile: file, releaseArchive: release, planFingerprint: receipt.PlanFingerprint}
		var token string
		if browserSessionRequest(ctx) {
			active, e := sessions.withActiveOwner(owner, func() error { var e error; token, e = downloads.issue(ticket); return e })
			err = e
			if !active && err == nil {
				err = bundle.ErrExpired
			}
		} else {
			token, err = downloads.issue(ticket)
		}
		if err != nil {
			release()
			return nil, exportProblem(err)
		}
		out := &ticketOutput{}
		out.Body.URL = webDownloadFilePath + "?ticket=" + token
		out.Body.Receipt = receipt
		return out, nil
	})
	mux.HandleFunc("GET /api/v1/exports/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		owner, err := exportOwner(r.Context())
		if err != nil {
			writeError(w, NewError(401, "unauthorized", "authenticated export owner required"))
			return
		}
		id := r.PathValue("id")
		job, err := d.Store.ExportJob(r.Context(), owner, id)
		if err != nil {
			writeError(w, exportProblem(err))
			return
		}
		after := int64(0)
		if value := r.URL.Query().Get("after"); value != "" {
			after, err = strconv.ParseInt(value, 10, 64)
			if err != nil || after < 0 {
				writeError(w, NewError(400, "validation", "invalid progress sequence"))
				return
			}
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		flusher, _ := w.(http.Flusher)
		report := func(j bundle.Job) error {
			err := json.MarshalWrite(w, struct {
				Delivery       string     `json:"delivery"`
				RequestedAfter int64      `json:"requested_after"`
				Job            bundle.Job `json:"job"`
			}{"current_state", after, j})
			if err == nil {
				_, err = w.Write([]byte{'\n'})
			}
			if flusher != nil {
				flusher.Flush()
			}
			return err
		}
		if report(job) != nil {
			return
		}
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for job.State == "queued" || job.State == "running" {
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
				return
			case <-ticker.C:
				next, err := d.Store.ExportJob(r.Context(), owner, id)
				if err != nil {
					return
				}
				if next.Sequence != job.Sequence {
					if report(next) != nil {
						return
					}
					job = next
				}
			}
		}
	})
}

func exportBrowserRouteAllowed(r *http.Request) bool {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/exports/")
	parts := strings.Split(path, "/")
	if len(parts) == 1 {
		return r.URL.RawQuery == "" && r.Method == http.MethodPost && (parts[0] == "sources" || parts[0] == "plans" || parts[0] == "jobs")
	}
	if len(parts) < 2 || !validPageJobPathID(parts[1]) {
		return false
	}
	if len(parts) == 2 {
		return r.URL.RawQuery == "" && r.Method == http.MethodGet && (parts[0] == "plans" || parts[0] == "jobs")
	}
	if len(parts) == 3 {
		if parts[0] == "plans" && parts[2] == "preview" {
			return r.Method == http.MethodGet && r.URL.RawQuery == ""
		}
		if parts[0] == "sources" && parts[2] == "seal" {
			return r.Method == http.MethodPost && r.URL.RawQuery == ""
		}
		if parts[0] == "jobs" {
			if parts[2] == "events" {
				q := r.URL.Query()
				return r.Method == http.MethodGet && (r.URL.RawQuery == "" || len(q) == 1 && len(q["after"]) == 1)
			}
			return r.Method == http.MethodPost && r.URL.RawQuery == "" && (parts[2] == "cancel" || parts[2] == "download")
		}
	}
	if len(parts) == 4 && parts[0] == "sources" && parts[2] == "chunks" {
		index, err := strconv.Atoi(parts[3])
		return err == nil && index >= 0 && index < 100 && r.Method == http.MethodPut && r.URL.RawQuery == ""
	}
	return false
}
