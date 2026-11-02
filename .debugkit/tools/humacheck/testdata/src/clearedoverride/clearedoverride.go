// Package clearedoverride sets huma.DefaultJSONFormat to v2, puts Huma's v1
// value back, and only then copies it into the defaults map, so the API
// still serializes with v1 and the missing-install finding is expected.
package clearedoverride

import (
	"encoding/json"
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
	huma.DefaultJSONFormat = huma.Format{ // want "encoding/json \\(v1\\) is imported"
		Marshal:   func(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) },
		Unmarshal: json.Unmarshal,
	}
	huma.DefaultFormats["application/json"] = huma.DefaultJSONFormat
}

func New() huma.API {
	return humago.New(http.NewServeMux(), huma.DefaultConfig("cleared", "1"))
}
