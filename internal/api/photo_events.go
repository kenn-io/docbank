package api

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

const photoEventsPath = "/api/v1/photos/events"
const maxPhotoEventStreams = 128

func registerPhotoEvents(shutdown context.Context, mux *http.ServeMux, api huma.API, d Deps) {
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "photoEvents", Method: http.MethodGet, Path: photoEventsPath,
		Summary: "Observe photo changes as server-sent events",
		Tags:    []string{"Photo events"},
		Responses: map[string]*huma.Response{
			"200": {Description: "SSE change frames contain the JSON {} and name no photo. Every frame reaches every stream, locked or not; each client refetches through the read path that hides photos from locked viewers. Clients must refetch on connection and reconnect to recover missed changes.", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: openAPIStringType}}}},
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
		events, unsubscribe := d.Store.SubscribePhotoEvents()
		defer unsubscribe()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		controller := http.NewResponseController(w)
		write := func(frame string) error {
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := io.WriteString(w, frame)
			if err == nil {
				err = controller.Flush()
			}
			// An armed deadline would fail a later response on this keep-alive connection.
			_ = controller.SetWriteDeadline(time.Time{})
			return err
		}
		// Flush headers so clients can start reads before the first change.
		if write("") != nil {
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
			case _, ok := <-events:
				if !ok {
					return
				}
				if write("event: change\ndata: {}\n\n") != nil {
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
