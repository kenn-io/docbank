// Package kataclient adapts Kata's public client to Docbank evidence operations.
package kataclient

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/docbank/internal/config"
	kata "go.kenn.io/kata/pkg/client"
	wire "go.kenn.io/kata/pkg/client/generated"
)

const (
	operationTimeout = 10 * time.Second
	maxRequestBytes  = 1 << 20
	maxResponseBytes = 2 << 20
)

type Issue struct {
	UID          string                    `json:"uid"`
	ShortID      string                    `json:"short_id"`
	Title        string                    `json:"title"`
	Body         string                    `json:"body"`
	Metadata     map[string]jsontext.Value `json:"metadata"`
	Comments     []Comment                 `json:"comments,omitzero"`
	QualifiedRef string                    `json:"qualified_ref"`
}

type Comment struct {
	Body string `json:"body"`
}

// Error exposes only an HTTP status and a recognized machine code. Remote
// error messages and transport details can contain credentials or issue data.
type Error struct {
	Status int
	Code   string
	cause  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("Kata request failed (status %d, code %s)", e.Status, e.Code)
}

func (e *Error) Unwrap() error { return e.cause }

func IsGuardConflict(err error) bool {
	var target *Error
	return errors.As(err, &target) && target.Status == http.StatusConflict && target.Code == "metadata_guard_failed"
}

// Client uses only the explicitly resolved Docbank connection.
type Client struct {
	project       string
	http          *http.Client
	api           *kata.Client
	initErr       error
	schemaChecked atomic.Bool
}

func New(cfg config.KataConnection) *Client {
	c := &Client{project: cfg.Project, http: &http.Client{Timeout: operationTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	c.api, c.initErr = kata.NewWithHTTPClient(cfg.Endpoint, c.http, kata.WithRequestEditor(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		// A write with an unknown outcome must be reconciled by the service.
		req.GetBody = nil
		return nil
	}))
	c.http.Transport = boundedTransport{base: http.DefaultTransport}
	return c
}

// CheckProject applies the configured write scope before replay reconciliation.
func (c *Client) CheckProject(ref string) error {
	_, _, err := c.splitRef(ref, true)
	return err
}

func (c *Client) failure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if own, ok := errors.AsType[*Error](err); ok {
		return own
	}
	code := "request_failed"
	if envelope, ok := errors.AsType[wire.ErrorEnvelope](err); ok && envelope.ErrorData.Code == "metadata_guard_failed" {
		code = "metadata_guard_failed"
	}
	return &Error{Status: kata.StatusCode(err), Code: code, cause: safeCause(ctx, err)}
}

func (c *Client) ensureSchema(ctx context.Context) error {
	if c.initErr != nil {
		return &Error{Code: "invalid_connection"}
	}
	if c.schemaChecked.Load() {
		return nil
	}
	response, err := c.api.Health(ctx)
	if err != nil {
		return c.failure(ctx, err)
	}
	if response.APISchemaVersion == nil || !supportedSchema(*response.APISchemaVersion) {
		return &Error{Code: "incompatible_api"}
	}
	c.schemaChecked.Store(true)
	return nil
}

func (c *Client) projectID(ctx context.Context, project string) (int64, error) {
	if err := c.ensureSchema(ctx); err != nil {
		return 0, err
	}
	response, err := c.api.ListProjects(ctx, &wire.ListProjectsRequestOptions{})
	if err != nil {
		return 0, c.failure(ctx, err)
	}
	for _, p := range response.Projects {
		if p.Name == project && p.Active && p.ID > 0 && p.DeletedAt == nil {
			return p.ID, nil
		}
	}
	return 0, &Error{Code: "project_unavailable"}
}

func (c *Client) Get(ctx context.Context, ref string) (Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	project, id, err := c.splitRef(ref, false)
	if err != nil {
		return Issue{}, err
	}
	return c.get(ctx, project, id)
}

