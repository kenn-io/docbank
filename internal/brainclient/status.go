package brainclient

import (
	"context"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
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

// Status does not start a local daemon. Publication readiness comes from the
// document page snapshot; consent is a separate live observation.
func (c *Client) Status(ctx context.Context, name string) (Status, error) {
	cfg, err := c.registry.Get(ctx, name)
	if err != nil {
		return Status{}, err
	}
	status := Status{Name: name, ObservedAt: c.deps.Now().UTC(), ConnectionState: "unknown", BodyIndexingState: "unknown"}
	conn, running, err := c.connection(ctx, cfg, false)
	if err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		status.ProblemCode = "daemon_unavailable"
		return status, nil
	}
	if !running {
		status.ConnectionState = "stopped"
		return status, nil
	}
	defer func() { _ = conn.Close() }()
	page, err := conn.ListDocuments(ctx, api.DocumentQuery{PublishedNotes: &api.PublishedNoteSelector{Publication: name}, PageSize: 1})
	if err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		status.ProblemCode = "publication_unavailable"
		if daemonconn.IsTransportError(err) {
			status.ProblemCode = "daemon_unavailable"
		}
		return status, nil
	}
	status.ConnectionState = "connected"
	status.Readiness = page.NoteReadiness
	plan, err := conn.NoteLocalTextPlan(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		status.ProblemCode = "indexing_status_unavailable"
		if daemonconn.IsTransportError(err) {
			status.ConnectionState = "unknown"
			status.ProblemCode = "daemon_unavailable"
		}
		return status, nil
	}
	if err := api.ValidateNoteLocalTextPlan(plan); err != nil {
		status.ProblemCode = "indexing_plan_unrecognized"
		return status, nil
	}
	if plan.Plan.ConsentState == "active" {
		status.BodyIndexingState = "authorized"
	} else {
		status.BodyIndexingState = "consent_" + plan.Plan.ConsentState
	}
	return status, nil
}
