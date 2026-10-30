package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// ChangeEvent uses the same change/result/error framing as mailbox streams.
type ChangeEvent struct {
	Type   string             `json:"type"`
	Change *store.VaultChange `json:"change,omitempty"`
	Error  *Error             `json:"error,omitempty"`
	Cursor string             `json:"cursor,omitempty"`
}

func changeProblem(err error, head string) *Error {
	if errors.Is(err, store.ErrChangeCursorExpired) {
		problem := NewError(410, "changes_cursor_expired", "Rescan the vault, then resume from the supplied cursor.")
		problem.Cursor = head
		return problem
	}
	if errors.Is(err, store.ErrInvalidChangeCursor) {
		return NewError(400, "validation", err.Error())
	}
	return NewError(500, "internal", err.Error())
}

const changesPath = "/api/v1/changes"

func registerChanges(shutdown context.Context, mux *http.ServeMux, api huma.API, d Deps) {
	problem := &huma.Response{Description: "Invalid or expired cursor", Content: map[string]*huma.MediaType{new(Error).ContentType("application/json"): {Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[Error](), true, "")}}}
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "vaultChanges", Method: http.MethodGet, Path: changesPath, Summary: "Read resumable vault changes", Tags: []string{"Changes"},
		Parameters: []*huma.Param{{Name: "after", In: "query", Schema: &huma.Schema{Type: openAPIStringType}}, {Name: "follow", In: "query", Schema: &huma.Schema{Type: "boolean"}}},
		Responses: map[string]*huma.Response{
			"200": {Description: "NDJSON change records followed by a terminal result or error", Content: map[string]*huma.MediaType{mailboxNDJSONMediaType: {Schema: &huma.Schema{Type: openAPIStringType}}}},
			"400": problem, "410": problem,
		},
	})
	mux.HandleFunc("GET "+changesPath, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		stop := context.AfterFunc(shutdown, cancel)
		defer stop()
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, NewError(400, "validation", "invalid change query: "+err.Error()))
			return
		}
		after := query.Get("after")
		follow := false
		if value := query.Get("follow"); value != "" {
			var err error
			follow, err = strconv.ParseBool(value)
			if err != nil {
				writeError(w, NewError(400, "validation", "follow must be a boolean"))
				return
			}
		}
		changes, head, err := d.Store.VaultChanges(ctx, after, 500)
		if err != nil {
			writeError(w, changeProblem(err, head))
			return
		}
		w.Header().Set("Content-Type", mailboxNDJSONMediaType)
		w.Header().Set("Cache-Control", "no-store")
		controller := http.NewResponseController(w)
		interrupted := make(chan struct{})
		stopWrite := context.AfterFunc(ctx, func() {
			_ = controller.SetWriteDeadline(time.Now())
			close(interrupted)
		})
		defer func() {
			if !stopWrite() {
				<-interrupted
			}
			_ = controller.SetWriteDeadline(time.Time{})
		}()
		stream := newEventStreamWriter[ChangeEvent](w, cancel)
		// Flush even an empty follow response so the client can start reading.
		if err := controller.Flush(); err != nil {
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			for _, change := range changes {
				stream.send(ChangeEvent{Type: "change", Change: &change})
				if stream.err() != nil {
					return
				}
				after = change.Cursor
			}
			if len(changes) < 500 {
				if !follow {
					stream.send(ChangeEvent{Type: "result", Cursor: head})
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
				stream.send(ChangeEvent{Type: "error", Error: changeProblem(err, head)})
				return
			}
		}
	})
}
