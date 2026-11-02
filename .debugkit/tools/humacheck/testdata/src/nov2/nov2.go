// Package nov2 builds a Huma API on Huma's defaults without ever installing
// a JSON v2 format; the finding is anchored at go.mod.
package nov2

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func New() huma.API {
	return humago.New(http.NewServeMux(), huma.DefaultConfig("defaults", "1"))
}
