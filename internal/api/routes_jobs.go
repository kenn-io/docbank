package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func observableJob(snapshot jobs.Snapshot) Job {
	job := Job{
		Name: snapshot.Name, Status: string(snapshot.Status),
		StartedAt: snapshot.StartedAt.Format(time.RFC3339Nano), Error: snapshot.Error,
	}
	if snapshot.FinishedAt != nil {
		job.FinishedAt = snapshot.FinishedAt.Format(time.RFC3339Nano)
	}
	return job
}

func registerJobRoutes(api huma.API, d Deps) {
	applyControl := func(
		job *Job, controls map[string]store.LaneControl, lane string, active bool,
	) {
		if !active {
			return
		}
		control, found := controls[lane]
		if !found {
			return
		}
		job.Controllable = true
		job.CanSetConcurrency = store.LaneConcurrencyAdjustable(lane)
		job.Paused, job.Concurrency = control.Paused, control.Concurrency
		job.ControlRevision = control.Revision
	}
	type output struct {
		Body JobList
	}
	huma.Register(api, huma.Operation{
		OperationID: "listJobs", Method: http.MethodGet, Path: "/api/v1/jobs",
		Summary: "List daemon background jobs and their current status",
	}, func(ctx context.Context, _ *struct{}) (*output, error) {
		out := &output{Body: JobList{Items: []Job{}}}
		redactErrors := browserSessionRequest(ctx)
		operationNames := make(map[string]struct{})
		var controls map[string]store.LaneControl
		if d.Store != nil {
			var err error
			controls, err = d.Store.LaneControls(ctx)
			if err != nil {
				out.Body.LaneControlsError = err.Error()
				if redactErrors {
					out.Body.LaneControlsError = "lane controls are unavailable; inspect with the Docbank CLI for details"
				}
			}
			operations, err := d.Store.StorageOperations(ctx, 1000)
			if err != nil {
				return nil, FromStoreError(err)
			}
			for _, operation := range operations {
				name := "storage:" + operation.ID
				operationNames[name] = struct{}{}
				errorDetail := operation.Error
				if redactErrors && errorDetail != "" {
					errorDetail = "storage operation failed; inspect with the Docbank CLI for details"
				}
				job := Job{
					Name: name, Status: string(operation.State),
					StartedAt: operation.CreatedAt.Format(time.RFC3339Nano),
					Error:     errorDetail, OperationID: operation.ID, Kind: operation.Kind,
					CompletedObjects: operation.CompletedObjects,
					TotalObjects:     operation.TotalObjects,
					FinishedAt:       storageOperationAPI(operation).FinishedAt,
					CancelRequested:  operation.CancelRequested,
				}
				job.CanCancel = operation.State == store.StorageOperationQueued ||
					operation.State == store.StorageOperationRunning
				applyControl(&job, controls, operation.Kind, job.CanCancel)
				out.Body.Items = append(out.Body.Items, job)
			}
		}
		if d.Jobs != nil {
			for _, snapshot := range d.Jobs.Snapshot() {
				if _, durable := operationNames[snapshot.Name]; durable {
					continue
				}
				job := observableJob(snapshot)
				applyControl(&job, controls, snapshot.Name, snapshot.Status == jobs.StatusRunning)
				if redactErrors && job.Error != "" {
					job.Error = "background job failed; inspect with the Docbank CLI for details"
				}
				out.Body.Items = append(out.Body.Items, job)
			}
		}
		slices.SortFunc(out.Body.Items, func(a, b Job) int {
			return strings.Compare(a.Name, b.Name)
		})
		return out, nil
	})

	type controlOutput struct {
		ETag string `header:"ETag"`
		Body LaneControl
	}
	controlResult := func(control store.LaneControl) *controlOutput {
		return &controlOutput{ETag: revisionETag(control.Revision), Body: LaneControl{
			Lane: control.Lane, Paused: control.Paused, Concurrency: control.Concurrency,
			Revision:          control.Revision,
			CanSetConcurrency: store.LaneConcurrencyAdjustable(control.Lane),
		}}
	}
	controlError := func(err error) error {
		if errors.Is(err, store.ErrLaneControlsFile) {
			// Lane routes are daemon-only, so the file path is safe to show.
			return NewError(http.StatusInternalServerError, "lane_controls_unreadable",
				err.Error()+"; repair or remove lane-controls.json")
		}
		if errors.Is(err, store.ErrLaneControl) {
			return NewError(http.StatusBadRequest, "validation",
				"lane is read-only or concurrency is unsupported")
		}
		return FromStoreError(err)
	}
	huma.Register(api, huma.Operation{
		OperationID: "getLaneControl", Method: http.MethodGet,
		Path: "/api/v1/jobs/lanes/{lane}", Summary: "Read durable lane controls",
	}, func(ctx context.Context, in *struct {
		Lane string `path:"lane"`
	}) (*controlOutput, error) {
		control, err := d.Store.LaneControl(ctx, in.Lane)
		if err != nil {
			return nil, controlError(err)
		}
		return controlResult(control), nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "setLaneControl", Method: http.MethodPut,
		Path:    "/api/v1/jobs/lanes/{lane}",
		Summary: "Pause, resume, or set photo preview concurrency",
	}, func(ctx context.Context, in *struct {
		Lane    string `path:"lane"`
		IfMatch string `header:"If-Match"`
		Body    SetLaneControlRequest
	}) (*controlOutput, error) {
		revision, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		control, err := d.Store.SetLaneControl(ctx, store.LaneControl{
			Lane: in.Lane, Paused: in.Body.Paused, Concurrency: in.Body.Concurrency,
		}, revision)
		if err != nil {
			return nil, controlError(err)
		}
		return controlResult(control), nil
	})

	type operationOutput struct{ Body StorageOperation }
	huma.Register(api, huma.Operation{
		OperationID: "getStorageOperation", Method: http.MethodGet,
		Path:    "/api/v1/jobs/{operation_id}",
		Summary: "Inspect one durable storage operation and its latest receipt",
	}, func(ctx context.Context, in *struct {
		OperationID string `path:"operation_id"`
	}) (*operationOutput, error) {
		operation, err := d.Store.StorageOperation(ctx, in.OperationID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &operationOutput{Body: storageOperationAPI(operation)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "cancelStorageOperation", Method: http.MethodPost,
		Path:    "/api/v1/jobs/{operation_id}/cancel",
		Summary: "Request cancellation at the next durable object boundary",
	}, func(ctx context.Context, in *struct {
		OperationID string `path:"operation_id"`
	}) (*operationOutput, error) {
		if err := d.Store.RequestStorageOperationCancel(ctx, in.OperationID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, FromStoreError(err)
			}
			if errors.Is(err, store.ErrStorageOperationTerminal) {
				return nil, NewError(
					http.StatusConflict, "storage_operation_terminal", err.Error(),
				)
			}
			return nil, FromStoreError(err)
		}
		operation, err := d.Store.StorageOperation(ctx, in.OperationID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		result := storageOperationAPI(operation)
		if browserSessionRequest(ctx) {
			// Receipts and raw errors can name daemon-host paths.
			result.Receipt = nil
			if result.Error != "" {
				result.Error = "storage operation failed; inspect with the Docbank CLI for details"
			}
		}
		return &operationOutput{Body: result}, nil
	})
}
