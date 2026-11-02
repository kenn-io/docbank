// Package v2shared installs a format defined in a sibling package.
package v2shared

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"v2shared/codecs"
)

func New() huma.API {
	config := huma.DefaultConfig("shared", "1")
	config.Formats["application/json"] = codecs.JSONFormat
	return humago.New(http.NewServeMux(), config)
}
