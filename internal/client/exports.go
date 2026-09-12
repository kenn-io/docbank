package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"go.kenn.io/docbank/document/bundle"
)

func (c *Client) CreateExportSource(ctx context.Context, r bundle.SourceRequest) (bundle.Source, error) {
	var out bundle.Source
	err := c.do(ctx, http.MethodPost, "/api/v1/exports/sources", nil, r, &out)
	return out, err
}
func (c *Client) PutExportChunk(ctx context.Context, id string, index int, members []bundle.Member) error {
	return c.do(ctx, http.MethodPut, "/api/v1/exports/sources/"+url.PathEscape(id)+"/chunks/"+strconv.Itoa(index), nil, struct {
		Members []bundle.Member `json:"members"`
	}{members}, nil)
}
func (c *Client) SealExportSource(ctx context.Context, id string) (bundle.Source, error) {
	var out bundle.Source
	err := c.do(ctx, http.MethodPost, "/api/v1/exports/sources/"+url.PathEscape(id)+"/seal", nil, struct{}{}, &out)
	return out, err
}
func (c *Client) CreateExportPlan(ctx context.Context, r bundle.PlanRequest) (bundle.Plan, error) {
	var out bundle.Plan
	err := c.do(ctx, http.MethodPost, "/api/v1/exports/plans", nil, r, &out)
	return out, err
}
func (c *Client) ExportPlan(ctx context.Context, id string) (bundle.Plan, error) {
	var out bundle.Plan
	err := c.do(ctx, http.MethodGet, "/api/v1/exports/plans/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}
func (c *Client) CreateExportJob(ctx context.Context, r bundle.JobRequest) (bundle.Job, error) {
	var out bundle.Job
	err := c.do(ctx, http.MethodPost, "/api/v1/exports/jobs", nil, r, &out)
	return out, err
}

func (c *Client) ExportPlanPreview(ctx context.Context, id string) (bundle.PlanPreview, error) {
	var out bundle.PlanPreview
	err := c.do(ctx, http.MethodGet, "/api/v1/exports/plans/"+url.PathEscape(id)+"/preview", nil, nil, &out)
	return out, err
}
func (c *Client) ExportJob(ctx context.Context, id string) (bundle.Job, error) {
	var out bundle.Job
	err := c.do(ctx, http.MethodGet, "/api/v1/exports/jobs/"+url.PathEscape(id), nil, nil, &out)
	return out, err
}
func (c *Client) CancelExportJob(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/exports/jobs/"+url.PathEscape(id)+"/cancel", nil, struct{}{}, nil)
}

type ExportTicket struct {
	URL     string         `json:"url"`
	Receipt bundle.Receipt `json:"receipt"`
}

func (c *Client) ExportDownloadTicket(ctx context.Context, id string) (ExportTicket, error) {
	return c.ExportDownloadTicketNamed(ctx, id, bundle.DownloadRequest{})
}

func (c *Client) ExportDownloadTicketNamed(ctx context.Context, id string, request bundle.DownloadRequest) (ExportTicket, error) {
	var out ExportTicket
	if _, err := bundle.DownloadBasename(request.Basename); err != nil {
		return out, err
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/exports/jobs/"+url.PathEscape(id)+"/download", nil, request, &out)
	return out, err
}
