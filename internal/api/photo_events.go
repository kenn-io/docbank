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

func registerPhotoEvents(shutdown context.Context, mux *http.ServeMux, api huma.API, d Deps) {
	api.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "photoEvents", Method: http.MethodGet, Path: photoEventsPath,
		Summary: "Observe photo changes as server-sent events",
		Tags:    []string{"Photo events"},
		Responses: map[string]*huma.Response{
			"200": {Description: "SSE change frames contain JSON {} or {\"asset_id\":\"...\"}. Clients must refetch on connection and reconnect to recover missed changes. A frame naming a photo reaches a viewer without a Hidden unlock only for a visible photo; {} frames, including tag changes, reach every viewer.", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: openAPIStringType}}}},
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
		send := func(events ...store.PhotoEvent) error {
			visible, err := d.Store.VisiblePhotoEvents(r.Context(), events)
			if err != nil {
				return err
			}
			for _, event := range visible {
				data, _ := json.Marshal(event) // PhotoEvent contains only strings and integers.
				if err := write(fmt.Sprintf("event: change\ndata: %s\n\n", data)); err != nil {
					return err
				}
			}
			return nil
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
			case batch, ok := <-events:
				if !ok {
					return
				}
				if send(batch...) != nil {
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
