//go:build windows

package fslink

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// junctionTarget must resolve a target the way the symlink the junction
// stands in for would, independent of the working directory.
func TestJunctionTarget(t *testing.T) {
	tests := []struct {
		name, target, link, want string
	}{
		{name: "absolute", target: `C:\shared`, link: `D:\work\link`, want: `C:\shared`},
		{name: "rooted uses link volume", target: `\shared`, link: `D:\work\link`, want: `D:\shared`},
		{name: "rooted with slash", target: `/shared`, link: `D:\work\link`, want: `D:\shared`},
		{name: "relative uses link parent", target: `..\shared`, link: `D:\work\link`, want: `D:\shared`},
		{name: "relative plain", target: `sub`, link: `D:\work\link`, want: `D:\work\sub`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, junctionTarget(tt.target, tt.link))
		})
	}
}
