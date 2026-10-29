package vision

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestParseFaces(t *testing.T) {
	t.Parallel()
	valid := `{"box":{"x":0.1,"y":0.2,"width":0.2,"height":0.3},"roll":0,"facing":0,"eyes_open":false,"smile":null}`
	for _, tc := range []struct {
		name, raw string
		want      *document.FaceObservation
		fails     bool
	}{
		{name: "valid", raw: `{"faces":[` + valid + `]}`, want: &document.FaceObservation{Box: document.FaceBox{X: .1, Y: .2, Width: .2, Height: .3}, Roll: new(0.0), Facing: new(0.0), EyesOpen: new(false)}},
		{name: "empty", raw: `{"faces":[]}`},
		{name: "whitespace", raw: " \t\r\n" + `{"faces":[]}` + " \n"},
		{name: "fenced", raw: "```json\n" + `{"faces":[]}` + "\n```"},
		{name: "trailing object", raw: `{"faces":[]} {"faces":[`, fails: true},
		{name: "missing", raw: `{"caption":"portrait"}`, fails: true},
		{name: "null", raw: `{"faces":null}`, fails: true},
		{name: "malformed", raw: `{"faces":[`, fails: true},
		{name: "invalid", raw: `{"faces":[{}]}`, fails: true},
		{name: "absent smile key", raw: `{"faces":[{"box":{"x":0,"y":0,"width":0.2,"height":0.3},"roll":0,"facing":1,"eyes_open":true}]}`, want: &document.FaceObservation{Box: document.FaceBox{Width: .2, Height: .3}, Roll: new(0.0), Facing: new(1.0), EyesOpen: new(true)}},
		{name: "too many", raw: `{"faces":[` + strings.Repeat(valid+",", 1000) + valid + `]}`, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseFaces([]byte(tc.raw))
			if tc.fails {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.want != nil {
				require.Equal(t, []document.FaceObservation{*tc.want}, result.Faces)
			} else {
				require.Empty(t, result.Faces)
			}
		})
	}
	for _, tc := range []struct {
		fields string
		fails  bool
	}{
		{`"roll":4,"facing":1`, true},
		{`"roll":0,"facing":-1`, true},
		{`"roll":"zero","facing":1`, true},
		{`"roll":null,"facing":null`, false},
	} {
		raw := fmt.Sprintf(`{"faces":[{"box":{"x":0,"y":0,"width":0.2,"height":0.2},%s,"eyes_open":null,"smile":null}]}`, tc.fields)
		result, err := ParseFaces([]byte(raw))
		if tc.fails {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, []document.FaceObservation{{Box: document.FaceBox{Width: .2, Height: .2}}}, result.Faces)
		}
	}
}
