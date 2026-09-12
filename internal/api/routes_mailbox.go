package api

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/mailbox"
	"go.kenn.io/docbank/internal/store"
)

const mailboxJSONLimit = 1 << 20

func mailboxBrowserRequestAllowed(r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/mailbox/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/api/v1/mailbox/") || len(parts) < 1 || len(parts) > 4 {
		return false
	}
	if r.Method != http.MethodGet && r.URL.RawQuery != "" {
		return false
	}
	switch parts[0] {
	case "containers":
		if len(parts) == 1 {
			return r.Method == http.MethodPost
		}
		if parts[1] == "" {
			return false
		}
		if len(parts) == 2 {
			return r.Method == http.MethodGet || r.Method == http.MethodDelete
		}
		if len(parts) == 3 && (parts[2] == "seal" || parts[2] == "preview") {
			return r.Method == http.MethodPost
		}
		return false // Browser bytes require the ownership-proved socket.
	case "jobs":
		if len(parts) == 1 {
			return r.Method == http.MethodGet || r.Method == http.MethodPost
		}
		if parts[1] == "" {
			return false
		}
		if len(parts) == 2 {
			return r.Method == http.MethodGet
		}
		if len(parts) != 3 {
			return false
		}
		return (r.Method == http.MethodGet && (parts[2] == "occurrences" || parts[2] == "events")) || (r.Method == http.MethodPost && (parts[2] == "cancel" || parts[2] == "resume"))
	}
	return false
}
func readMailboxJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, mailboxJSONLimit)
	b, err := io.ReadAll(r.Body)
	if err == nil && (len(b) == 0 || strings.TrimSpace(string(b)) == "null") {
		err = store.ErrMailboxInvalid
	}
	if err == nil {
		err = json.Unmarshal(b, out, json.RejectUnknownMembers(true))
	}
	if err != nil {
		writeMailboxError(w, err)
		return false
	}
	return true
}
func writeMailboxError(w http.ResponseWriter, err error) {
	status := 422
	code := "mailbox_invalid"
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = 404
		code = "not_found"
	case errors.Is(err, store.ErrMailboxConflict), errors.Is(err, store.ErrStaleRevision):
		status = 409
		code = "mailbox_conflict"
	case errors.Is(err, store.ErrMailboxLimit):
		code = "mailbox_limit"
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		status = 413
		code = "too_large"
	}
	writeError(w, NewError(status, code, err.Error()))
}
func mailboxOwner(d Deps) string { return "vault:" + d.Store.VaultID() }
func mailboxService(d Deps) *mailbox.Service {
	return &mailbox.Service{Store: d.Store, Blobs: d.Blobs, Spool: filepath.Join(d.VaultRoot, "blobs", "tmp")}
}
func mailboxPage(r *http.Request) (string, int, error) {
	limit := 100
	after := r.URL.Query().Get("after")
	for k, v := range r.URL.Query() {
		if len(v) != 1 || (k != "after" && k != "limit") {
			return "", 0, store.ErrMailboxInvalid
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		limit, err = strconv.Atoi(v)
		if err != nil {
			return "", 0, store.ErrMailboxInvalid
		}
	}
	if limit < 1 || limit > 100 || len(after) > 128 {
		return "", 0, store.ErrMailboxLimit
	}
	return after, limit, nil
}
func registerMailboxRoutes(mux *http.ServeMux, api huma.API, d Deps, g *gate) {
	service := mailboxService(d)
	mux.HandleFunc("POST /api/v1/mailbox/containers", func(w http.ResponseWriter, r *http.Request) {
		var request store.MailboxContainerRequest
		if !readMailboxJSON(w, r, &request) {
			return
		}
		request.Owner = mailboxOwner(d)
		var c store.MailboxContainer
		err := g.mutate(func() error { var err error; c, err = d.Store.BeginMailboxContainer(r.Context(), request); return err })
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 201, c)
	})
	mux.HandleFunc("GET /api/v1/mailbox/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, err := d.Store.MailboxContainer(r.Context(), mailboxOwner(d), r.PathValue("id"))
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, c)
	})
	mux.HandleFunc("PUT /api/v1/mailbox/containers/{id}/chunks/{index}", func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(r.PathValue("index"))
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		size, err := strconv.ParseInt(r.Header.Get(BlobSizeHeader), 10, 64)
		if err != nil || size < 1 || size > store.MailboxChunkBytes {
			writeMailboxError(w, store.ErrMailboxInvalid)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, size+1)
		err = g.mutate(func() error {
			return service.UploadChunk(r.Context(), mailboxOwner(d), r.PathValue("id"), index, r.Header.Get(BlobHashHeader), size, r.Body)
		})
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, store.MailboxChunk{Index: index, SHA256: r.Header.Get(BlobHashHeader), Size: size})
	})
	mux.HandleFunc("POST /api/v1/mailbox/containers/{id}/seal", func(w http.ResponseWriter, r *http.Request) {
		var c store.MailboxContainer
		err := g.mutate(func() error {
			var err error
			c, err = service.Seal(r.Context(), mailboxOwner(d), r.PathValue("id"))
			return err
		})
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, c)
	})
	mux.HandleFunc("DELETE /api/v1/mailbox/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := g.mutate(func() error { return d.Store.AbortMailboxContainer(r.Context(), mailboxOwner(d), r.PathValue("id")) })
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/mailbox/containers/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Dialect string `json:"dialect"`
		}
		if !readMailboxJSON(w, r, &request) {
			return
		}
		out, err := service.Preview(r.Context(), mailboxOwner(d), r.PathValue("id"), request.Dialect)
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/mailbox/jobs", func(w http.ResponseWriter, r *http.Request) {
		var request store.MailboxJobRequest
		if !readMailboxJSON(w, r, &request) {
			return
		}
		var j store.MailboxJob
		err := g.mutate(func() error {
			var err error
			j, err = d.Store.BeginMailboxJob(r.Context(), mailboxOwner(d), request)
			return err
		})
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 201, j)
	})
	mux.HandleFunc("GET /api/v1/mailbox/jobs", func(w http.ResponseWriter, r *http.Request) {
		after, limit, err := mailboxPage(r)
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		out, err := d.Store.MailboxJobs(r.Context(), mailboxOwner(d), after, limit)
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET /api/v1/mailbox/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := d.Store.MailboxJob(r.Context(), mailboxOwner(d), r.PathValue("id"))
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, j)
	})
	mux.HandleFunc("GET /api/v1/mailbox/jobs/{id}/occurrences", func(w http.ResponseWriter, r *http.Request) {
		after, limit, err := mailboxPage(r)
		var ordinal int64
		if after != "" && err == nil {
			ordinal, err = strconv.ParseInt(after, 10, 64)
		}
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		out, err := d.Store.MailboxOccurrences(r.Context(), mailboxOwner(d), r.PathValue("id"), ordinal, limit)
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/mailbox/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		err := g.mutate(func() error { return d.Store.CancelMailboxJob(r.Context(), mailboxOwner(d), r.PathValue("id")) })
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/mailbox/jobs/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Request      store.MailboxJobRequest `json:"request"`
			Continuation bool                    `json:"continuation"`
		}
		if !readMailboxJSON(w, r, &input) {
			return
		}
		if input.Request.ID != r.PathValue("id") {
			writeMailboxError(w, store.ErrMailboxConflict)
			return
		}
		var j store.MailboxJob
		err := g.mutate(func() error {
			var err error
			j, err = d.Store.ResumeMailboxJob(r.Context(), mailboxOwner(d), input.Request, input.Continuation)
			return err
		})
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, j)
	})
	registerMailboxTransferRoutes(mux, d, g, service)
	registerMailboxEvents(mux, d)
	registerMailboxOpenAPI(api)
}
func registerMailboxTransferRoutes(mux *http.ServeMux, d Deps, g *gate, service *mailbox.Service) {
	mux.HandleFunc("POST /api/v1/mailbox/archives", func(w http.ResponseWriter, r *http.Request) {
		var a store.MailboxArchive
		if !readMailboxJSON(w, r, &a) {
			return
		}
		a.Owner = mailboxOwner(d)
		err := g.mutate(func() error { return d.Store.RegisterMailboxArchive(r.Context(), a) })
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 201, a)
	})
	mux.HandleFunc("POST /api/v1/mailbox/transfers", func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("X-Docbank-Transfer")
		if len(header) > 32768 {
			writeMailboxError(w, store.ErrMailboxLimit)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(header)
		var request store.MailboxTransferRequest
		if err == nil {
			err = json.Unmarshal(raw, &request, json.RejectUnknownMembers(true))
		}
		if err != nil || request.Size < 1 || request.Size > 128<<20 {
			writeMailboxError(w, store.ErrMailboxInvalid)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, request.Size+1)
		var receipt store.MailboxTransferReceipt
		err = g.mutate(func() error {
			var err error
			receipt, err = service.Transfer(r.Context(), mailboxOwner(d), request, r.Body)
			return err
		})
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		writeJSON(w, 200, receipt)
	})
}
