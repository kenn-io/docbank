package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

type packageImportBinding struct {
	SourceKind        string `json:"source_kind"`
	SourceLocator     string `json:"source_locator"`
	Into              string `json:"into"`
	AcceptPartial     bool   `json:"accept_partial"`
	IndexSuppliedText bool   `json:"index_supplied_text"`
	Total             int    `json:"total"`
}

// PackageMutationGate coordinates package writes with the owning vault.
type PackageMutationGate interface {
	MutateContext(ctx context.Context, mutate func() error) error
}

func handlePackageImport(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	var request PackageImportRequest
	if problem := readPackageJSON(w, r, &request); problem != nil {
		writeError(w, problem)
		return
	}
	owner, problem := packageContainerOwner(r, d)
	if problem != nil {
		writeError(w, problem)
		return
	}
	result, err := AdmitPackageImport(r.Context(), d, g, owner, request)
	if err != nil {
		writePackageError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

// AdmitPackageImport validates and durably admits one idempotent import job.
func AdmitPackageImport(ctx context.Context, d Deps, g PackageMutationGate, owner string, request PackageImportRequest) (PackageImportJob, error) {
	if parsed, err := uuid.Parse(request.OperationID); err != nil || parsed.Version() != 4 ||
		request.PreflightID == "" || !validPackageName(request.Name) ||
		utf8.RuneCountInString(request.Party) > 64 || !strings.HasPrefix(request.Into, "/") {
		return PackageImportJob{}, NewError(http.StatusUnprocessableEntity, "validation",
			"a version-4 operation_id, preflight_id, package name and absolute destination are required")
	}
	requestHash, err := packageImportRequestHash(request)
	if err != nil {
		return PackageImportJob{}, err
	}
	// An exact retry stays usable after its short-lived preflight expires.
	existing, err := d.Store.PackageImportJob(ctx, owner, request.OperationID)
	if err == nil {
		if existing.RequestSHA256 != requestHash {
			return PackageImportJob{}, store.ErrPackageConflict
		}
		return packageImportStatus(ctx, d.Store, existing)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return PackageImportJob{}, err
	}
	preview, err := d.Store.PackagePreflight(ctx, owner, request.PreflightID)
	if err != nil {
		return PackageImportJob{}, err
	}
	if preview.Blocking {
		return PackageImportJob{}, store.ErrPackageConflict
	}
	var summary PackagePreflight
	if summary, err = packagePreflightFromRecord(preview); err != nil {
		return PackageImportJob{}, err
	}
	if summary.Records < 1 || summary.Records > maxPackageRecords {
		return PackageImportJob{}, store.ErrPackageConflict
	}
	into, err := d.Store.NodeByPath(ctx, request.Into)
	if err != nil {
		return PackageImportJob{}, err
	}
	if !into.IsDir() {
		return PackageImportJob{}, NewError(http.StatusUnprocessableEntity, "validation", "destination must be an existing folder")
	}
	var mapping loadfile.Mapping
	if err := json.Unmarshal([]byte(preview.MappingJSON), &mapping, json.RejectUnknownMembers(true)); err != nil {
		return PackageImportJob{}, err
	}
	volumes := make([]store.PackageVolume, len(summary.Volumes))
	for i, volume := range summary.Volumes {
		mapped := volume.DeclaredRoot
		if override, ok := mapping.VolumeRoots[volume.VolumeName]; ok {
			mapped = override
		}
		rootBinding, err := store.PackageVolumeRootBinding(preview.SourceRef, mapped)
		if err != nil {
			return PackageImportJob{}, err
		}
		volumes[i] = store.PackageVolume{Ordinal: volume.Ordinal, VolumeName: volume.VolumeName,
			DeclaredRoot: volume.DeclaredRoot, MappedRoot: mapped, ResolvedRootSHA256: rootBinding}
	}
	binding := packageImportBinding{SourceKind: preview.SourceKind, SourceLocator: preview.SourceLocator,
		Into: request.Into, AcceptPartial: request.AcceptPartial,
		IndexSuppliedText: request.IndexSuppliedText, Total: summary.Records}
	jobJSON, err := canonical.Marshal(binding)
	if err != nil {
		return PackageImportJob{}, err
	}
	var admitted store.PackageImportJob
	err = g.MutateContext(ctx, func() error {
		run, beginErr := d.Store.BeginIngest(ctx, "package:loadfile", preview.SourceRef)
		if beginErr != nil {
			return beginErr
		}
		pkg := store.PackageRequest{PackageID: uuid.NewString(), Direction: "received", State: "importing",
			PackageName: request.Name, PartyLabel: request.Party, IngestID: run.ID(),
			ProfileSHA256: preview.ProfileSHA256, ProfileJSON: preview.ProfileJSON,
			MappingSHA256: preview.MappingSHA256, MappingJSON: preview.MappingJSON,
			ManifestSHA256: preview.ManifestSHA256, ManifestBlobSHA256: preview.ManifestBlobSHA256,
			Volumes: volumes}
		admitted, beginErr = d.Store.AdmitPackageImport(ctx, run, pkg, store.PackageImportJobRequest{
			ID: uuid.NewString(), Owner: owner, OperationID: request.OperationID,
			RequestSHA256: requestHash, PreflightID: preview.PreflightID,
			PackageID: pkg.PackageID, JobJSON: jobJSON,
		})
		return beginErr
	})
	if err != nil {
		return PackageImportJob{}, err
	}
	return packageImportStatus(ctx, d.Store, admitted)
}

func validPackageName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func handlePackageImportStatus(w http.ResponseWriter, r *http.Request, d Deps) {
	owner, problem := packageContainerOwner(r, d)
	if problem != nil {
		writeError(w, problem)
		return
	}
	job, err := d.Store.PackageImportJob(r.Context(), owner, r.PathValue("operation_id"))
	if err != nil {
		writePackageError(w, err)
		return
	}
	writePackageImportStatus(r.Context(), w, d.Store, job, http.StatusOK)
}

func handlePackageImportCancel(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	owner, problem := packageContainerOwner(r, d)
	if problem != nil {
		writeError(w, problem)
		return
	}
	var job store.PackageImportJob
	err := g.MutateContext(r.Context(), func() error {
		var cancelErr error
		job, cancelErr = d.Store.CancelPackageImportJob(r.Context(), owner, r.PathValue("operation_id"))
		return cancelErr
	})
	if err != nil {
		writePackageError(w, err)
		return
	}
	writePackageImportStatus(r.Context(), w, d.Store, job, http.StatusOK)
}

func writePackageImportStatus(ctx context.Context, w http.ResponseWriter, catalog *store.Store, job store.PackageImportJob, status int) {
	result, err := packageImportStatus(ctx, catalog, job)
	if err != nil {
		writePackageError(w, err)
		return
	}
	writeJSON(w, status, result)
}

func packageImportStatus(ctx context.Context, catalog *store.Store, job store.PackageImportJob) (PackageImportJob, error) {
	var binding packageImportBinding
	if err := json.Unmarshal(job.JobJSON, &binding, json.RejectUnknownMembers(true)); err != nil {
		return PackageImportJob{}, err
	}
	progress, err := catalog.PackageImportProgress(ctx, job.PackageID)
	if err != nil {
		return PackageImportJob{}, err
	}
	return PackageImportJob{OperationID: job.OperationID, JobID: job.ID,
		PackageID: job.PackageID, PreflightID: job.PreflightID, State: job.State,
		Committed: progress.Committed, Total: binding.Total, GapCount: progress.GapCount, Gaps: progress.Gaps,
		CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt}, nil
}

// ReadPackageImport returns current progress for one owner-scoped operation.
func ReadPackageImport(ctx context.Context, catalog *store.Store, owner, operationID string) (PackageImportJob, error) {
	job, err := catalog.PackageImportJob(ctx, owner, operationID)
	if err != nil {
		return PackageImportJob{}, err
	}
	return packageImportStatus(ctx, catalog, job)
}
