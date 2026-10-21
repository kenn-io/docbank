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
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/config"
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
	QualifiedRef string                    `json:"qualified_ref"`
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
	endpoint      string
	key           string
	http          *http.Client
	projects      map[string]int64
	schemaChecked bool
}

func New(cfg config.KataConnection) *Client {
	return &Client{project: cfg.Project, endpoint: cfg.Endpoint, key: cfg.APIKey,
		projects: make(map[string]int64),
		http: &http.Client{Timeout: operationTimeout, Transport: boundedTransport{base: http.DefaultTransport},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// doJSON owns transport policy; generated Kata types own the wire contract.
func (c *Client) doJSON(ctx context.Context, method, path, key string, body, target any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil || len(data) > maxRequestBytes {
			return &Error{Code: "request_too_large"}
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(data))
	if err != nil {
		return &Error{Code: "invalid_connection"}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	// Unknown write outcomes belong to evidence reconciliation, not HTTP retries.
	req.GetBody = nil
	resp, err := c.http.Do(req)
	if err != nil {
		if own, ok := errors.AsType[*Error](err); ok {
			return own
		}
		return &Error{Code: "request_failed", cause: safeCause(ctx, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return &Error{Code: "response_read_failed", cause: safeCause(ctx, err)}
	}
	if resp.StatusCode != http.StatusOK {
		code := "request_failed"
		var envelope wire.ErrorEnvelope
		if json.Unmarshal(data, &envelope) == nil && envelope.ErrorData.Code == "metadata_guard_failed" {
			code = "metadata_guard_failed"
		}
		return &Error{Status: resp.StatusCode, Code: code}
	}
	return decodeResponseNumbers(data, target)
}

// CheckProject applies the configured write scope before replay reconciliation.
func (c *Client) CheckProject(ref string) error {
	_, _, err := c.splitRef(ref, true)
	return err
}

func (c *Client) ensureSchema(ctx context.Context) error {
	if c.schemaChecked {
		return nil
	}
	var response wire.HealthResponseBody
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/health", "", nil, &response); err != nil {
		return err
	}
	if response.APISchemaVersion == nil || !supportedSchema(*response.APISchemaVersion) {
		return &Error{Code: "incompatible_api"}
	}
	c.schemaChecked = true
	return nil
}

func (c *Client) projectID(ctx context.Context, project string) (int64, error) {
	if err := c.ensureSchema(ctx); err != nil {
		return 0, err
	}
	if id := c.projects[project]; id != 0 {
		return id, nil
	}
	var response wire.ListProjectsResponseBody
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects", "", nil, &response); err != nil {
		return 0, err
	}
	for _, p := range response.Projects {
		if p.Name == project && p.Active && p.ID > 0 && p.DeletedAt == nil {
			c.projects[project] = p.ID
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
	// Depth zero returns only the source issue, without its discussion or neighbors.
	var response wire.ReachableGraphResponseBody
	path := issuePath(projectID, id) + "/graph?depth=0"
	if err := c.doJSON(ctx, http.MethodGet, path, "", nil, &response); err != nil {
		return Issue{}, err
	}
	if len(response.Nodes) != 1 || response.Nodes[0].UID != response.SourceUID {
		return Issue{}, &Error{Code: "invalid_response"}
	}
	node := response.Nodes[0]
	return fromIssue(wire.ShowIssueOut{UID: node.UID, ShortID: node.ShortID, Title: node.Title, Body: node.Body, Metadata: node.Metadata}, project)
}

func issuePath(projectID int64, id string) string {
	return "/api/v1/projects/" + strconv.FormatInt(projectID, 10) + "/issues/" + id
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
	var response wire.MutationResponseBody
	err = c.doJSON(ctx, http.MethodPost, "/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues", key,
		wire.CreateIssueBody{Actor: new("docbank"), ForceNew: new(true), Title: title, Body: &body, Metadata: values}, &response)
	if err != nil {
		return Issue{}, err
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
	path := "/api/v1/issues?meta=" + url.QueryEscape("docbank.action="+marker) + "&sort=oldest&limit=1"
	var response wire.ListAllIssuesResponseBody
	err := c.doJSON(ctx, http.MethodGet, path, "", nil, &response)
	if failure, ok := errors.AsType[*Error](err); ok && (failure.Status == 401 || failure.Status == 403 || failure.Status == 404) {
		id, projectErr := c.projectID(ctx, c.project)
		if projectErr != nil {
			return nil, projectErr
		}
		err = c.doJSON(ctx, http.MethodGet, path+"&project_id="+strconv.FormatInt(id, 10), "", nil, &response)
	}
	if err != nil {
		return nil, err
	}
	if len(response.Issues) == 0 {
		return nil, nil //nolint:nilnil // A missing marker is a successful lookup.
	}
	found := response.Issues[0]
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
	var response wire.PatchIssueMetadataResponseBody
	err = c.doJSON(ctx, http.MethodPost, issuePath(projectID, id)+"/metadata", "",
		wire.PatchIssueMetadataBody{Actor: new("docbank"), Patch: map[string]any{key: value}, Guard: &wire.MetadataPatchGuard{MetadataPatchGuard_OneOf: guard}}, &response)
	if err != nil {
		return Issue{}, err
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
	var response wire.CommentResponseBody
	return c.doJSON(ctx, http.MethodPost, issuePath(projectID, id)+"/comments", key,
		wire.CreateCommentBody{Actor: new("docbank"), Body: body}, &response)
}

// HasComment scans durable comment events in pages. Each reply stays bounded
// even when the discussion exceeds our response limit or the replay key expires.
func (c *Client) HasComment(ctx context.Context, ref, uid, body string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	project, _, err := c.splitRef(ref, true)
	if err != nil {
		return false, err
	}
	id, err := c.projectID(ctx, project)
	if err != nil {
		return false, err
	}
	var cursor int64
	pageSize := 8
	for {
		var page wire.PollEventsBody
		path := "/api/v1/projects/" + strconv.FormatInt(id, 10) + "/events?limit=" + strconv.Itoa(pageSize) + "&after_id=" + strconv.FormatInt(cursor, 10)
		if err := c.doJSON(ctx, http.MethodGet, path, "", nil, &page); err != nil {
			if failure, ok := errors.AsType[*Error](err); ok && failure.Code == "response_too_large" && pageSize > 1 {
				pageSize /= 2
				continue
			}
			return false, err
		}
		if page.ResetRequired {
			return false, &Error{Code: "comment_history_unavailable"}
		}
		for _, event := range page.Events {
			if event.Type != "issue.commented" || event.IssueUID == nil || *event.IssueUID != uid {
				continue
			}
			var comment struct {
				Body string `json:"body"`
			}
			if json.Unmarshal(event.Payload, &comment) != nil {
				return false, &Error{Code: "invalid_response"}
			}
			if comment.Body == body {
				return true, nil
			}
		}
		if page.NextAfterID == cursor {
			return false, nil
		}
		if page.NextAfterID < cursor {
			return false, &Error{Code: "invalid_response"}
		}
		cursor = page.NextAfterID
	}
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
