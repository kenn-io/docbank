// Package server registers the module's Huma routes.
package server

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type Input struct{}
type Output struct{}

const accountsPath = "/accounts"

func apiOperation(method, path, id string) huma.Operation {
	return huma.Operation{Method: method, Path: path, OperationID: id}
}

func handler(context.Context, *Input) (*Output, error) { return &Output{}, nil }

// registerRaw forwards its path parameter to an operation builder, so its
// constant arguments are routes.
func registerRaw(api huma.API, method, path, id string) {
	huma.Register(api, apiOperation(method, path, id), handler)
}

func New(mux *http.ServeMux) huma.API {
	cfg := huma.DefaultConfig("routes", "1")
	f := huma.Format{
		Marshal:   func(w io.Writer, v any) error { return json.MarshalWrite(w, v) },
		Unmarshal: func(data []byte, v any) error { return json.Unmarshal(data, v) },
	}
	cfg.Formats = map[string]huma.Format{"application/json": f}
	api := humago.NewWithPrefix(mux, "/api/v1", cfg)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: "/ping"}, handler)
	huma.Register(api, huma.Operation{Method: http.MethodGet, Path: accountsPath + "/{id}"}, handler)
	huma.Get(api, "/jobs", handler)
	huma.Post(api, "/jobs/{id}/review", handler)
	huma.Post(api, "/projects/{project}/issues/{issue}/actions/close", handler)
	huma.Get(api, "/projects/{project}", handler)
	huma.Register(api, apiOperation(http.MethodGet, "/queue", "get-queue"), handler)
	registerRaw(api, http.MethodPost, "/raw/{id}", "raw")
	group := huma.NewGroup(api, "/v2")
	huma.Get(group, "/grouped", handler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Middleware path checks are not client calls.
		if strings.HasPrefix(r.URL.Path, "/api/v1/ping") {
			w.WriteHeader(http.StatusOK)
		}
	})
	return api
}
