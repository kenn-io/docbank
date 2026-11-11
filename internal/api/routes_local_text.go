package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
)

type LocalTextStatus struct {
	Active           bool   `json:"active"`
	Provider         string `json:"provider" enum:",local,ollama,configured"`
	DownloadBytes    int64  `json:"download_bytes"`
	RestartRequired  bool   `json:"restart_required"`
	RestartAvailable bool   `json:"restart_available"`
}

type LocalTextPlanRequest struct {
	Server string `json:"server" enum:"auto,local"`
}

type LocalTextApplyRequest struct {
	Server          string `json:"server" enum:"auto,local"`
	PlanFingerprint string `json:"plan_fingerprint" pattern:"^[0-9a-f]{64}$"`
	Consent         bool   `json:"consent"`
}

type LocalTextPlan struct {
	Fingerprint        string              `json:"fingerprint"`
	VaultUID           string              `json:"vault_uid"`
	Profile            string              `json:"profile"`
	Server             string              `json:"server"`
	ProfileFingerprint string              `json:"profile_fingerprint"`
	Models             []string            `json:"models"`
	VectorEffect       string              `json:"vector_effect"`
	Managed            bool                `json:"managed"`
	Flow               []ProcessingFlowHop `json:"flow"`
	RetainedClasses    []string            `json:"retained_classes"`
	Scope              string              `json:"scope"`
}

func fromLocalTextPlan(p processing.LocalTextPlan) LocalTextPlan {
	return LocalTextPlan{Fingerprint: p.Fingerprint, VaultUID: p.VaultUID, Profile: p.Profile, Server: p.Server,
		ProfileFingerprint: p.ProfileFingerprint, Models: p.Models, VectorEffect: p.VectorEffect,
		RetainedClasses: p.RetainedClasses, Scope: p.Scope, Managed: p.Managed, Flow: fromProcessingPlan(processing.Plan{Flow: p.Flow}).Flow}
}

func registerLocalTextRoutes(api huma.API, d Deps) {
	type statusOutput struct{ Body LocalTextStatus }
	huma.Register(api, huma.Operation{OperationID: "getLocalTextStatus", Method: http.MethodGet,
		Path: "/api/v1/processing/local/status", Summary: "Read search by meaning setup status"},
		func(ctx context.Context, _ *struct{}) (*statusOutput, error) {
			if d.LocalTextStatus == nil {
				return nil, processingUnavailable()
			}
			status, err := d.LocalTextStatus(ctx)
			if err != nil {
				return nil, fromProcessingError(err)
			}
			status.RestartAvailable = d.RestartDaemon != nil
			return &statusOutput{Body: status}, nil
		})
	type restartInput struct {
		Origin string `header:"Origin"`
	}
	type restartOutput struct {
		Body struct {
			NewTab bool `json:"new_tab"`
		}
	}
	huma.Register(api, huma.Operation{OperationID: "restartDaemon", Method: http.MethodPost,
		Path: "/api/v1/processing/local/restart", Summary: "Restart Docbank", DefaultStatus: http.StatusAccepted},
		func(ctx context.Context, input *restartInput) (*restartOutput, error) {
			if !browserSessionRequest(ctx) {
				return nil, NewError(http.StatusForbidden, "forbidden", "A browser session is required")
			}
			if d.RestartDaemon == nil {
				return nil, NewError(http.StatusConflict, "restart_unavailable", "Restart Docbank where it runs")
			}
			openTab := input.Origin == strings.TrimSuffix(d.WebURL, "/")
			if err := d.RestartDaemon(openTab); err != nil {
				return nil, NewError(http.StatusInternalServerError, "restart_failed", err.Error())
			}
			out := &restartOutput{}
			out.Body.NewTab = openTab
			return out, nil
		})
	type planInput struct{ Body LocalTextPlanRequest }
	type output struct{ Body LocalTextPlan }
	huma.Register(api, huma.Operation{OperationID: "planLocalText", Method: http.MethodPost,
		Path: "/api/v1/processing/local/plans", Summary: "Preview vault-wide text indexing"},
		func(ctx context.Context, input *planInput) (*output, error) {
			if d.LocalText == nil {
				return nil, processingUnavailable()
			}
			plan, err := d.LocalText.Plan(ctx, input.Body.Server)
			if err != nil {
				return nil, fromProcessingError(err)
			}
			return &output{Body: fromLocalTextPlan(plan)}, nil
		})
	type applyInput struct{ Body LocalTextApplyRequest }
	huma.Register(api, huma.Operation{OperationID: "applyLocalText", Method: http.MethodPost,
		Path: "/api/v1/processing/local/setup", Summary: "Approve vault-wide and ongoing text indexing"},
		func(ctx context.Context, input *applyInput) (*output, error) {
			if d.LocalText == nil {
				return nil, processingUnavailable()
			}
			if !input.Body.Consent {
				return nil, fromProcessingError(processing.ErrConsentRequired)
			}
			plan, err := d.LocalText.Apply(ctx, input.Body.Server, input.Body.PlanFingerprint)
			if err != nil {
				return nil, fromProcessingError(err)
			}
			return &output{Body: fromLocalTextPlan(plan)}, nil
		})
}
