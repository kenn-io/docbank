package brainclient

import (
	"context"
	"time"

	"go.kenn.io/docbank/internal/api"
)

// Status reports an observation, never a saved intent as evidence of indexing.
type Status struct {
	Name              string             `json:"name"`
	ObservedAt        time.Time          `json:"observed_at"`
	ConnectionState   string             `json:"connection_state"`
	BodyIndexingState string             `json:"body_indexing_state"`
	Readiness         *api.NoteReadiness `json:"readiness,omitzero"`
	ProblemCode       string             `json:"problem_code,omitzero"`
}

// Status does not start a local daemon. Until the atomic publication readiness
// snapshot is available, indexing stays unknown rather than inferred.
func (c *Client) Status(ctx context.Context, name string) (Status, error) {
	cfg, err := c.registry.Get(ctx, name)
	if err != nil {
		return Status{}, err
	}
	status := Status{Name: name, ObservedAt: c.deps.Now().UTC(), ConnectionState: "unknown", BodyIndexingState: "unknown"}
	conn, running, err := c.connection(ctx, cfg, false)
	if err != nil {
		status.ProblemCode = "daemon_unavailable"
		return status, nil
	}
	if !running {
		status.ConnectionState = "stopped"
		return status, nil
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ListDocuments(ctx, api.DocumentQuery{PublishedNotes: &api.PublishedNoteSelector{Publication: name}, PageSize: 1}); err != nil {
		status.ProblemCode = "publication_unavailable"
		return status, nil
	}
	status.ConnectionState = "connected"
	return status, nil
}
