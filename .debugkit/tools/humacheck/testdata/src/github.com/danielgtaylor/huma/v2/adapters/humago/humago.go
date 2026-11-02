// Package humago is a minimal stub of the net/http Huma adapter.
package humago

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type Mux interface {
	Handle(pattern string, handler http.Handler)
}

type adapter struct{}

func (adapter) Handle(*huma.Operation) {}

func NewAdapter(m Mux, prefix string) huma.Adapter { return adapter{} }

func New(m Mux, config huma.Config) huma.API { return huma.NewAPI(config, NewAdapter(m, "")) }

func NewWithPrefix(m Mux, prefix string, config huma.Config) huma.API {
	return huma.NewAPI(config, NewAdapter(m, prefix))
}
