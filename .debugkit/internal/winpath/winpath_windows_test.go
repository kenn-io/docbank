package winpath

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLong(t *testing.T) {
	long := `C:\` + strings.Repeat(`d\`, 150) + "file"
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "short absolute", path: `C:\dir\file`, want: `C:\dir\file`},
		{name: "long absolute", path: long, want: `\\?\` + long},
		{name: "long with slashes and dots", path: strings.ReplaceAll(long, `\`, `/`) + `/../other`, want: `\\?\` + strings.TrimSuffix(long, "file") + "other"},
		{name: "long UNC", path: `\\server\share\` + strings.Repeat(`d\`, 150) + "file", want: `\\?\UNC\server\share\` + strings.Repeat(`d\`, 150) + "file"},
		{name: "already extended", path: `\\?\` + long, want: `\\?\` + long},
		{name: "NT prefix", path: `\??\` + long, want: `\??\` + long},
		{name: "long device path", path: `\\.\` + long[3:], want: `\\.\` + long[3:]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Long(tt.path))
		})
	}
}

// A long relative path counts the working directory toward the limit, as
// the os package does.
func TestLongRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	rel := strings.Repeat(`d\`, 130) + "file"

	got := Long(rel)

	require.True(t, strings.HasPrefix(got, `\\?\`), got)
	want, err := filepath.Abs(rel)
	require.NoError(t, err)
	assert.Equal(t, `\\?\`+want, got)
}
