package api

import (
	"errors"
	"net/http"
	"strconv"

	"go.kenn.io/docbank/internal/store"
)

type packageContainerInput struct {
	ContainerID string `json:"container_id"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
}

func packageContainerOutput(c store.MailboxContainer) PackageContainer {
	return PackageContainer{ContainerID: c.ID, Format: c.Format, State: c.State,
		SHA256: c.SHA256, Size: c.Size, CreatedAt: c.CreatedAt}
}

// packageContainerOwner uses a durable vault identity for API-key requests.
// Browser sessions retain their separate credential-scoped ownership.
func packageContainerOwner(r *http.Request, d Deps) (string, *Error) {
	if !browserSessionRequest(r.Context()) {
		return mailboxOwner(d), nil
	}
	owner, ok := workspaceSnapshotOwner(r.Context())
	if !ok {
		return "", NewError(http.StatusInternalServerError, "internal", "authenticated request owner is unavailable")
	}
	return owner, nil
}

func packageOwnedContainer(r *http.Request, d Deps) (store.MailboxContainer, error) {
	owner, problem := packageContainerOwner(r, d)
	if problem != nil {
		return store.MailboxContainer{}, problem
	}
	c, err := d.Store.MailboxContainer(r.Context(), owner, r.PathValue("id"))
	if err == nil && c.Format != "zip" {
		return c, store.ErrNotFound
	}
	return c, err
}

func writePackageContainerError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrMailboxInvalid) || errors.Is(err, store.ErrMailboxLimit) || errors.Is(err, store.ErrMailboxConflict) || errors.Is(err, store.ErrNotFound) {
		writeMailboxError(w, err)
		return
	}
	writePackageError(w, err)
}

func registerPackageContainerRoutes(mux *http.ServeMux, d Deps, g *gate) {
	service := mailboxService(d)
	mux.HandleFunc("POST /api/v1/packages/containers", func(w http.ResponseWriter, r *http.Request) {
		var input packageContainerInput
		if problem := readPackageJSON(w, r, &input); problem != nil {
			writeError(w, problem)
			return
		}
		owner, problem := packageContainerOwner(r, d)
		if problem != nil {
			writeError(w, problem)
			return
		}
		var c store.MailboxContainer
		err := g.mutate(func() error {
			var err error
			c, err = d.Store.BeginMailboxContainer(r.Context(), store.MailboxContainerRequest{
				ID: input.ContainerID, Owner: owner, SHA256: input.SHA256, Size: input.Size, Format: "zip",
			})
			return err
		})
		if err != nil {
			writePackageContainerError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, packageContainerOutput(c))
	})
	mux.HandleFunc("GET /api/v1/packages/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, err := packageOwnedContainer(r, d)
		if err != nil {
			writePackageContainerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, packageContainerOutput(c))
	})
	mux.HandleFunc("PUT /api/v1/packages/containers/{id}/chunks/{index}", func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(r.PathValue("index"))
		if err != nil || index < 0 || index >= store.MailboxMaxChunks {
			writePackageContainerError(w, store.ErrMailboxInvalid)
			return
		}
		size, err := strconv.ParseInt(r.Header.Get(BlobSizeHeader), 10, 64)
		if err != nil || size < 1 || size > store.MailboxChunkBytes {
			writePackageContainerError(w, store.ErrMailboxInvalid)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, size+1)
		var c store.MailboxContainer
		err = g.mutate(func() error {
			var err error
			c, err = packageOwnedContainer(r, d)
			if err != nil {
				return err
			}
			return service.UploadChunk(r.Context(), c.Owner, c.ID, index, r.Header.Get(BlobHashHeader), size, r.Body)
		})
		if err != nil {
			writePackageContainerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, store.MailboxChunk{Index: index, SHA256: r.Header.Get(BlobHashHeader), Size: size})
	})
	mux.HandleFunc("POST /api/v1/packages/containers/{id}/seal", func(w http.ResponseWriter, r *http.Request) {
		var c store.MailboxContainer
		err := g.mutate(func() error {
			var err error
			c, err = packageOwnedContainer(r, d)
			if err != nil {
				return err
			}
			c, err = service.Seal(r.Context(), c.Owner, c.ID)
			return err
		})
		if err != nil {
			writePackageContainerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, packageContainerOutput(c))
	})
	mux.HandleFunc("DELETE /api/v1/packages/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := g.mutate(func() error {
			c, err := packageOwnedContainer(r, d)
			if err != nil {
				return err
			}
			return d.Store.AbortMailboxContainer(r.Context(), c.Owner, c.ID)
		})
		if err != nil {
			writePackageContainerError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
