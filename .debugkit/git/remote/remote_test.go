package gitremote

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClonePathRejectsTraversalAndSeparators(t *testing.T) {
	tests := []Identity{
		{Host: "", Owner: "acme", Name: "widget"},
		{Host: "github.com", Owner: "", Name: "widget"},
		{Host: "github.com", Owner: "acme/../evil", Name: "widget"},
		{Host: "github.com", Owner: "/acme", Name: "widget"},
		{Host: "github.com", Owner: `acme\evil`, Name: "widget"},
		{Host: "github.com", Owner: "acme", Name: "nested/widget"},
	}
	for _, id := range tests {
		if _, err := ClonePath(t.TempDir(), id); err == nil {
			require.FailNow(t, fmt.Sprintf("ClonePath(%+v) succeeded, want error", id))
		}
	}
}

func TestClonePathPartitionsByHost(t *testing.T) {
	base := t.TempDir()
	path, err := ClonePath(base, Identity{Host: "github.com", Owner: "acme", Name: "widget"})
	if err != nil {
		require.FailNow(t, err.Error())
	}
	want := filepath.Join(base, "github.com", "acme", "widget.git")
	if path != want {
		require.FailNow(t, fmt.Sprintf("path = %q, want %q", path, want))
	}
}

func TestValidateRemoteIdentity(t *testing.T) {
	require := require.New(t)
	id := Identity{Host: "github.com", Owner: "acme", Name: "widget"}
	if err := ValidateRemoteIdentity(id, "git@github.com:acme/widget.git"); err != nil {
		require.FailNow(err.Error())
	}
	if err := ValidateRemoteIdentity(id, "https://evil.example.com/acme/widget.git"); err == nil {
		require.FailNow("expected host mismatch")
	}
	if err := ValidateRemoteIdentity(id, "https://github.com/other/widget.git"); err == nil {
		require.FailNow("expected repo mismatch")
	}
	if err := ValidateRemoteIdentity(id, "/tmp/widget.git"); err != nil {
		require.FailNow(fmt.Sprintf("local paths should be accepted: %v", err))
	}
}

func TestCloneURLIdentityNormalizesHostAndPreservesRepoCase(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("example.com/Acme/Widget",
		CloneURLIdentity("https://EXAMPLE.com:443/Acme/Widget.git"),
	)
	assert.Equal("2001:db8::1/Acme/Widget",
		CloneURLIdentity("https://[2001:db8::1]:443/Acme/Widget.git"),
	)
	assert.NotEqual(CloneURLIdentity("ssh://example.com/Acme/Widget.git"),
		CloneURLIdentity("ssh://example.com:443/Acme/Widget.git"),
	)
	assert.Equal("/tmp/Widget.git", CloneURLIdentity(" /tmp/Widget.git "))
}
