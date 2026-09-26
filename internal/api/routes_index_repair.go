package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/processing"
)

func registerIndexRepairRoutes(api huma.API, coordinator *processing.IndexCoordinator) {
	type planOutput struct{ Body processing.IndexRepairPlan }
	huma.Register(api, huma.Operation{
		OperationID: "planIndexRepair", Method: http.MethodPost,
		Path: "/api/v1/index/repair-plans", Summary: "Preview bounded work, providers and cost for targeted index repair",
		MaxBodyBytes: 32 << 10,
	}, func(ctx context.Context, in *struct {
		Body processing.IndexRepairPlanRequest
	}) (*planOutput, error) {
		if coordinator == nil {
			return nil, NewError(http.StatusServiceUnavailable, "index_unavailable", "index operations are unavailable")
		}
		plan, err := coordinator.PlanRepair(ctx, in.Body)
		if err != nil {
			return nil, fromIndexError(err)
		}
		return &planOutput{Body: plan}, nil
	})

	type repairOutput struct{ Body processing.IndexRepairReport }
	huma.Register(api, huma.Operation{
		OperationID: "repairIndexes", Method: http.MethodPost,
		Path: "/api/v1/index/repairs", Summary: "Build, validate and atomically publish targeted index generations",
		MaxBodyBytes: 32 << 10,
	}, func(ctx context.Context, in *struct{ Body processing.IndexRepairRequest }) (*repairOutput, error) {
		if coordinator == nil {
			return nil, NewError(http.StatusServiceUnavailable, "index_unavailable", "index operations are unavailable")
		}
		report, err := coordinator.Repair(ctx, in.Body)
		if err != nil {
			return nil, fromIndexError(err)
		}
		return &repairOutput{Body: report}, nil
	})
}