func (c *Client) get(ctx context.Context, project, id string) (Issue, error) {
	if !validID(id) {
		return Issue{}, &Error{Code: "invalid_response"}
	}
	projectID, err := c.projectID(ctx, project)
	if err != nil {
		return Issue{}, err
	}
	response, err := c.api.ShowIssueWithResponse(ctx, &wire.ShowIssueRequestOptions{PathParams: &wire.ShowIssuePath{ProjectID: projectID, Ref: id}})
	if err != nil {
		return Issue{}, c.failure(ctx, err)
	}
	if err := decodeResponseNumbers(response.Body, response.JSON200); err != nil {
		return Issue{}, err
	}
	issue, err := fromIssue(response.JSON200.Issue, project)
	if err != nil {
		return Issue{}, err
	}
	for _, comment := range response.JSON200.Comments {
		issue.Comments = append(issue.Comments, Comment{Body: comment.Body})
	}
	return issue, nil
}

func fromIssue(source wire.ShowIssueOut, project string) (Issue, error) {
	issue := Issue{UID: source.UID, ShortID: source.ShortID, Title: source.Title, Body: source.Body, Metadata: make(map[string]jsontext.Value, len(source.Metadata))}
	if !validID(issue.UID) || !validID(issue.ShortID) {
		return Issue{}, &Error{Code: "invalid_response"}
	}
	for key, value := range source.Metadata {
		data, err := json.Marshal(value)
		if err != nil {
			return Issue{}, &Error{Code: "invalid_response"}
		}
		issue.Metadata[key] = data
	}
	issue.QualifiedRef = project + "#" + issue.ShortID
	return issue, nil
}

func validKey(key string) bool {
	return strings.TrimSpace(key) != "" && len(key) <= 4096 && !strings.ContainsAny(key, "\r\n")
}

func (c *Client) Create(ctx context.Context, key, title, body string, metadata map[string]jsontext.Value) (Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if !validKey(key) || strings.TrimSpace(title) == "" {
		return Issue{}, &Error{Code: "invalid_request"}
	}
	projectID, err := c.projectID(ctx, c.project)
	if err != nil {
		return Issue{}, err
	}
	values := make(map[string]any, len(metadata))
	for key, value := range metadata {
		values[key] = value
	}
	response, err := c.api.CreateIssue(ctx, &wire.CreateIssueRequestOptions{
		PathParams: &wire.CreateIssuePath{ProjectID: strconv.FormatInt(projectID, 10)},
		Body:       &wire.CreateIssueBody{Actor: new("docbank"), ForceNew: new(true), Title: title, Body: &body, Metadata: values},
		Header:     &wire.CreateIssueHeaders{IdempotencyKey: &key},
	})
	if err != nil {
		return Issue{}, c.failure(ctx, err)
	}
	return c.get(ctx, c.project, response.Issue.UID)
}

// FindMarker includes closed issues and follows issues moved to another project.
func (c *Client) FindMarker(ctx context.Context, marker string) (*Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if marker == "" || len(marker) > maxRequestBytes {
		return nil, &Error{Code: "invalid_request"}
	}
	if err := c.ensureSchema(ctx); err != nil {
		return nil, err
	}
	query := &wire.ListAllIssuesQuery{Meta: []string{"docbank.action=" + marker}, Sort: new(wire.ListAllIssuesQuerySort("oldest")), Limit: new(int64(1))}
	response, err := c.api.ListAllIssuesWithResponse(ctx, &wire.ListAllIssuesRequestOptions{Query: query})
	if status := kata.StatusCode(err); status == 401 || status == 403 || status == 404 {
		id, projectErr := c.projectID(ctx, c.project)
		if projectErr != nil {
			return nil, projectErr
		}
		query.ProjectID = &id
		response, err = c.api.ListAllIssuesWithResponse(ctx, &wire.ListAllIssuesRequestOptions{Query: query})
	}
	if err != nil {
		return nil, c.failure(ctx, err)
	}
	if err := decodeResponseNumbers(response.Body, response.JSON200); err != nil {
		return nil, err
	}
	if len(response.JSON200.Issues) == 0 {
		return nil, nil //nolint:nilnil // A missing marker is a successful lookup.
	}
	found := response.JSON200.Issues[0]
	if !config.ValidKataProject(found.ProjectName) {
		return nil, &Error{Code: "invalid_response"}
	}
	issue, err := fromIssue(wire.ShowIssueOut{UID: found.UID, ShortID: found.ShortID, Title: found.Title, Body: found.Body, Metadata: found.Metadata}, found.ProjectName)
	return &issue, err
}

