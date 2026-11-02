// Package defaultvar assigns huma.DefaultJSONFormat but never writes it into
// a Formats map. huma.DefaultFormats copied the v1 value at init, so the API
// still serializes with v1; the missing-install finding is expected.
package defaultvar

import (
	jsonv2 "encoding/json/v2"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func init() {
	huma.DefaultJSONFormat = huma.Format{
		Marshal:   func(w io.Writer, v any) error { return jsonv2.MarshalWrite(w, v) },
		Unmarshal: func(data []byte, v any) error { return jsonv2.Unmarshal(data, v) },
	}
}

func New() huma.API {
	return humago.New(http.NewServeMux(), huma.DefaultConfig("defaults", "1"))
}
