// Package client hand-rolls calls against the server's routes.
package client

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"

	"routes/client/generated"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.MarshalWrite(&body, in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, &body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.UnmarshalRead(resp.Body, out)
}

func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) Ping(ctx context.Context) error {
	return c.GetJSON(ctx, "/api/v1/ping", nil) // want "own Huma route /api/v1/ping"
}

func (c *Client) Account(ctx context.Context, id string) error {
	return c.GetJSON(ctx, "/api/v1/accounts/"+id, nil) // want "own Huma route /api/v1/accounts/\{param\}"
}

func (c *Client) Review(ctx context.Context, id int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/review", id), nil, nil) // want "own Huma route /api/v1/jobs/\{param\}/review"
}

func Jobs(ctx context.Context, base string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/jobs?limit=%d", base, 10), nil) // want "own Huma route /api/v1/jobs"
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func Queue(base string) (*http.Response, error) {
	return http.Get(base + "/api/v1/queue") // want "own Huma route /api/v1/queue"
}

func Grouped(base string) (*http.Response, error) {
	return http.Get(base + "/api/v1/v2/grouped") // want "own Huma route /api/v1/v2/grouped"
}

func Raw(base string) (*http.Response, error) {
	return http.Post(base+"/api/v1/raw/7", "application/json", nil) // want "own Huma route /api/v1/raw/\{param\}"
}

// Labeled forwards only path into the request; label is a log string.
func (c *Client) Labeled(ctx context.Context, path, label string) error {
	_ = label
	return c.GetJSON(ctx, path, nil)
}

func UseLabeled(ctx context.Context, c *Client) error {
	return c.Labeled(ctx, "/api/v1/jobs", "/api/v1/ping") // want "own Huma route /api/v1/jobs"
}

// Foreign calls another service; its paths are not this module's routes.
func (c *Client) Foreign(ctx context.Context) error {
	return c.GetJSON(ctx, "/api/tags", nil)
}

type ExternalClient struct{ APIOrigin string }

// huma-check:external
func (c *ExternalClient) get(ctx context.Context, path string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, c.APIOrigin+path, nil)
}

// Project calls a configured external API even though its path overlaps the
// module's own project route and the beginning of its longer close route.
func (c *ExternalClient) Project(ctx context.Context, projectID string) (*http.Request, error) {
	return c.get(ctx, "/api/v1/projects/"+projectID)
}

// The declaration does not exempt neighboring methods on the same receiver.
func (c *ExternalClient) ownGet(ctx context.Context, path string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, c.APIOrigin+path, nil)
}

func (c *ExternalClient) OwnProject(ctx context.Context, projectID string) (*http.Request, error) {
	return c.ownGet(ctx, "/api/v1/projects/"+projectID) // want "own Huma route /api/v1/projects/\{param\}"
}

// Other is a non-requester; route-looking literals here are fine.
func Other() string {
	return "/api/v1/ping"
}

func UseGenerated(ctx context.Context) error {
	return generated.NewClient("http://localhost").Ping(ctx)
}

// Requests made while the package initializes are checked too.
var _, initErr = http.Get("http://localhost/api/v1/queue") // want "own Huma route /api/v1/queue"
