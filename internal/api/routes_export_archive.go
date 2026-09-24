package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/store"
)

type exportArchiveLease func(context.Context, string, string) (*os.File, bundle.Receipt, func(), error)

const exportArchiveProblemMediaType = "application/problem+json"

func registerExportArchiveRead(mux *http.ServeMux, api huma.API, d Deps) {
	registry := api.OpenAPI().Components.Schemas
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "readExportArchive", Method: http.MethodGet,
		Path:    "/api/v1/exports/jobs/{id}/archive",
		Summary: "Read a completed, reverified export archive",
		Parameters: []*huma.Param{{Name: "id", In: openAPIPathLocation, Required: true,
			Schema: &huma.Schema{Type: openAPIStringType, Format: "uuid"}}},
		Responses: map[string]*huma.Response{
			"200": {Description: "Verified ZIP archive", Content: map[string]*huma.MediaType{
				"application/zip": {Schema: &huma.Schema{Type: openAPIStringType, Format: openAPIBinaryFormat}},
			}},
			"default": {Description: "Export request failed", Content: map[string]*huma.MediaType{
				exportArchiveProblemMediaType: {Schema: huma.SchemaFromType(registry, reflect.TypeFor[Error]())},
			}},
		},
	})
	mux.HandleFunc("GET /api/v1/exports/jobs/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		if d.Store == nil || d.Exports == nil {
			writeError(w, NewError(http.StatusServiceUnavailable, "export_unavailable", "export worker unavailable"))
			return
		}
		owner, err := exportOwner(r.Context())
		if err != nil {
			writeError(w, exportProblem(err))
			return
		}
		serveExportArchiveRead(w, r, d.Store, owner, r.PathValue("id"), d.Exports.Lease)
	})
}

func archiveReadProblem(err error) *Error {
	if errors.Is(err, store.ErrExportVisibilityChanged) {
		return NewError(http.StatusConflict, "visibility_changed", "export source visibility changed")
	}
	return exportProblem(err)
}

func checkExportArchiveRead(ctx context.Context, catalog *store.Store, owner, id string) (bundle.Job, error) {
	job, err := catalog.ExportJob(ctx, owner, id)
	if err != nil {
		return bundle.Job{}, err
	}
	if job.State != "completed" || job.Receipt == nil {
		return bundle.Job{}, bundle.ErrConflict
	}
	plan, err := catalog.ExportPlan(ctx, owner, job.PlanID)
	if err != nil {
		return bundle.Job{}, err
	}
	if plan.Fingerprint != job.Fingerprint || plan.Source.ID == "" {
		return bundle.Job{}, bundle.ErrConflict
	}
	if err = catalog.CheckExportPlanVisibility(ctx, owner, plan.ID); err != nil {
		return bundle.Job{}, err
	}
	return job, nil
}

// serveExportArchiveRead completes all checks before committing a success
// response. The second authority read catches withdrawal during full-byte
// preverification; a failed check returns a problem with no archive bytes.
func serveExportArchiveRead(w http.ResponseWriter, r *http.Request, catalog *store.Store, owner, id string, lease exportArchiveLease) {
	if !validPageJobPathID(id) {
		writeError(w, NewError(http.StatusNotFound, "not_found", "not found"))
		return
	}
	if _, err := checkExportArchiveRead(r.Context(), catalog, owner, id); err != nil {
		writeError(w, archiveReadProblem(err))
		return
	}
	file, receipt, release, err := lease(r.Context(), owner, id)
	if err != nil {
		writeError(w, archiveReadProblem(err))
		return
	}
	defer release()
	if receipt.Size < 1 || receipt.Size > bundle.MaxArchiveBytes {
		writeError(w, archiveReadProblem(bundle.ErrInvalidArchive))
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeError(w, archiveReadProblem(err))
		return
	}
	job, err := checkExportArchiveRead(r.Context(), catalog, owner, id)
	if err != nil {
		writeError(w, archiveReadProblem(err))
		return
	}
	if *job.Receipt != receipt || receipt.PlanFingerprint != job.Fingerprint {
		writeError(w, archiveReadProblem(bundle.ErrConflict))
		return
	}
	if err = r.Context().Err(); err != nil {
		writeError(w, archiveReadProblem(err))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="docbank-export.zip"`)
	w.Header().Set("Content-Length", strconv.FormatInt(receipt.Size, 10))
	w.Header().Set("Docbank-Plan-Fingerprint", receipt.PlanFingerprint)
	w.Header().Set("Docbank-Archive-Sha256", receipt.SHA256)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = copyExportArchive(r.Context(), w, file, receipt.Size)
}

// copyExportArchive never reads past the retained receipt size. It checks
// cancellation on every bounded chunk, including after a short write.
func copyExportArchive(ctx context.Context, dst io.Writer, file *os.File, size int64) error {
	if size < 0 || size > bundle.MaxArchiveBytes {
		return bundle.ErrLimit
	}
	const chunk = 256 << 10
	buffer := make([]byte, chunk)
	reader := io.NewSectionReader(file, 0, size)
	var copied int64
	for copied < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := min(size-copied, int64(chunk))
		n, err := io.ReadFull(reader, buffer[:want])
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		written, err := dst.Write(buffer[:n])
		copied += int64(written)
		if err != nil {
			return err
		}
		if written != n {
			return io.ErrShortWrite
		}
	}
	return ctx.Err()
}
