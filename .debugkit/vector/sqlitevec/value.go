package sqlitevec

import (
	"strconv"
	"strings"
)

func vectorValue(vector []float32) (string, any, error) {
	return "vec_f32(?)", vectorLiteral(vector), nil
}

func vectorLiteral(vector []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vector {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
