package api

import (
	"net/http"

	"go.kenn.io/docbank/internal/loadfile"
	"go.kenn.io/docbank/internal/store"
)

func handlePackageContainerPreflight(w http.ResponseWriter, r *http.Request, d Deps, g *gate) {
	var request PackagePreflightRequest
	if problem := readPackageJSON(w, r, &request); problem != nil {
		writeError(w, problem)
		return
	}
	id := r.PathValue("id")
	if request.SourceKind != "container" || request.SourceRef != id || id == "" {
		writeError(w, NewError(http.StatusUnprocessableEntity, "package_reference_unsafe", "preflight source must name this sealed container"))
		return
	}
	owner, problem := packageContainerOwner(r, d)
	if problem != nil {
		writeError(w, problem)
		return
	}
	container, err := d.Store.MailboxContainer(r.Context(), owner, id)
	if err != nil {
		writePackageContainerError(w, err)
		return
	}
	if container.Format != "zip" || container.State != "sealed" {
		writePackageContainerError(w, store.ErrMailboxConflict)
		return
	}
	reader, err := mailboxService(d).ReaderAt(r.Context(), container)
	if err != nil {
		writePackageContainerError(w, err)
		return
	}
	root, err := loadfile.ExtractZIP(r.Context(), reader, container.Size)
	if err != nil {
		writePackageError(w, err)
		return
	}
	defer func() { _ = loadfile.RemoveExtractedZIP(root) }()
	preview, err := buildPackagePreflightFromRoot(r.Context(), d, g, owner, request, root, id)
	if err != nil {
		writePackageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}
