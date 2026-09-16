package daemonconn

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/docbank/internal/apiclient"
)

// API uses generated operations with this daemon's ownership-proven connection.
func (c *Connection) API() *apiclient.Client {
	return apiclient.NewClient(apiTransport{connection: c})
}

// apiTransport is the generator's transport hook. Route, parameter and body
// definitions belong to the generated client; this hook supplies JSON v2,
// daemon credentials, outcome classification and caller-owned response streams.
type apiTransport struct{ connection *Connection }

func (t apiTransport) GetBaseURL() string { return t.connection.base }

type bodylessOptions struct{ runtime.RequestOptions }

func (bodylessOptions) GetBody() any { return nil }

func (t apiTransport) CreateRequest(ctx context.Context, params runtime.RequestOptionsParameters,
	editors ...runtime.RequestEditorFn,
) (*http.Request, error) {
	var body any
	if params.Options != nil {
		body = params.Options.GetBody()
		params.Options = bodylessOptions{params.Options}
	}
	builder, err := runtime.NewAPIClient(t.GetBaseURL())
	if err != nil {
		return nil, fmt.Errorf("creating daemon transport: %w", err)
	}
	req, err := builder.CreateRequest(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("building generated daemon request: %w", err)
	}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding daemon request: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(encoded))
		req.ContentLength = int64(len(encoded))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(encoded)), nil
		}
	}
	if t.connection.key != "" {
		req.Header.Set("X-Api-Key", t.connection.key)
	}
	for _, edit := range editors {
		if err := edit(ctx, req); err != nil {
			return nil, err
		}
	}
	return req, nil
}

func (t apiTransport) ExecuteRequest(ctx context.Context, req *http.Request, _ string) (*runtime.Response, error) {
	resp, err := t.connection.hc.Do(req) // #nosec G704 -- Generated paths use the caller-selected daemon connection.
	if err != nil {
		return nil, classifyRequestFailure(resp, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	result := &runtime.Response{StatusCode: resp.StatusCode, Headers: resp.Header, Raw: resp}
	if runtime.IsStreamingResponse(ctx) || runtime.IsStreamingResponse(req.Context()) {
		result.Streaming = true
		return result, nil
	}
	defer func() { _ = resp.Body.Close() }()
	result.Content, err = io.ReadAll(resp.Body)
	if err == nil && len(result.Content) == 0 && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		err = io.EOF
	}
	if err != nil {
		return nil, &responseDecodeError{err: err}
	}
	return result, nil
}
