// Package kataclient implements Docbank's bounded subset of Kata's native API.
package kataclient

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/docbank/internal/config"
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

// Client does not retry writes: after an unknown outcome the caller must use
// its action marker or idempotency key to reconcile explicitly.
type Client struct {
	endpoint      string
	apiKey        string
	project       string
	http          *http.Client
	invalid       bool
	schemaChecked atomic.Bool
}

// New also validates direct callers, so constructing a KataConnection by hand
// cannot bypass the endpoint restrictions enforced by configuration loading.
// Invalid connections fail locally when an operation is attempted.
func New(cfg config.KataConnection) *Client {
	u, err := url.Parse(cfg.Endpoint)
	valid := err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == "" && u.RawPath == "" && (u.Path == "" || u.Path == "/") && (u.Scheme == "https" || u.Scheme == "http")
	if valid && u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		valid = strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
	}
	valid = valid && config.ValidKataProject(cfg.Project) && cfg.APIKey != "" && len(cfg.APIKey) <= 4096 && !strings.ContainsAny(cfg.APIKey, "\r\n")
	return &Client{endpoint: strings.TrimRight(cfg.Endpoint, "/"), apiKey: cfg.APIKey, project: cfg.Project, invalid: !valid,
		http: &http.Client{Timeout: operationTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
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

// ensureSchema refuses servers that predate guarded metadata and native idempotency.
func (c *Client) ensureSchema(ctx context.Context) error {
	if c.schemaChecked.Load() {
		return nil
	}
	var health struct {
		Version string `json:"api_schema_version"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/health", nil, "", &health); err != nil {
		return err
	}
	if !supportedSchema(health.Version) {
		return &Error{Code: "incompatible_api"}
	}
	c.schemaChecked.Store(true)
	return nil
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

func (c *Client) issuePath(ctx context.Context, project string) (string, error) {
	if err := c.ensureSchema(ctx); err != nil {
		return "", err
	}
	var response struct {
		Projects []struct {
			ID        int64          `json:"id"`
			Name      string         `json:"name"`
			Active    bool           `json:"active"`
			DeletedAt jsontext.Value `json:"deleted_at"`
		} `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects", nil, "", &response); err != nil {
		return "", err
	}
	for _, p := range response.Projects {
		if p.Name == project && p.Active && p.ID > 0 && (len(p.DeletedAt) == 0 || string(p.DeletedAt) == "null") {
			return "/api/v1/projects/" + strconv.FormatInt(p.ID, 10) + "/issues", nil
		}
	}
	return "", &Error{Code: "project_unavailable"}
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
	path, err := c.issuePath(ctx, project)
	if err != nil {
		return Issue{}, err
	}
	var response struct {
		Issue    Issue     `json:"issue"`
		Comments []Comment `json:"comments"`
	}
	if err := c.do(ctx, http.MethodGet, path+"/"+id, nil, "", &response); err != nil {
		return Issue{}, err
	}
	if err := qualify(&response.Issue, project); err != nil {
		return Issue{}, err
	}
	response.Issue.Comments = response.Comments
	return response.Issue, nil
}

func qualify(issue *Issue, project string) error {
	if !validID(issue.UID) || !validID(issue.ShortID) {
		return &Error{Code: "invalid_response"}
	}
	issue.QualifiedRef = project + "#" + issue.ShortID
	return nil
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
	request := struct {
		Actor    string                    `json:"actor"`
		ForceNew bool                      `json:"force_new"`
		Title    string                    `json:"title"`
		Body     string                    `json:"body"`
		Metadata map[string]jsontext.Value `json:"metadata"`
	}{"docbank", true, title, body, metadata}
	path, err := c.issuePath(ctx, c.project)
	if err != nil {
		return Issue{}, err
	}
	var response struct {
		Issue Issue `json:"issue"`
	}
	if err := c.do(ctx, http.MethodPost, path, request, key, &response); err != nil {
		return Issue{}, err
	}
	return c.get(ctx, c.project, response.Issue.UID)
}

// FindMarker includes closed issues by leaving Kata's status filter unset.
func (c *Client) FindMarker(ctx context.Context, marker string) (*Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if marker == "" || len(marker) > maxRequestBytes {
		return nil, &Error{Code: "invalid_request"}
	}
	if err := c.ensureSchema(ctx); err != nil {
		return nil, err
	}
	query := url.Values{"meta": {"docbank.action=" + marker}, "sort": {"oldest"}, "limit": {"1"}}
	var response struct {
		Issues []struct {
			Issue

			ProjectName string `json:"project_name"`
		} `json:"issues"`
	}
	project := c.project
	err := c.do(ctx, http.MethodGet, "/api/v1/issues?"+query.Encode(), nil, "", &response)
	if failure, ok := errors.AsType[*Error](err); ok && (failure.Status == 401 || failure.Status == 403 || failure.Status == 404) {
		path, pathErr := c.issuePath(ctx, c.project)
		if pathErr != nil {
			return nil, pathErr
		}
		err = c.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, "", &response)
	}
	if err != nil {
		return nil, err
	}
	if len(response.Issues) > 0 && response.Issues[0].ProjectName != "" {
		project = response.Issues[0].ProjectName
		if !config.ValidKataProject(project) {
			return nil, &Error{Code: "invalid_response"}
		}
	}
	if len(response.Issues) == 0 {
		return nil, nil //nolint:nilnil // An absent durable marker is a successful lookup.
	}
	issue := response.Issues[0].Issue
	if err := qualify(&issue, project); err != nil {
		return nil, err
	}
	return &issue, nil
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
	guard := struct {
		Key      string  `json:"key"`
		IfAbsent bool    `json:"if_absent,omitzero"`
		IfValue  *string `json:"if_value,omitzero"`
	}{Key: key, IfAbsent: previous == nil}
	if previous != nil {
		v := string(previous)
		guard.IfValue = &v
	}
	request := struct {
		Actor string                    `json:"actor"`
		Patch map[string]jsontext.Value `json:"patch"`
		Guard any                       `json:"guard"`
	}{"docbank", map[string]jsontext.Value{key: value}, guard}
	path, err := c.issuePath(ctx, project)
	if err != nil {
		return Issue{}, err
	}
	var response struct {
		Issue Issue `json:"issue"`
	}
	if err := c.do(ctx, http.MethodPost, path+"/"+id+"/metadata", request, "", &response); err != nil {
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
	request := struct {
		Actor string `json:"actor"`
		Body  string `json:"body"`
	}{"docbank", body}
	path, err := c.issuePath(ctx, project)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path+"/"+id+"/comments", request, key, nil)
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxRequestBytes-b.Len() {
		return 0, &Error{Code: "request_too_large"}
	}
	_, _ = b.Buffer.Write(p) // bytes.Buffer.Write always returns a nil error.
	return len(p), nil
}

func (c *Client) do(ctx context.Context, method, path string, request any, key string, response any) error {
	if c.invalid {
		return &Error{Code: "invalid_connection"}
	}
	var body io.Reader
	if request != nil {
		var encoded boundedBuffer
		if err := json.MarshalWrite(&encoded, request); err != nil {
			return &Error{Code: "invalid_request"}
		}
		// Keep GetBody unset: net/http must not replay a write automatically,
		// even when it carries an Idempotency-Key header.
		body = io.NopCloser(bytes.NewReader(encoded.Bytes()))
	}
	if len(path) > maxRequestBytes {
		return &Error{Code: "request_too_large"}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return &Error{Code: "invalid_request"}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if request != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return &Error{Code: "context_done", cause: ctx.Err()}
		}
		return &Error{Code: "transport_error", cause: safeCause(ctx, err)}
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return &Error{Status: res.StatusCode, Code: "response_read_failed", cause: safeCause(ctx, err)}
	}
	if len(data) > maxResponseBytes {
		return &Error{Status: res.StatusCode, Code: "response_too_large"}
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		code := "http_error"
		// Never echo arbitrary remote codes; even an error code can contain
		// private remote text. This is the one semantic failure we consume.
		if failure.Error.Code == "metadata_guard_failed" {
			code = failure.Error.Code
		}
		return &Error{Status: res.StatusCode, Code: code}
	}
	if response == nil {
		return nil
	}
	if err := json.Unmarshal(data, response); err != nil {
		return &Error{Status: res.StatusCode, Code: "invalid_response"}
	}
	return nil
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
