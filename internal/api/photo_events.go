package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

const photoEventsPath = "/api/v1/photos/events"
const maxPhotoEventStreams = 128
const photoEventWriteTimeout = 10 * time.Second

func registerPhotoEvents(shutdown context.Context, mux *http.ServeMux, api huma.API, d Deps) {
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "photoEvents", Method: http.MethodGet, Path: photoEventsPath,
		Summary: "Observe photo changes as server-sent events",
		Responses: map[string]*huma.Response{
			"200": {Description: "An initial refetch followed by visible resource IDs and revisions. Reconnects refetch current state.", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: openAPIStringType}}}},
			"429": {Description: "The daemon's concurrent photo event stream limit is reached.", Content: map[string]*huma.MediaType{new(Error).ContentType("application/json"): {Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[Error](), true, "")}}},
		},
	})
	streams := make(chan struct{}, maxPhotoEventStreams)
	mux.HandleFunc("GET "+photoEventsPath, func(w http.ResponseWriter, r *http.Request) {
		select {
		case streams <- struct{}{}:
			defer func() { <-streams }()
		default:
			writeError(w, NewError(http.StatusTooManyRequests, "photo_event_stream_limit", "Close another photo view and retry."))
			return
		}
		events, unsubscribe := d.Store.SubscribePhotoEvents(r.Context())
		defer unsubscribe()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		controller := http.NewResponseController(w)
		write := func(frame string) error {
			// Writers without deadline support still end with the request context.
			_ = controller.SetWriteDeadline(time.Now().Add(photoEventWriteTimeout))
			if _, err := io.WriteString(w, frame); err != nil {
				return fmt.Errorf("writing photo event: %w", err)
			}
			if err := controller.Flush(); err != nil {
				return fmt.Errorf("flushing photo event: %w", err)
			}
			// An armed deadline would fail a later response on this keep-alive connection.
			_ = controller.SetWriteDeadline(time.Time{})
			return nil
		}
		send := func(event store.PhotoEvent) error {
			if err := r.Context().Err(); err != nil {
				return err
			}
			visible, err := d.Store.VisiblePhotoEvent(r.Context(), event)
			if err != nil {
				if r.Context().Err() == nil {
					d.Logger.Error("checking photo event access", "error", err)
				}
				return err
			}
			if visible == (store.PhotoEvent{}) {
				return nil
			}
			data, err := json.Marshal(visible)
			if err != nil {
				d.Logger.Error("encoding photo event", "error", err)
				return err
			}
			return write(fmt.Sprintf("event: change\ndata: %s\n\n", data))
		}
		if send(store.PhotoEvent{Refetch: true}) != nil {
			return
		}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-shutdown.Done():
				return
			case event, ok := <-events:
				if !ok || send(event) != nil {
					return
				}
			case <-ticker.C:
				if write(": keepalive\n\n") != nil {
					return
				}
			}
		}
	})
}
