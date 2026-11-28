package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
)

type LocalTextStatus struct {
	Active          bool   `json:"active"`
	Provider        string `json:"provider" enum:",local,ollama,configured"`
	DownloadBytes   int64  `json:"download_bytes"`
	RestartRequired bool   `json:"restart_required"`
}

type LocalTextPlanRequest struct {
	Server string                      `json:"server" enum:"auto,local"`
	Target *processing.LocalTextTarget `json:"target,omitempty"`
}

type LocalTextApplyRequest struct {
	Server          string                      `json:"server" enum:"auto,local"`
	Target          *processing.LocalTextTarget `json:"target,omitempty"`
	PlanFingerprint string                      `json:"plan_fingerprint" pattern:"^[0-9a-f]{64}$"`
	Consent         bool                        `json:"consent"`
}

func localTextTarget(server string, target *processing.LocalTextTarget) processing.LocalTextTarget {
	if target == nil {
		return processing.LocalTextTarget{Server: server}
	}
	choice := *target
	choice.Server = server
	return choice
}

type LocalTextPlan struct {
	Fingerprint        string                      `json:"fingerprint"`
	VaultUID           string                      `json:"vault_uid"`
	Profile            string                      `json:"profile"`
	Server             string                      `json:"server"`
	ProfileFingerprint string                      `json:"profile_fingerprint"`
	Models             []string                    `json:"models"`
	VectorEffect       string                      `json:"vector_effect"`
	Managed            bool                        `json:"managed"`
	Flow               []ProcessingFlowHop         `json:"flow"`
	RetainedClasses    []string                    `json:"retained_classes"`
	Scope              string                      `json:"scope"`
	Retained           []string                    `json:"retained,omitempty"`
	Target             *processing.LocalTextTarget `json:"target,omitempty"`
}

func fromLocalTextPlan(p processing.LocalTextPlan) LocalTextPlan {
	return LocalTextPlan{Fingerprint: p.Fingerprint, VaultUID: p.VaultUID, Profile: p.Profile, Server: p.Server,
		ProfileFingerprint: p.ProfileFingerprint, Models: p.Models, VectorEffect: p.VectorEffect,
		Retained: p.Retained, Target: p.Target, RetainedClasses: p.RetainedClasses, Scope: p.Scope, Managed: p.Managed, Flow: fromProcessingPlan(processing.Plan{Flow: p.Flow}).Flow}
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
			return &statusOutput{Body: status}, nil
		})
	type planInput struct{ Body LocalTextPlanRequest }
	type output struct{ Body LocalTextPlan }
	huma.Register(api, huma.Operation{OperationID: "planLocalText", Method: http.MethodPost,
		Path: "/api/v1/processing/local/plans", Summary: "Preview vault-wide text indexing"},
		func(ctx context.Context, input *planInput) (*output, error) {
			if d.LocalText == nil {
				return nil, processingUnavailable()
			}
			if input.Body.Target != nil {
				if err := input.Body.Target.ValidateEmbeddingServer(); err != nil {
					return nil, NewError(http.StatusUnprocessableEntity, "processing_profile_unavailable", err.Error())
				}
			}
			plan, err := d.LocalText.Plan(ctx, localTextTarget(input.Body.Server, input.Body.Target))
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
			if input.Body.Target != nil {
				if err := input.Body.Target.ValidateEmbeddingServer(); err != nil {
					return nil, NewError(http.StatusUnprocessableEntity, "processing_profile_unavailable", err.Error())
				}
			}
			plan, err := d.LocalText.Apply(ctx, localTextTarget(input.Body.Server, input.Body.Target), input.Body.PlanFingerprint)
			if err != nil {
				return nil, fromProcessingError(err)
			}
			return &output{Body: fromLocalTextPlan(plan)}, nil
		})
}
