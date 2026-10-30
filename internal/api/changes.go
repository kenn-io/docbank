package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

const changesPath = "/api/v1/changes"

func registerChanges(shutdown context.Context, mux *http.ServeMux, api huma.API, d Deps) {
	problem := &huma.Response{Description: "Invalid or expired cursor", Content: map[string]*huma.MediaType{new(Error).ContentType("application/json"): {Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[Error](), true, "")}}}
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "vaultChanges", Method: http.MethodGet, Path: changesPath, Summary: "Read resumable vault changes", Tags: []string{"Changes"},
		Parameters: []*huma.Param{{Name: "after", In: "query", Schema: &huma.Schema{Type: openAPIStringType}}, {Name: "follow", In: "query", Schema: &huma.Schema{Type: "boolean"}}},
		Responses: map[string]*huma.Response{
			"200": {Description: "One vault change per line", Content: map[string]*huma.MediaType{mailboxNDJSONMediaType: {Schema: &huma.Schema{Type: openAPIStringType}}}},
			"400": problem, "410": problem,
		},
	})
	mux.HandleFunc("GET "+changesPath, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(shutdown, cancel)
		defer stop()
		after := r.URL.Query().Get("after")
		follow := false
		if value := r.URL.Query().Get("follow"); value != "" {
			var err error
			follow, err = strconv.ParseBool(value)
			if err != nil {
				writeError(w, NewError(400, "validation", "follow must be a boolean"))
				return
			}
		}
		changes, head, err := d.Store.VaultChanges(ctx, after, 500)
		if err != nil {
			if errors.Is(err, store.ErrChangeCursorExpired) {
				problem := NewError(410, "changes_cursor_expired", "Rescan the vault, then resume from the supplied cursor.")
				problem.Cursor = head
				writeError(w, problem)
			} else if errors.Is(err, store.ErrInvalidChangeCursor) {
				writeError(w, NewError(400, "validation", err.Error()))
			} else {
				writeError(w, NewError(500, "internal", err.Error()))
			}
			return
		}
		w.Header().Set("Content-Type", mailboxNDJSONMediaType)
		w.Header().Set("Cache-Control", "no-store")
		stream := newEventStreamWriter[store.VaultChange](w, cancel)
		// Flush even an empty follow response so the client can start reading.
		if f := responseFlusher(w); f != nil {
			f.Flush()
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			for _, change := range changes {
				stream.send(change)
				if stream.err() != nil {
					return
				}
				after = change.Cursor
			}
			if len(changes) < 500 {
				if !follow {
					return
				}
				if after == "" {
					after = head
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
			changes, head, err = d.Store.VaultChanges(ctx, after, 500)
			if err != nil {
				return
			}
		}
	})
}
