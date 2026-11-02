package shellquote

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSingle(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, input, want string
	}{
		{name: "empty", want: "''"},
		{name: "spaces", input: "/opt/Git Tools/git", want: "'/opt/Git Tools/git'"},
		{name: "single quote", input: "/opt/Git's/git", want: "'/opt/Git'\\''s/git'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, Single(test.input))
		})
	}
}
