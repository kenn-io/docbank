// Package pkglevel builds its API and group in package-level initializers,
// which count as construction sites and route prefixes like any function
// body would.
package pkglevel

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

var mux = http.NewServeMux()

var api = humago.NewWithPrefix(mux, "/api", huma.DefaultConfig("pkg", "1"))

var group = huma.NewGroup(api, "/v3")
