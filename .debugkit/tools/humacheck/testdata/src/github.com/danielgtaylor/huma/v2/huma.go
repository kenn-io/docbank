// Package huma is a minimal stub of github.com/danielgtaylor/huma/v2 for
// analyzer fixtures.
package huma

import (
	"context"
	"encoding/json"
	"io"
)

type Format struct {
	Marshal   func(w io.Writer, v any) error
	Unmarshal func(data []byte, v any) error
}

type Config struct {
	Title       string
	OpenAPIPath string
	DocsPath    string
	Formats     map[string]Format
}

type API interface{ OpenAPI() *OpenAPI }

type OpenAPI struct{}

type Adapter interface{ Handle(*Operation) }

type Operation struct {
	OperationID string
	Method      string
	Path        string
	Summary     string
}

var DefaultJSONFormat = Format{
	Marshal:   func(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) },
	Unmarshal: json.Unmarshal,
}

var DefaultFormats = map[string]Format{
	"application/json": DefaultJSONFormat,
	"json":             DefaultJSONFormat,
}

func DefaultConfig(title, version string) Config {
	return Config{Title: title, Formats: DefaultFormats}
}

type api struct{}

func (api) OpenAPI() *OpenAPI { return &OpenAPI{} }

func NewAPI(config Config, a Adapter) API { return api{} }

type Group struct{ API }

func NewGroup(a API, prefixes ...string) *Group { return &Group{API: a} }

func Register[I, O any](a API, op Operation, handler func(context.Context, *I) (*O, error)) {}

func Get[I, O any](a API, path string, handler func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
}

func Post[I, O any](a API, path string, handler func(context.Context, *I) (*O, error), opts ...func(*Operation)) {
}
