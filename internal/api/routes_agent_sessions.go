package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

type agentSessionRequest struct {
	Operations []Operation `json:"operations"`
	SourceIDs  []string    `json:"source_ids" minItems:"1" maxItems:"4096"`
	TTLSeconds int64       `json:"ttl_seconds"`
}

type agentSessionResponse struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func registerAgentSessionRoutes(api huma.API, registry *agentSessionRegistry) {
	huma.Register(api, huma.Operation{
		OperationID: "issueAgentSession", Method: http.MethodPost,
		Path: "/api/v1/agent-sessions", Summary: "Issue a bounded read-only agent session",
		Description:   "Only the read operation is currently accepted. Scope to exact source IDs and a lifetime of at most one hour.",
		DefaultStatus: http.StatusCreated, MaxBodyBytes: 16 << 10,
	}, func(ctx context.Context, in *struct{ Body agentSessionRequest }) (*struct{ Body agentSessionResponse }, error) {
		if !masterAgentSessionRequest(ctx) || registry == nil {
			return nil, NewError(http.StatusForbidden, "operation_denied", "master credential required")
		}
		if in.Body.TTLSeconds <= 0 || in.Body.TTLSeconds > int64(maxAgentSessionTTL/time.Second) {
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_agent_session", "session duration is outside the supported range")
		}
		issued, err := registry.issue(agentSessionGrant{
			Operations: in.Body.Operations, SourceIDs: in.Body.SourceIDs,
			TTL: time.Duration(in.Body.TTLSeconds) * time.Second,
		})
		if err != nil {
			if errors.Is(err, ErrAgentSessionLimit) {
				return nil, NewError(http.StatusTooManyRequests, "agent_session_limit", "agent session limit reached")
			}
			return nil, NewError(http.StatusUnprocessableEntity, "invalid_agent_session", "invalid agent session grant")
		}
		return &struct{ Body agentSessionResponse }{Body: agentSessionResponse(issued)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "revokeAgentSession", Method: http.MethodDelete,
		Path: "/api/v1/agent-sessions/{id}", Summary: "Revoke one agent session",
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		if !masterAgentSessionRequest(ctx) || registry == nil {
			return nil, NewError(http.StatusForbidden, "operation_denied", "master credential required")
		}
		if !registry.revoke(in.ID) {
			return nil, NewError(http.StatusNotFound, "not_found", "agent session not found")
		}
		return &struct{}{}, nil
	})
}

func masterAgentSessionRequest(ctx context.Context) bool {
	kind, _ := ctx.Value(authenticationContextKey{}).(string)
	return kind == "master"
}
