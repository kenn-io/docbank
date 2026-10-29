package vision

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestParseFaces(t *testing.T) {
	t.Parallel()
	valid := `{"box":{"x":0.1,"y":0.2,"width":0.2,"height":0.3},"roll":0,"facing":0,"eyes_open":false,"smile":null}`
	validFace := []document.FaceObservation{{Box: document.FaceBox{X: .1, Y: .2, Width: .2, Height: .3}, Roll: new(0.0), Facing: new(0.0), EyesOpen: new(false)}}
	for _, tc := range []struct {
		name, raw string
		want      []document.FaceObservation
		fails     bool
	}{
		{name: "valid", raw: `{"faces":[` + valid + `]}`, want: validFace},
		{name: "bad box among valid faces", raw: `{"faces":[{"box":{"x":0.8,"y":0,"width":0.21,"height":0.2}},` + valid + `,{}]}`, want: []document.FaceObservation{{Box: document.FaceBox{X: .8, Width: .19999999999999996, Height: .2}}, validFace[0]}},
		{name: "top left clipped", raw: `{"faces":[{"box":{"x":-0.1,"y":-0.2,"width":0.3,"height":0.4}}]}`, want: []document.FaceObservation{{Box: document.FaceBox{Width: .19999999999999998, Height: .2}}}},
		{name: "bottom clipped", raw: `{"faces":[{"box":{"x":0,"y":0.8,"width":0.2,"height":0.3}}]}`, want: []document.FaceObservation{{Box: document.FaceBox{Y: .8, Width: .2, Height: .19999999999999996}}}},
		{name: "unusable box among valid faces", raw: `{"faces":[{"box":{"x":1.1,"y":0,"width":0.2,"height":0.2}},` + valid + `]}`, want: validFace},
		{name: "wrong field types among valid faces", raw: `{"faces":[{"box":"bad"},` + valid + `,{"box":{"x":0,"y":0,"width":0.2,"height":0.2},"roll":"zero"}]}`, want: validFace},
		{name: "wrong item types among valid faces", raw: `{"faces":[null,42,"bad",` + valid + `]}`, want: validFace},
		{name: "empty", raw: `{"faces":[]}`},
		{name: "fenced", raw: "```json\n" + `{"faces":[]}` + "\n```"},
		{name: "leading commentary", raw: "Here are the faces:\n" + `{"faces":[]}`, fails: true},
		{name: "trailing object", raw: `{"faces":[]} {"faces":[`, fails: true},
		{name: "missing", raw: `{"caption":"portrait"}`, fails: true},
		{name: "null array", raw: `{"faces":null}`, fails: true},
		{name: "wrong array type", raw: `{"faces":{}}`, fails: true},
		{name: "malformed", raw: `{"faces":[`, fails: true},
		{name: "invalid", raw: `{"faces":[{}]}`, fails: true},
		{name: "all invalid", raw: `{"faces":[{},null,{"box":{"x":1.1,"y":0,"width":0.21,"height":0.2}}]}`, fails: true},
		{name: "zero width after clipping", raw: `{"faces":[{"box":{"x":-0.2,"y":0,"width":0.2,"height":0.2}}]}`, fails: true},
		{name: "zero height after clipping", raw: `{"faces":[{"box":{"x":0,"y":1,"width":0.2,"height":0.2}}]}`, fails: true},
		{name: "nonfinite coordinate", raw: `{"faces":[{"box":{"x":1e999,"y":0,"width":0.2,"height":0.2}},` + valid + `]}`, want: validFace},
		{name: "invalid roll", raw: `{"faces":[{"box":{"x":0,"y":0,"width":0.2,"height":0.2},"roll":4},` + valid + `]}`, want: validFace},
		{name: "invalid facing", raw: `{"faces":[{"box":{"x":0,"y":0,"width":0.2,"height":0.2},"facing":2},` + valid + `]}`, want: validFace},
		{name: "absent smile key", raw: `{"faces":[{"box":{"x":0,"y":0,"width":0.2,"height":0.3},"roll":0,"facing":1,"eyes_open":true}]}`, want: []document.FaceObservation{{Box: document.FaceBox{Width: .2, Height: .3}, Roll: new(0.0), Facing: new(1.0), EyesOpen: new(true)}}},
		{name: "measurement type", raw: `{"faces":[{"box":{"x":0,"y":0,"width":0.2,"height":0.2},"roll":"zero","facing":1,"eyes_open":null,"smile":null}]}`, fails: true},
		{name: "null measurements", raw: `{"faces":[{"box":{"x":0,"y":0,"width":0.2,"height":0.2},"roll":null,"facing":null,"eyes_open":null,"smile":null}]}`, want: []document.FaceObservation{{Box: document.FaceBox{Width: .2, Height: .2}}}},
		{name: "too many", raw: `{"faces":[` + strings.Repeat(valid+",", 1000) + valid + `]}`, fails: true},
		{name: "too many including invalid", raw: `{"faces":[` + strings.Repeat("{},", 1000) + valid + `]}`, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseFaces([]byte(tc.raw))
			if tc.fails {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.want != nil {
				require.Equal(t, tc.want, result.Faces)
			} else {
				require.Empty(t, result.Faces)
			}
		})
	}
}
