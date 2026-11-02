package managedworktree

import (
	"context"
	"errors"
	"strings"
)

// WorktreeHookOptions prepares a caller-trusted lifecycle script independently
// of a Git mutation. MergeRequest requires the script to already exist outside
// the destination. RunHook supplies application process and platform policy.
type WorktreeHookOptions struct {
	ProjectRoot string
	Path        string
	// BaseDir selects the creation destination when Path is empty. The same
	// default directory and branch slug as CreateWorktreeOnDisk are used.
	BaseDir           string
	Branch            string
	Script            string
	WorktreeName      string
	EnvironmentPrefix string
	MergeRequest      bool
	RunHook           HookRunner
}

// PreparedWorktreeHook keeps validated paths private until the caller chooses
// to execute the script, for example after releasing a repository lock.
type PreparedWorktreeHook struct {
	options WorktreeHookOptions
}

// PrepareWorktreeHook validates the script without running it or creating a tree.
func PrepareWorktreeHook(ctx context.Context, opts WorktreeHookOptions) (*PreparedWorktreeHook, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	opts.ProjectRoot, err = absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Path) == "" {
		opts.Path, err = resolveWorktreeDestination(opts.ProjectRoot, strings.TrimSpace(opts.Branch), "", opts.BaseDir)
	} else {
		opts.Path, err = absRequired(opts.Path, "worktree path")
	}
	if err != nil {
		return nil, err
	}
	if opts.MergeRequest {
		opts.Script, err = resolveMergeRequestHookScript(opts.ProjectRoot, opts.Path, opts.Script)
	} else {
		opts.Script, err = resolveHookScript(opts.ProjectRoot, opts.Script)
	}
	if err != nil {
		return nil, err
	}
	opts.Branch = strings.TrimSpace(opts.Branch)
	return &PreparedWorktreeHook{options: opts}, nil
}

// WorktreePath returns the destination validated with the script. Pass it to
// creation so script preparation and Git use the same resolved destination.
func (h *PreparedWorktreeHook) WorktreePath() string { return h.options.Path }

// Run executes the prepared script without taking a Git lock or rolling back.
func (h *PreparedWorktreeHook) Run(ctx context.Context) error {
	if h == nil {
		return errors.New("lifecycle hook was not prepared")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := h.options
	if opts.Script == "" {
		return nil
	}
	return executeLifecycleHook(ctx, opts.RunHook, opts.Script, opts.ProjectRoot, opts.Path,
		opts.Branch, opts.WorktreeName, opts.EnvironmentPrefix)
}
