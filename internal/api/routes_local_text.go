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
	Server              string `json:"server" enum:"auto,local"`
	EmbeddingServer     string `json:"embedding_server,omitempty"`
	EmbeddingModel      string `json:"embedding_model,omitempty"`
	EmbeddingDimensions int    `json:"embedding_dimensions,omitempty"`
}

type LocalTextApplyRequest struct {
	Server              string `json:"server" enum:"auto,local"`
	EmbeddingServer     string `json:"embedding_server,omitempty"`
	EmbeddingModel      string `json:"embedding_model,omitempty"`
	EmbeddingDimensions int    `json:"embedding_dimensions,omitempty"`
	PlanFingerprint     string `json:"plan_fingerprint" pattern:"^[0-9a-f]{64}$"`
	Consent             bool   `json:"consent"`
}

type LocalTextPlan struct {
	Fingerprint        string                            `json:"fingerprint"`
	VaultUID           string                            `json:"vault_uid"`
	Profile            string                            `json:"profile"`
	Server             string                            `json:"server"`
	ProfileFingerprint string                            `json:"profile_fingerprint"`
	Models             []string                          `json:"models"`
	VectorEffect       string                            `json:"vector_effect"`
	Managed            bool                              `json:"managed"`
	Flow               []ProcessingFlowHop               `json:"flow"`
	RetainedClasses    []string                          `json:"retained_classes"`
	Scope              string                            `json:"scope"`
	Retained           []string                          `json:"retained,omitempty"`
	Profiles           []processing.LocalTextProfilePlan `json:"profiles,omitempty"`
	Target             *processing.LocalTextTarget       `json:"target,omitempty"`
}

func fromLocalTextPlan(p processing.LocalTextPlan) LocalTextPlan {
	return LocalTextPlan{Fingerprint: p.Fingerprint, VaultUID: p.VaultUID, Profile: p.Profile, Server: p.Server,
		ProfileFingerprint: p.ProfileFingerprint, Models: p.Models, VectorEffect: p.VectorEffect,
		Retained: p.Retained, Target: p.Target, Profiles: p.Profiles, RetainedClasses: p.RetainedClasses, Scope: p.Scope, Managed: p.Managed, Flow: fromProcessingPlan(processing.Plan{Flow: p.Flow}).Flow}
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
			var plan processing.LocalTextPlan
			var err error
			if input.Body.EmbeddingServer == "" && input.Body.EmbeddingModel == "" && input.Body.EmbeddingDimensions == 0 {
				plan, err = d.LocalText.Plan(ctx, input.Body.Server)
			} else {
				plan, err = d.LocalText.PlanTarget(ctx, processing.LocalTextTarget{Server: input.Body.Server, EmbeddingServer: input.Body.EmbeddingServer, EmbeddingModel: input.Body.EmbeddingModel, EmbeddingDimensions: input.Body.EmbeddingDimensions})
			}
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
			var plan processing.LocalTextPlan
			var err error
			if input.Body.EmbeddingServer == "" && input.Body.EmbeddingModel == "" && input.Body.EmbeddingDimensions == 0 {
				plan, err = d.LocalText.Apply(ctx, input.Body.Server, input.Body.PlanFingerprint)
			} else {
				plan, err = d.LocalText.ApplyTarget(ctx, processing.LocalTextTarget{Server: input.Body.Server, EmbeddingServer: input.Body.EmbeddingServer, EmbeddingModel: input.Body.EmbeddingModel, EmbeddingDimensions: input.Body.EmbeddingDimensions}, input.Body.PlanFingerprint)
			}
			if err != nil {
				return nil, fromProcessingError(err)
			}
			return &output{Body: fromLocalTextPlan(plan)}, nil
		})
}
