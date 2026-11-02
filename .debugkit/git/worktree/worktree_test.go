package gitworktree

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	gittest "go.kenn.io/kit/git/test"
)

func TestCreateCaptureAndApplyPatch(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	repo := gittest.NewRepoWithCommit(t)

	wt, err := Create(ctx, repo.Root, "HEAD", Options{ParentDir: t.TempDir()})
	if err != nil {
		require.FailNow(err.Error())
	}
	t.Cleanup(func() { _ = wt.Close(ctx) })

	if err := os.WriteFile(filepath.Join(wt.Dir, "base.txt"), []byte("changed\n"), 0o644); err != nil {
		require.FailNow(err.Error())
	}
	patch, err := wt.CapturePatch(ctx)
	if err != nil {
		require.FailNow(err.Error())
	}
	if patch == "" {
		require.FailNow("expected non-empty patch")
	}
	if err := CheckPatch(ctx, repo.Root, patch); err != nil {
		require.FailNow(fmt.Sprintf("patch should apply cleanly: %v", err))
	}
	if err := ApplyPatch(ctx, repo.Root, patch); err != nil {
		require.FailNow(err.Error())
	}
	got, err := os.ReadFile(filepath.Join(repo.Root, "base.txt"))
	if err != nil {
		require.FailNow(err.Error())
	}
	if string(got) != "changed\n" {
		require.FailNow(fmt.Sprintf("base.txt = %q, want changed", got))
	}
	err = CheckPatch(ctx, repo.Root, patch)
	require.Error(err, "patch should conflict after being applied")
	require.ErrorAs(err, new(*PatchConflictError))
}

func TestGitmodulesFileProtocolDetection(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`url = "../local.git"`, true},
		{`url = file:///tmp/local.git`, true},
		{`url = https://github.com/acme/widget.git`, false},
		{`path = deps/widget`, false},
	}
	for _, tt := range tests {
		url, ok := ParseGitmodulesURL(tt.line)
		got := ok && IsFileProtocolURL(url)
		if got != tt.want {
			require.FailNow(t, fmt.Sprintf("line %q got %v want %v", tt.line, got, tt.want))
		}
	}
}
