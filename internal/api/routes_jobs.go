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
	type output struct {
		Body JobList
	}
	huma.Register(api, huma.Operation{
		OperationID: "listJobs", Method: http.MethodGet, Path: "/api/v1/jobs",
		Summary: "List daemon background jobs and their current status",
	}, func(ctx context.Context, _ *struct{}) (*output, error) {
		out := &output{Body: JobList{Items: []Job{}}}
		operationNames := make(map[string]struct{})
		if d.Store != nil {
			operations, err := d.Store.StorageOperations(ctx, 1000)
			if err != nil {
				return nil, FromStoreError(err)
			}
			for _, operation := range operations {
				name := "storage:" + operation.ID
				operationNames[name] = struct{}{}
				out.Body.Items = append(out.Body.Items, Job{
					Name: name, Status: string(operation.State),
					StartedAt: operation.CreatedAt.Format(time.RFC3339Nano),
					Error:     operation.Error, OperationID: operation.ID, Kind: operation.Kind,
					CompletedObjects: operation.CompletedObjects,
					TotalObjects:     operation.TotalObjects,
					FinishedAt:       storageOperationAPI(operation).FinishedAt,
				})
			}
		}
		if d.Jobs != nil {
			for _, snapshot := range d.Jobs.Snapshot() {
				if _, durable := operationNames[snapshot.Name]; durable {
					continue
				}
				out.Body.Items = append(out.Body.Items, observableJob(snapshot))
			}
		}
		slices.SortFunc(out.Body.Items, func(a, b Job) int {
			return strings.Compare(a.Name, b.Name)
		})
		return out, nil
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
		return &operationOutput{Body: storageOperationAPI(operation)}, nil
	})
}
