package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/jobs"
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
	}, func(_ context.Context, _ *struct{}) (*output, error) {
		out := &output{Body: JobList{Items: []Job{}}}
		if d.Jobs == nil {
			return out, nil
		}
		for _, snapshot := range d.Jobs.Snapshot() {
			out.Body.Items = append(out.Body.Items, observableJob(snapshot))
		}
		return out, nil
	})
}