func (c *Client) Patch(ctx context.Context, ref, key string, previous, value jsontext.Value) (Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	project, id, err := c.splitRef(ref, true)
	if err != nil {
		return Issue{}, err
	}
	if key == "" || !value.IsValid() || previous != nil && (!previous.IsValid() || string(bytes.TrimSpace(previous)) == "null") {
		return Issue{}, &Error{Code: "invalid_request"}
	}
	guard := &wire.MetadataPatchGuard_OneOf{}
	if previous == nil {
		guard.N, guard.B = 2, wire.MetadataPatchGuard_OneOf_1{Key: key, IfAbsent: wire.True}
	} else {
		guard.N, guard.A = 1, wire.MetadataPatchGuard_OneOf_0{Key: key, IfValue: string(previous)}
	}
	projectID, err := c.projectID(ctx, project)
	if err != nil {
		return Issue{}, err
	}
	response, err := c.api.PatchIssueMetadata(ctx, &wire.PatchIssueMetadataRequestOptions{
		PathParams: &wire.PatchIssueMetadataPath{ProjectID: projectID, Ref: id},
		Body:       &wire.PatchIssueMetadataBody{Actor: new("docbank"), Patch: map[string]any{key: value}, Guard: &wire.MetadataPatchGuard{MetadataPatchGuard_OneOf: guard}},
	})
	if err != nil {
		return Issue{}, c.failure(ctx, err)
	}
	return c.get(ctx, project, response.Issue.UID)
}

func (c *Client) Comment(ctx context.Context, ref, key, body string) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	project, id, err := c.splitRef(ref, true)
	if err != nil {
		return err
	}
	if !validKey(key) {
		return &Error{Code: "invalid_request"}
	}
	projectID, err := c.projectID(ctx, project)
	if err != nil {
		return err
	}
	_, err = c.api.CreateComment(ctx, &wire.CreateCommentRequestOptions{
		PathParams: &wire.CreateCommentPath{ProjectID: strconv.FormatInt(projectID, 10), Ref: id},
		Body:       &wire.CreateCommentBody{Actor: new("docbank"), Body: body},
		Header:     &wire.CreateCommentHeaders{IdempotencyKey: &key},
	})
	return c.failure(ctx, err)
}

// boundedTransport enforces Docbank's limits without owning Kata wire types.
type boundedTransport struct{ base http.RoundTripper }

func (b boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		data, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBytes+1))
		_ = req.Body.Close()
		if err != nil || len(data) > maxRequestBytes {
			return nil, &Error{Code: "request_too_large"}
		}
		req.Body = io.NopCloser(bytes.NewReader(data))
	}
	response, err := b.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	_ = response.Body.Close()
	if err != nil {
		return nil, &Error{Status: response.StatusCode, Code: "response_read_failed", cause: safeCause(req.Context(), err)}
	}
	if len(data) > maxResponseBytes {
		return nil, &Error{Status: response.StatusCode, Code: "response_too_large"}
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	return response, nil
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func (c *Client) splitRef(ref string, write bool) (string, string, error) {
	project, id := c.project, ref
	if p, tail, ok := strings.Cut(ref, "#"); ok {
		project, id = p, tail
	}
	if !config.ValidKataProject(project) || !validID(id) {
		return "", "", &Error{Code: "invalid_reference"}
	}
	if write && project != c.project {
		return "", "", &Error{Code: "wrong_project"}
	}
	return project, id, nil
}

func supportedSchema(version string) bool {
	version = strings.TrimPrefix(version, "v")
	core, _, _ := strings.Cut(version, "+")
	core, prerelease, _ := strings.Cut(core, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	var numbers [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strconv.Itoa(n) != part {
			return false
		}
		numbers[i] = n
	}
	if numbers[0] > 0 || numbers[1] > 21 {
		return true
	}
	return numbers[1] == 21 && (numbers[2] > 0 || prerelease == "")
}

// Retain only standard cancellation causes, never transport errors containing URLs.
func safeCause(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

// Generated metadata maps use any. Keep opaque JSON integers exact when
// adapting them back into raw evidence for subsequent guarded writes.
func decodeResponseNumbers(data []byte, target any) error {
	decoder := jsonv1.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return &Error{Code: "invalid_response"}
	}
	return nil
}
