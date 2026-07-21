package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
	internalmaintenance "go.kenn.io/docbank/internal/maintenance"
)

func registerOpsRoutes(api huma.API, d Deps, g *gate) {
	type storageStatusOutput struct{ Body StorageStatus }
	huma.Register(api, huma.Operation{
		OperationID: "storageStatus", Method: http.MethodGet, Path: "/api/v1/storage",
		Summary: "Report loose and packed physical storage usage",
	}, func(ctx context.Context, _ *struct{}) (*storageStatusOutput, error) {
		stats, err := d.Blobs.Stats(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &storageStatusOutput{Body: StorageStatus{
			LooseBlobs: stats.LooseBlobs, LooseBytes: stats.LooseBytes,
			Packs: stats.Packs, PackStoredBytes: stats.PackStoredBytes,
			PackedBlobs: stats.PackedBlobs, PackedRawBytes: stats.PackedRawBytes,
			PackedStoredBytes: stats.PackedStoredBytes, DeadPackedBytes: stats.DeadPackedBytes,
		}}, nil
	})

	type storageRepackOutput struct{ Body StorageRepackReport }
	huma.Register(api, huma.Operation{
		OperationID: "storageRepack", Method: http.MethodPost, Path: "/api/v1/storage/repack",
		Summary: "Rewrite eligible sparse packs and retire dead pack files",
	}, func(ctx context.Context, in *struct {
		Body struct {
			MaxBytes     int64  `json:"max_bytes,omitempty" minimum:"0"`
			MinAge       string `json:"min_age,omitempty" example:"24h"`
			MinDeadBytes int64  `json:"min_dead_bytes,omitempty" minimum:"0"`
		}
	}) (*storageRepackOutput, error) {
		minAge, err := ParseAge(in.Body.MinAge)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		out := &storageRepackOutput{}
		err = g.maintain(func() error {
			report, err := runRepack(ctx, d, in.Body.MaxBytes, minAge, in.Body.MinDeadBytes)
			if err != nil {
				return FromMaintenanceError(err)
			}
			out.Body = storageRepackReport(report)
			return nil
		})
		return out, err
	})

	type storagePackOutput struct{ Body StoragePackReport }
	huma.Register(api, huma.Operation{
		OperationID: "storagePack", Method: http.MethodPost, Path: "/api/v1/storage/pack",
		Summary: "Pack authorized loose blobs into immutable pack files",
	}, func(ctx context.Context, in *struct {
		Body struct {
			MaxBytes int64 `json:"max_bytes,omitempty" minimum:"0"`
		}
	}) (*storagePackOutput, error) {
		out := &storagePackOutput{}
		err := g.maintain(func() error {
			report, err := internalmaintenance.Pack(ctx, d.Store, d.Blobs, in.Body.MaxBytes)
			if err != nil {
				return FromMaintenanceError(err)
			}
			out.Body = storagePackReport(report)
			return nil
		})
		return out, err
	})

	type ingestOutput struct{ Body IngestReport }
	type ingestPreflightOutput struct{ Body IngestPreflightReport }
	huma.Register(api, huma.Operation{
		OperationID: "preflightIngest", Method: http.MethodPost, Path: "/api/v1/ingest/preflight",
		Summary: "Inventory server-side files without opening content or mutating the vault",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Paths   []string `json:"paths" minItems:"1"`
			Exclude []string `json:"exclude,omitempty"`
		}
	}) (*ingestPreflightOutput, error) {
		if err := validateIngestPaths(in.Body.Paths); err != nil {
			return nil, err
		}
		opts := ingest.Options{Exclude: in.Body.Exclude}
		if err := ingest.ValidateOptions(opts); err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		report, err := ingest.Preflight(ctx, in.Body.Paths, opts)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &ingestPreflightOutput{Body: ingestPreflightReport(report)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "ingest", Method: http.MethodPost, Path: "/api/v1/ingest",
		Summary: "Import server-side files or directory trees (loopback callers only)",
	}, func(ctx context.Context, in *struct {
		Body ingestRequest
	}) (*ingestOutput, error) {
		dest, opts, err := ingestParams(in.Body)
		if err != nil {
			return nil, err
		}
		report, err := runIngest(ctx, d, g, in.Body.Paths, dest, opts)
		return &ingestOutput{Body: report}, err
	})

	ingestStreamSchema := api.OpenAPI().Components.Schemas.Schema(
		reflect.TypeFor[IngestEvent](), true, "IngestEvent")
	huma.Register(api, huma.Operation{
		OperationID: "streamIngest", Method: http.MethodPost, Path: "/api/v1/ingest/stream",
		Summary: "Import server-side paths while streaming structured progress",
		Description: "Returns newline-delimited JSON. Scan and ingest progress precede exactly one " +
			"terminal result or error event; an HTTP 200 only means the stream started.",
		Responses: map[string]*huma.Response{
			"200": {
				Description: "Ingest progress followed by one terminal result or error",
				Content: map[string]*huma.MediaType{
					"application/x-ndjson": {Schema: ingestStreamSchema},
				},
			},
		},
	}, func(_ context.Context, in *struct {
		Body ingestRequest
	}) (*huma.StreamResponse, error) {
		dest, opts, err := ingestParams(in.Body)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/x-ndjson")
			hctx.SetHeader("Cache-Control", "no-store")
			runCtx, cancel := context.WithCancel(hctx.Context())
			defer cancel()
			stream := newEventStreamWriter[IngestEvent](hctx.BodyWriter(), cancel)
			stream.send(IngestEvent{Type: "progress", Progress: &IngestProgress{Stage: "scan"}})
			preflight, scanErr := ingest.Preflight(runCtx, in.Body.Paths, opts)
			if stream.err() != nil {
				return
			}
			if scanErr != nil {
				stream.send(IngestEvent{Type: "error", Error: ingestProblem(scanErr)})
				return
			}
			stream.send(IngestEvent{Type: "progress", Progress: &IngestProgress{
				Stage: "scan", Done: preflight.Files, Total: preflight.Files,
				BytesDone: preflight.LogicalBytes, BytesTotal: preflight.LogicalBytes, Final: true,
			}})
			stream.send(IngestEvent{Type: "progress", Progress: &IngestProgress{
				Stage: "ingest", Total: preflight.Files, BytesTotal: preflight.LogicalBytes,
			}})
			if stream.err() != nil {
				return
			}
			opts.Progress = func(event ingest.ProgressEvent) {
				stream.send(IngestEvent{Type: "progress", Progress: &IngestProgress{
					Stage: "ingest", Done: event.FilesDone, Total: preflight.Files,
					BytesDone: event.BytesRead, BytesTotal: preflight.LogicalBytes,
					Added: event.Added, Skipped: event.Skipped, Excluded: event.Excluded,
					Failed: event.Failed, Final: event.Final,
				}})
			}
			report, ingestErr := runIngest(runCtx, d, g, in.Body.Paths, dest, opts)
			if stream.err() != nil {
				return
			}
			if ingestErr != nil {
				stream.send(IngestEvent{Type: "error", Error: ingestProblem(ingestErr)})
				return
			}
			stream.send(IngestEvent{Type: "result", Report: &report})
		}}, nil
	})

	type trashListOutput struct {
		Body struct {
			Items []Node `json:"items"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "listTrash", Method: http.MethodGet, Path: "/api/v1/trash",
		Summary: "List restorable trash roots, newest first",
	}, func(ctx context.Context, _ *struct{}) (*trashListOutput, error) {
		roots, err := d.Store.TrashedRoots(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &trashListOutput{}
		out.Body.Items = []Node{}
		for _, n := range roots {
			out.Body.Items = append(out.Body.Items, fromStoreNode(n))
		}
		return out, nil
	})

	type emptyOutput struct{ Body TrashEmptyReport }
	huma.Register(api, huma.Operation{
		OperationID: "emptyTrash", Method: http.MethodPost, Path: "/api/v1/trash/empty",
		Summary: "Report (run=false) or hard-delete (run=true) trash roots",
	}, func(ctx context.Context, in *struct {
		Body struct {
			OlderThan string `json:"older_than,omitempty" example:"30d"`
			Run       bool   `json:"run,omitempty" default:"false"`
		}
	}) (*emptyOutput, error) {
		age, err := ParseAge(in.Body.OlderThan)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		out := &emptyOutput{}
		err = g.maintain(func() error {
			rep, err := d.Store.TrashEmpty(ctx, age, in.Body.Run)
			if err != nil {
				return FromStoreError(err)
			}
			out.Body = TrashEmptyReport{
				CandidateRoots: rep.Candidates,
				Deleted:        rep.Deleted,
				Run:            rep.Run,
			}
			return nil
		})
		return out, err
	})

	type gcOutput struct{ Body GCReport }
	huma.Register(api, huma.Operation{
		OperationID: "gc", Method: http.MethodPost, Path: "/api/v1/gc",
		Summary: "Report (run=false) or reclaim (run=true) unreachable blobs",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Run bool `json:"run"`
		}
	}) (*gcOutput, error) {
		out := &gcOutput{}
		err := g.maintain(func() error {
			return d.Blobs.WithMutation(ctx, func() error {
				rep, err := runGC(ctx, d, in.Body.Run)
				if err != nil {
					return err
				}
				out.Body = rep
				return nil
			})
		})
		return out, err
	})

	type verifyOutput struct{ Body VerifyReport }
	huma.Register(api, huma.Operation{
		OperationID: "verify", Method: http.MethodPost, Path: "/api/v1/verify",
		Summary: "Validate metadata and re-hash every stored blob",
	}, func(ctx context.Context, _ *struct{}) (*verifyOutput, error) {
		out := &verifyOutput{}
		err := g.maintain(func() error {
			report, err := runVerify(ctx, d)
			out.Body = report
			return err
		})
		return out, err
	})
}

type ingestRequest struct {
	Paths   []string `json:"paths" minItems:"1"`
	Dest    string   `json:"dest" default:"/inbox"`
	Exclude []string `json:"exclude,omitempty"`
}

func ingestParams(body ingestRequest) (string, ingest.Options, error) {
	if err := validateIngestPaths(body.Paths); err != nil {
		return "", ingest.Options{}, err
	}
	opts := ingest.Options{Exclude: body.Exclude}
	if err := ingest.ValidateOptions(opts); err != nil {
		return "", ingest.Options{}, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
	}
	// The schema default covers an absent dest, not an explicit ""
	// (which MkdirAll would treat as the vault root).
	dest := body.Dest
	if dest == "" {
		dest = "/inbox"
	}
	if !strings.HasPrefix(dest, "/") {
		return "", ingest.Options{}, NewError(http.StatusUnprocessableEntity, "validation",
			fmt.Sprintf("dest %q must be an absolute virtual path (start with /)", dest))
	}
	return dest, opts, nil
}

func runIngest(
	ctx context.Context,
	d Deps,
	g *gate,
	paths []string,
	dest string,
	opts ingest.Options,
) (IngestReport, error) {
	var out IngestReport
	err := g.mutate(func() error {
		return d.Blobs.WithMutation(ctx, func() error {
			ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
			rep, err := ing.AddPathsWithOptions(ctx, paths, dest, opts)
			if err != nil {
				return FromStoreError(err)
			}
			out = IngestReport{Added: rep.Added, Skipped: rep.Skipped, Excluded: rep.Excluded}
			for _, failure := range rep.Failed {
				out.Failed = append(out.Failed, IngestFailure{
					Path: failure.Path, Error: failure.Err.Error(),
				})
			}
			return nil
		})
	})
	return out, err
}

func ingestProblem(err error) *Error {
	var problem *Error
	if errors.As(err, &problem) {
		return problem
	}
	mapped := FromStoreError(err)
	if errors.As(mapped, &problem) {
		return problem
	}
	return NewError(http.StatusInternalServerError, "internal", err.Error())
}

func validateIngestPaths(paths []string) error {
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("path %q must be absolute: the daemon has no meaningful working directory", path))
		}
	}
	return nil
}

func ingestPreflightReport(report ingest.PreflightReport) IngestPreflightReport {
	out := IngestPreflightReport{
		Files: report.Files, Directories: report.Directories, LogicalBytes: report.LogicalBytes,
		PackEligible: ingestSizeClass(report.PackEligible),
		LooseOnly:    ingestSizeClass(report.LooseOnly),
		Rejected:     ingestSizeClass(report.Rejected),
		Excluded:     report.Excluded, Skipped: report.Skipped, Errors: report.Errors,
		OtherFileTypes:     ingestSizeClass(report.OtherFileTypes),
		FileTypesTruncated: report.FileTypesTruncated, FindingsTruncated: report.FindingsTruncated,
		FileTypes: make([]IngestFileType, 0, len(report.FileTypes)),
		Findings:  make([]IngestPreflightFinding, 0, len(report.Findings)),
	}
	for _, fileType := range report.FileTypes {
		out.FileTypes = append(out.FileTypes, IngestFileType{
			Extension: fileType.Extension, Files: fileType.Files, Bytes: fileType.Bytes,
		})
	}
	for _, finding := range report.Findings {
		out.Findings = append(out.Findings, IngestPreflightFinding{
			Path: finding.Path, Kind: finding.Kind, Detail: finding.Detail,
		})
	}
	return out
}

func ingestSizeClass(class ingest.SizeClass) IngestSizeClass {
	return IngestSizeClass{Files: class.Files, Bytes: class.Bytes}
}

func storagePackReport(report internalmaintenance.PackReport) StoragePackReport {
	stats := report.Stats
	return StoragePackReport{
		PacksSealed: stats.PacksSealed, BlobsPacked: stats.BlobsPacked, BytesPacked: stats.BytesPacked,
		PacksAdopted: stats.PacksAdopted, PacksRemoved: stats.PacksRemoved,
		PacksQuarantined: stats.PacksQuarantined, PacksUnreadable: stats.PacksUnreadable,
		RecordsDropped: stats.RecordsDropped, MappingsPruned: stats.MappingsPruned,
		BlobsMissing: stats.BlobsMissing, BlobsCorrupt: stats.BlobsCorrupt,
		BlobsDeferredOversized: stats.BlobsDeferredOversized,
		PacksDeferredOversized: stats.PacksDeferredOversized,
		LooseSwept:             stats.LooseSwept, LooseOrphansRemoved: stats.LooseOrphansRemoved,
		LooseOrphanSweepSuppressed: stats.LooseOrphanSweepSuppressed,
		BudgetExhausted:            stats.BudgetExhausted,
		More:                       report.More,
	}
}

func storageRepackReport(stats internalmaintenance.RepackReport) StorageRepackReport {
	return StorageRepackReport{
		MappingsPruned: stats.MappingsPruned, PacksSelected: stats.PacksSelected,
		PacksRewritten: stats.PacksRewritten, PacksSealed: stats.PacksSealed,
		PacksRemoved: stats.PacksRemoved, PacksDeferredOversized: stats.PacksDeferredOversized,
		BlobsRepacked: stats.BlobsRepacked, BytesRepacked: stats.BytesRepacked,
		BudgetExhausted: stats.BudgetExhausted,
	}
}

func runRepack(
	ctx context.Context, d Deps, maxBytes int64, minAge time.Duration, minDeadBytes int64,
) (internalmaintenance.RepackReport, error) {
	var total internalmaintenance.RepackReport
	var cursor string
	for {
		remainingBytes := maxBytes
		if maxBytes > 0 {
			remainingBytes -= total.BytesRepacked
			if remainingBytes <= 0 {
				total.BudgetExhausted = true
				return total, nil
			}
		}
		page, err := internalmaintenance.Repack(ctx, d.Store, d.Blobs,
			internalmaintenance.RepackOptions{
				Budget: internalmaintenance.Budget{
					MaxObjects: internalmaintenance.DefaultMaxObjects,
					MaxBytes:   remainingBytes,
					Cursor:     cursor,
				},
				MinAge: minAge, MinDeadBytes: minDeadBytes,
			})
		addRepackReport(&total, page)
		if err != nil {
			return total, err
		}
		if !page.More {
			return total, nil
		}
		if maxBytes > 0 && total.BytesRepacked >= maxBytes {
			total.BudgetExhausted = true
			return total, nil
		}
		if page.NextCursor != "" {
			cursor = page.NextCursor
		}
	}
}

func addRepackReport(total *internalmaintenance.RepackReport, page internalmaintenance.RepackReport) {
	total.MappingsPruned += page.MappingsPruned
	total.PacksSelected += page.PacksSelected
	total.PacksRewritten += page.PacksRewritten
	total.PacksSealed += page.PacksSealed
	total.PacksRemoved += page.PacksRemoved
	total.PacksDeferredOversized += page.PacksDeferredOversized
	total.BlobsRepacked += page.BlobsRepacked
	total.BytesRepacked += page.BytesRepacked
	total.BudgetExhausted = total.BudgetExhausted || page.BudgetExhausted
}

// runGC ports cmd/gc.go's semantics: candidates from row reachability,
// untracked files from a shard scan (safe under the maintenance gate — no
// concurrent ingest can be mid-write), files removed before rows so a crash
// leaves reconcilable row-without-file state, never the reverse.
func runGC(ctx context.Context, d Deps, run bool) (GCReport, error) {
	report := GCReport{Run: run}
	var cursor string
	for {
		page, err := internalmaintenance.GarbageCollect(ctx, d.Store, d.Blobs,
			internalmaintenance.GCOptions{
				Budget: internalmaintenance.Budget{
					MaxObjects: internalmaintenance.DefaultMaxObjects, Cursor: cursor,
				},
				DryRun: !run,
			})
		if err != nil {
			return GCReport{}, FromStoreError(err)
		}
		report.CandidateBlobs += page.CandidateBlobs
		report.UntrackedFiles += page.UntrackedFiles
		report.ReclaimableBytes += page.ReclaimableBytes
		report.PendingPackedBlobs += page.PendingPackedBlobs
		report.PendingPackedBytes += page.PendingPackedBytes
		report.ReclaimedFiles += page.ReclaimedFiles
		report.RemovedBlobs += page.RemovedBlobs
		report.Removed += page.Removed
		if !page.More {
			return report, nil
		}
		if page.NextCursor == "" {
			return GCReport{}, NewError(http.StatusInternalServerError, "internal",
				"gc made no resumable progress")
		}
		cursor = page.NextCursor
	}
}

func runVerify(ctx context.Context, d Deps) (VerifyReport, error) {
	var report VerifyReport
	var cursor string
	for {
		page, err := internalmaintenance.Verify(ctx, d.Store, d.Blobs,
			internalmaintenance.VerifyOptions{Budget: internalmaintenance.Budget{
				MaxObjects: internalmaintenance.DefaultMaxObjects, Cursor: cursor,
			}})
		if err != nil {
			if ctx.Err() != nil {
				return VerifyReport{}, NewError(http.StatusInternalServerError, "internal",
					fmt.Sprintf("verify interrupted: %v", ctx.Err()))
			}
			return VerifyReport{}, FromStoreError(err)
		}
		report.OK += page.OK
		report.MetadataProblems = append(report.MetadataProblems, page.MetadataProblems...)
		for _, problem := range page.Problems {
			report.Problems = append(report.Problems,
				VerifyProblem{Hash: problem.Hash, Problem: problem.Problem})
		}
		if !page.More {
			return report, nil
		}
		if page.NextCursor == "" {
			return VerifyReport{}, NewError(http.StatusInternalServerError, "internal",
				"verify made no resumable progress")
		}
		cursor = page.NextCursor
	}
}

// checkBlob returns "", "missing", "corrupt", or "unreadable".
func checkBlob(ctx context.Context, d Deps, hash string) string {
	f, _, err := d.Blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return "unreadable"
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		if isContentCorruption(err) {
			return "corrupt"
		}
		return "unreadable"
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return "corrupt"
	}
	return ""
}
