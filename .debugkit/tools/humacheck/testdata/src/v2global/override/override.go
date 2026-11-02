// Package override replaces Huma's process-wide default JSON format.
package override

import (
	jsonv2 "encoding/json/v2"
	"io"

	"github.com/danielgtaylor/huma/v2"
)

func init() {
	huma.DefaultJSONFormat = huma.Format{
		Marshal: func(w io.Writer, value any) error {
			return jsonv2.MarshalWrite(w, value)
		},
		Unmarshal: func(data []byte, value any) error {
			return jsonv2.Unmarshal(data, value)
		},
	}
	huma.DefaultFormats["application/json"] = huma.DefaultJSONFormat
	huma.DefaultFormats["json"] = huma.DefaultJSONFormat
}
