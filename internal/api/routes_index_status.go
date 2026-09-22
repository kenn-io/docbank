package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/processing"
)

// RegisterIndexRoutes adds the index operations to an existing authenticated
// API. The caller owns the coordinator and its projection adapters.
func RegisterIndexRoutes(api huma.API, coordinator *processing.IndexCoordinator) {
	registerIndexStatusRoute(api, coordinator)
	registerIndexRepairRoutes(api, coordinator)
}

func registerIndexStatusRoute(api huma.API, coordinator *processing.IndexCoordinator) {
	type statusOutput struct{ Body processing.IndexStatusReport }
	huma.Register(api, huma.Operation{
		OperationID: "getIndexStatus", Method: http.MethodGet,
		Path: "/api/v1/index/status", Summary: "Read projection generations, coverage and freshness",
	}, func(ctx context.Context, in *struct {
		RequireFresh     bool `query:"require_fresh"`
		WaitMilliseconds int  `query:"wait_ms" minimum:"0" maximum:"10000"`
	}) (*statusOutput, error) {
		if coordinator == nil {
			return nil, NewError(http.StatusServiceUnavailable, "index_unavailable", "index operations are unavailable")
		}
		report, err := coordinator.Status(ctx, processing.IndexStatusRequest{
			RequireFresh: in.RequireFresh, Wait: time.Duration(in.WaitMilliseconds) * time.Millisecond,
		})
		if err != nil {
			return nil, fromIndexError(err)
		}
		return &statusOutput{Body: report}, nil
	})
}

func fromIndexError(err error) error {
	switch {
	case errors.Is(err, processing.ErrIndexFreshnessTimeout):
		return NewError(http.StatusGatewayTimeout, "freshness_timeout", "the requested index watermark was not reached before the bounded wait expired")
	case errors.Is(err, processing.ErrInvalidIndexRequest):
		return NewError(http.StatusUnprocessableEntity, "invalid_index_request", "the index request is invalid")
	case errors.Is(err, processing.ErrIndexProviderConsentRequired):
		return NewError(http.StatusPreconditionRequired, "provider_consent_required", "review and allow the disclosed provider work before repair")
	case errors.Is(err, processing.ErrIndexPlanChanged):
		return NewError(http.StatusPreconditionFailed, "index_plan_changed", "the index repair plan changed; preview it again")
	case errors.Is(err, processing.ErrIndexRepairInProgress):
		return NewError(http.StatusConflict, "index_repair_in_progress", "the requested index repair is already running")
	case errors.Is(err, processing.ErrIndexDisabled):
		return NewError(http.StatusConflict, "index_disabled", "the requested index projection is disabled")
	case errors.Is(err, processing.ErrIndexNotAuthorized), errors.Is(err, processing.ErrIndexPermissionChanged):
		return NewError(http.StatusPreconditionFailed, "index_permission_changed", "index authority changed; preview the repair again")
	case errors.Is(err, processing.ErrIndexSourceChanged):
		return NewError(http.StatusPreconditionFailed, "index_source_changed", "index source authority changed; preview the repair again")
	case errors.Is(err, processing.ErrIndexStorageFull):
		return NewError(http.StatusInsufficientStorage, "index_storage_full", "the index candidate could not be stored")
	case errors.Is(err, processing.ErrIndexManifestCorrupt):
		return NewError(http.StatusInternalServerError, "index_manifest_corrupt", "an index manifest failed validation")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return NewError(http.StatusRequestTimeout, "index_request_canceled", "the index operation was canceled")
	default:
		return NewError(http.StatusInternalServerError, "index_repair_failed", "the index operation failed before cutover")
	}
}
