package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/processing"
)

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
