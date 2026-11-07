package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type FaceSetupApplyRequest struct {
	PlanFingerprint string `json:"plan_fingerprint" pattern:"^[0-9a-f]{64}$"`
	Consent         bool   `json:"consent"`
}

func registerFaceSetupRoutes(api huma.API, d Deps) {
	type output struct{ Body LocalTextPlan }
	huma.Register(api, huma.Operation{OperationID: "planFaces", Method: http.MethodPost, Path: "/api/v1/processing/faces/plans", Summary: "Preview local face suggestions"}, func(ctx context.Context, _ *struct{}) (*output, error) {
		if d.Faces == nil {
			return nil, processingUnavailable()
		}
		plan, err := d.Faces.Plan(ctx)
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &output{Body: fromLocalTextPlan(plan)}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "applyFaces", Method: http.MethodPost, Path: "/api/v1/processing/faces/setup", Summary: "Enable local face suggestions"}, func(ctx context.Context, in *struct{ Body FaceSetupApplyRequest }) (*output, error) {
		if d.Faces == nil {
			return nil, processingUnavailable()
		}
		if !in.Body.Consent {
			return nil, NewError(422, "consent_required", "consent is required")
		}
		plan, err := d.Faces.Apply(ctx, in.Body.PlanFingerprint)
		if err != nil {
			return nil, fromProcessingError(err)
		}
		return &output{Body: fromLocalTextPlan(plan)}, nil
	})
}
