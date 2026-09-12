package api

import (
	"context"
	"go.kenn.io/docbank/internal/store"
	"net/http"
	"time"
)

type MailboxEvent struct {
	Type  string            `json:"type"`
	Job   *store.MailboxJob `json:"job,omitempty"`
	Error string            `json:"error,omitempty"`
}

const mailboxProgressEvent = "progress"

func registerMailboxEvents(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("GET /api/v1/mailbox/jobs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		job, err := d.Store.MailboxJob(ctx, mailboxOwner(d), r.PathValue("id"))
		if err != nil {
			writeMailboxError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		stream := newEventStreamWriter[MailboxEvent](w, cancel)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			terminal := job.State != "queued" && job.State != "running"
			kind := mailboxProgressEvent
			if terminal {
				kind = "result"
			}
			stream.send(MailboxEvent{Type: kind, Job: &job})
			if terminal || stream.err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			job, err = d.Store.MailboxJob(ctx, mailboxOwner(d), job.ID)
			if err != nil {
				stream.send(MailboxEvent{Type: streamErrorEvent, Error: err.Error()})
				return
			}
		}
	})
}
