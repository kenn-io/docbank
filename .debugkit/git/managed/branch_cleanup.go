package managedworktree

import (
	"context"
	"errors"
	"time"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// RemoveBranchOptions authorizes cleanup independently of a checkout, such as
// after removing stale registration metadata. Force does not bypass Git's
// refusal to delete a branch checked out in another worktree.
type RemoveBranchOptions struct {
	ProjectRoot string
	Branch      BranchRemoval
	Runner      gitcmd.Runner
	RunGit      GitRunner
}

// RemoveBranch reports whether the branch is absent after the attempt. An error
// can accompany completed deletion, including cancellation after Git succeeds.
// Callers provide repository synchronization and authority for the branch.
func RemoveBranch(ctx context.Context, opts RemoveBranchOptions) (bool, error) {
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, nil)
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return false, err
	}
	branches, err := prepareBranchRemovals(ctx, root, "", RemoveWorktreeOptions{Branches: []BranchRemoval{opts.Branch}})
	if err != nil {
		return false, err
	}
	if err := removeBranch(ctx, root, branches[0]); err != nil {
		inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		exists, inspectErr := localBranchExists(inspectCtx, root, opts.Branch.Name)
		return !exists && inspectErr == nil, errors.Join(err, inspectErr)
	}
	return true, nil
}
