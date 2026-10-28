package vision

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseFaces(t *testing.T) {
	t.Parallel()
	valid := `{"box":{"x":0.1,"y":0.2,"width":0.2,"height":0.3},"roll":0,"facing":0,"eyes_open":false,"smile":null}`
	for _, tc := range []struct {
		name, raw string
		count     int
		fails     bool
	}{
		{"valid", `{"faces":[` + valid + `]}`, 1, false},
		{"empty", `{"faces":[]}`, 0, false},
		{"missing", `{"caption":"portrait"}`, 0, true},
		{"null", `{"faces":null}`, 0, true},
		{"object", `{"faces":{}}`, 0, true},
		{"malformed", `{"faces":[`, 0, true},
		{"mixed", `{"faces":[{},null,{"box":{"width":2,"height":1},"roll":0,"facing":1,"eyes_open":true,"smile":false},` + valid + `]}`, 1, false},
		{"missing measurement", `{"faces":[{"box":{"width":0.2,"height":0.3},"roll":0,"facing":1,"eyes_open":true}]}`, 0, false},
		{"too many", `{"faces":[` + strings.Repeat(valid+",", 1000) + valid + `]}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseFaces([]byte(tc.raw))
			if tc.fails {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Faces, tc.count)
			if tc.count > 0 {
				require.Zero(t, *result.Faces[0].Roll)
				require.Zero(t, *result.Faces[0].Facing)
				require.False(t, *result.Faces[0].EyesOpen)
				require.Nil(t, result.Faces[0].Smile)
			}
		})
	}
	for _, fields := range []string{`"roll":4,"facing":1`, `"roll":0,"facing":-1`, `"roll":"zero","facing":1`, `"roll":null,"facing":null`} {
		raw := fmt.Sprintf(`{"faces":[{"box":{"width":0.2,"height":0.2},%s,"eyes_open":null,"smile":null}]}`, fields)
		result, err := ParseFaces([]byte(raw))
		require.NoError(t, err)
		if fields == `"roll":null,"facing":null` {
			require.Len(t, result.Faces, 1)
			require.Nil(t, result.Faces[0].Roll)
		} else {
			require.Empty(t, result.Faces)
		}
	}
}
