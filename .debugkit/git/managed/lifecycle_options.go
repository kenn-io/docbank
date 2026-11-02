package managedworktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/kit/fslink"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// CheckoutMode chooses whether creation may acquire a new branch.
type CheckoutMode uint8

const (
	CheckoutAuto CheckoutMode = iota // Attach an existing branch or create a missing one.
	CheckoutNewBranch
	CheckoutExistingBranch
	CheckoutDetached
)

// CheckoutPolicy chooses whether the checked-out tree may select Git programs.
type CheckoutPolicy uint8

const (
	CheckoutTrusted CheckoutPolicy = iota
	CheckoutIsolated
)

// FailureCleanup controls cleanup after an unsuccessful creation.
type FailureCleanup uint8

const (
	CleanupAutomatic FailureCleanup = iota
	CleanupDeferred
)

// UpstreamAction chooses the worktree's branch routing.
type UpstreamAction uint8

const (
	UpstreamDefault UpstreamAction = iota
	UpstreamTrack
	UpstreamLeave
	UpstreamClear
)

// TrackingCondition optionally requires the selected upstream tip to match HEAD.
type TrackingCondition uint8

const (
	TrackingExplicit TrackingCondition = iota
	TrackingIfHeadMatches
)

// UpstreamScope selects the Git configuration file used for branch routing.
type UpstreamScope uint8

const (
	UpstreamWorktree UpstreamScope = iota
	UpstreamRepository
)

// UpstreamPolicy selects tracking before any tracking mutation occurs.
// Ref is a full refs/heads/ name on Remote. Leave never changes tracking config.
// Worktree scope preserves compatible repository routing and rejects conflicts;
// Clear rejects inherited repository routing that worktree config cannot mask.
// Repository scope permits replacing routing in both writable scopes. Routing
// inherited from other configuration files is rejected rather than changed.
// Default preserves the entry point's ordinary tracking behavior.
type UpstreamPolicy struct {
	Action    UpstreamAction
	Condition TrackingCondition
	Scope     UpstreamScope
	Remote    string
	Ref       string
	// ConfigurePush also selects branch.pushRemote and worktree-local
	// push.default=upstream, regardless of the branch routing scope.
	// Leave and an unmatched head never change either fetch or push routing.
	ConfigurePush bool
}

var (
	ErrBranchNotFound         = errors.New("local branch not found")
	ErrInvalidWorktreeOptions = errors.New("invalid worktree options")
)

// WorktreeUpstreamOptions selects routing for a linked worktree in ProjectRoot.
type WorktreeUpstreamOptions struct {
	ProjectRoot string
	Path        string
	Policy      UpstreamPolicy
	Runner      gitcmd.Runner
	RunGit      GitRunner
}

// SetWorktreeUpstream updates branch routing in the selected configuration scope.
func SetWorktreeUpstream(ctx context.Context, opts WorktreeUpstreamOptions) error {
	execution := newLifecycleExecution(opts.Runner, opts.RunGit, nil)
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return err
	}
	path, err := absRequired(opts.Path, "worktree path")
	if err != nil {
		return err
	}
	return execution.setUpstream(ctx, root, path, opts.Policy)
}

func validateUpstreamPolicy(policy UpstreamPolicy) error {
	if policy.Action > UpstreamClear || policy.Condition > TrackingIfHeadMatches || policy.Scope > UpstreamRepository {
		return ErrInvalidWorktreeOptions
	}
	if policy.Action == UpstreamTrack && (strings.TrimSpace(policy.Remote) == "" ||
		!strings.HasPrefix(policy.Ref, "refs/heads/") || strings.TrimPrefix(policy.Ref, "refs/heads/") == "") {
		return fmt.Errorf("%w: tracking requires a remote and full branch ref", ErrInvalidWorktreeOptions)
	}
	return nil
}

func (e lifecycleExecution) setUpstream(ctx context.Context, root, path string, policy UpstreamPolicy) error {
	if err := validateUpstreamPolicy(policy); err != nil {
		return err
	}
	if policy.Action == UpstreamDefault || policy.Action == UpstreamLeave {
		return nil
	}
	rootCommon, err := e.run(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	pathCommon, err := e.run(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if comparableWorktreePath(strings.TrimSpace(string(rootCommon))) != comparableWorktreePath(strings.TrimSpace(string(pathCommon))) {
		return fmt.Errorf("%w: worktree belongs to another repository", ErrInvalidWorktreeOptions)
	}
	branchOut, err := e.run(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if gitcmd.IsExitCode(err, 1) && policy.Action == UpstreamClear {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve tracking branch: %w", err)
	}
	branch := strings.TrimSpace(string(branchOut))
	remote, ref := policy.Remote, policy.Ref
	if policy.Action == UpstreamTrack {
		if _, err := e.run(ctx, root, "check-ref-format", ref); err != nil {
			return err
		}
		if _, err := e.run(ctx, path, "remote", "get-url", "--", remote); err != nil {
			return err
		}
		if policy.Condition == TrackingIfHeadMatches {
			trackingOID, err := e.trackingOID(ctx, path, remote, ref)
			if err != nil {
				return err
			}
			headOID, err := e.run(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
			if err != nil {
				return err
			}
			if trackingOID != strings.TrimSpace(string(headOID)) {
				return nil
			}
		}
	} else {
		remote, ref = "", ""
	}
	entries := []gitcmd.Config{
		{Key: "branch." + branch + ".remote", Value: remote},
		{Key: "branch." + branch + ".merge", Value: ref},
	}
	if policy.ConfigurePush {
		entries = append(entries, gitcmd.Config{Key: "branch." + branch + ".pushRemote", Value: remote})
	}
	configurePush := policy.Action == UpstreamTrack && policy.ConfigurePush
	if configurePush {
		out, err := e.run(ctx, path, "config", "--show-scope", "--get", "push.default")
		if err != nil && !gitcmd.IsExitCode(err, 1) {
			return err
		}
		scope, value, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
		if scope == "command" && value != "upstream" {
			return fmt.Errorf("%w: command-scope push.default overrides upstream routing", ErrInvalidWorktreeOptions)
		}
	}
	registration, err := e.run(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if err := e.validateRoutingOrigins(ctx, path, strings.TrimSpace(string(rootCommon)), strings.TrimSpace(string(registration)), entries); err != nil {
		return err
	}
	inherited := make(map[string]bool)
	if policy.Scope == UpstreamWorktree {
		for _, entry := range entries {
			out, err := e.run(ctx, path, "config", "--local", "--null", "--get-all", entry.Key)
			if gitcmd.IsExitCode(err, 1) {
				continue
			}
			if err != nil {
				return err
			}
			for value := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
				if policy.Action == UpstreamClear || value != entry.Value {
					return fmt.Errorf("%w: %s requires repository scope to change inherited routing", ErrInvalidWorktreeOptions, entry.Key)
				}
			}
			inherited[entry.Key] = true
		}
	}
	enabled, configErr := e.run(ctx, path, "config", "--type=bool", "--get", "extensions.worktreeConfig")
	if configErr != nil && !gitcmd.IsExitCode(configErr, 1) {
		return configErr
	}
	worktreeConfigEnabled := strings.TrimSpace(string(enabled)) == "true"
	if policy.Scope == UpstreamWorktree || configurePush {
		// Shared core.bare/core.worktree require a caller-directed migration;
		// never change their meaning merely to configure tracking.
		checkCtx := withLifecycleExecution(ctx, e.runner, e.runGit, e.runHook)
		if err := validateWorktreeConfigCompatibility(checkCtx, root); err != nil {
			return err
		}
		if _, err := e.run(ctx, root, "config", "extensions.worktreeConfig", "true"); err != nil {
			return err
		}
		if !worktreeConfigEnabled {
			// Enabling the extension can expose a previously inactive config file.
			if err := e.validateRoutingOrigins(ctx, path, strings.TrimSpace(string(rootCommon)), strings.TrimSpace(string(registration)), entries); err != nil {
				return err
			}
		}
		worktreeConfigEnabled = true
	}
	scope := "--worktree"
	oldScopes := []string{"--worktree"}
	if policy.Scope == UpstreamRepository {
		scope = "--local"
		oldScopes = []string{"--local"}
		if worktreeConfigEnabled {
			oldScopes = append(oldScopes, "--worktree")
		}
	}
	for _, entry := range entries {
		// Git combines branch.merge across scopes. Preserve compatible shared
		// entries without duplicating them in worktree config.
		for _, oldScope := range oldScopes {
			_, err := e.run(ctx, path, "config", oldScope, "--unset-all", entry.Key)
			if err != nil && !gitcmd.IsExitCode(err, 5) {
				return err
			}
		}
		if policy.Action == UpstreamTrack && !inherited[entry.Key] {
			if _, err := e.run(ctx, path, "config", scope, "--replace-all", entry.Key, entry.Value); err != nil {
				return err
			}
		}
	}
	if configurePush {
		if _, err := e.run(ctx, path, "config", "--worktree", "--replace-all", "push.default", "upstream"); err != nil {
			return err
		}
		out, err := e.run(ctx, path, "config", "--get", "push.default")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "upstream" {
			return fmt.Errorf("%w: effective push.default overrides upstream routing", ErrInvalidWorktreeOptions)
		}
	}
	return nil
}

// trackingOID resolves a selected remote branch through its positive fetch
// mappings, without reading or changing the local branch's current upstream.
func (e lifecycleExecution) trackingOID(ctx context.Context, root, remote, ref string) (string, error) {
	out, err := e.run(ctx, root, "config", "--get-all", "remote."+remote+".fetch")
	if err != nil {
		return "", err
	}
	for spec := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		source, target, ok := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if !ok || target == "" {
			continue
		}
		if prefix, suffix, wildcard := strings.Cut(source, "*"); wildcard {
			if len(ref) < len(prefix)+len(suffix) || !strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, suffix) {
				continue
			}
			target = strings.Replace(target, "*", ref[len(prefix):len(ref)-len(suffix)], 1)
		} else if source != ref {
			continue
		}
		oid, err := e.run(ctx, root, "rev-parse", "--verify", "--end-of-options", target+"^{commit}")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(oid)), nil
	}
	return "", fmt.Errorf("%w: remote %s has no tracking ref for %s", ErrInvalidWorktreeOptions, remote, ref)
}

func (e lifecycleExecution) validateRoutingOrigins(ctx context.Context, path, common, registration string, entries []gitcmd.Config) error {
	localConfig := comparableWorktreePath(filepath.Join(common, "config"))
	worktreeConfig := comparableWorktreePath(filepath.Join(registration, "config.worktree"))
	for _, entry := range entries {
		out, err := e.run(ctx, path, "config", "--null", "--show-origin", "--get-all", entry.Key)
		if gitcmd.IsExitCode(err, 1) {
			continue
		}
		if err != nil {
			return err
		}
		fields := bytes.Split(out, []byte{0})
		for index := 0; index+1 < len(fields); index += 2 {
			origin, file := strings.CutPrefix(string(fields[index]), "file:")
			if !file {
				return fmt.Errorf("%w: %s is configured outside repository or worktree files", ErrInvalidWorktreeOptions, entry.Key)
			}
			if !filepath.IsAbs(origin) {
				origin = filepath.Join(path, origin)
			}
			origin = comparableWorktreePath(origin)
			if origin != localConfig && origin != worktreeConfig {
				return fmt.Errorf("%w: %s is configured outside repository or worktree files", ErrInvalidWorktreeOptions, entry.Key)
			}
		}
	}
	return nil
}

func (e lifecycleExecution) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return e.runWithRunner(ctx, e.runner, dir, args...)
}

func (e lifecycleExecution) runWithRunner(ctx context.Context, runner gitcmd.Runner, dir string, args ...string) ([]byte, error) {
	if e.runGit != nil {
		out, err := e.runGit(ctx, runner, dir, args...)
		if err != nil && ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return out, err
	}
	stdout, stderr, err := runner.Run(ctx, dir, nil, args...)
	return append(stdout, stderr...), err
}

func newLifecycleExecution(runner gitcmd.Runner, runGit GitRunner, runHook HookRunner) lifecycleExecution {
	if runner.Env == nil {
		isZero := len(runner.Config) == 0 && !runner.StripEnv && !runner.TerminalPrompt &&
			!runner.NullGlobalConfig && !runner.NoSystemConfig && !runner.DisableSafeDirectoryForward &&
			runner.WaitDelay == 0 && runner.StdoutLimit == 0 && runner.StderrLimit == 0 && !runner.AcceptSuccessfulWaitDelay
		runner.Env = os.Environ()
		if isZero {
			runner.StripEnv = true
		}
	}
	// Set the diagnostic locale before the first lifecycle command. The
	// caller may have already populated its cache under a different locale;
	// this environment needs its own cache, retained by nested operations.
	if len(runner.Env) == 0 || runner.Env[len(runner.Env)-1] != "LC_ALL=C" {
		runner = runner.WithEnv(slices.Concat(runner.Env, []string{"LC_ALL=C"}))
	}
	return lifecycleExecution{runner: runner, runGit: runGit, runHook: runHook}
}

func creationResult(ctx context.Context, root, path, branch string, created bool) CreateWorktreeResult {
	return CreateWorktreeResult{
		Path: path, Branch: branch, BranchCreated: created,
		projectRoot: root, ownedPath: path, ownedBranch: branch, ownedBranchCreated: created,
		runner: lifecycleRunner(ctx), runGit: lifecycleGitRunner(ctx), runHook: lifecycleHookRunner(ctx),
	}
}

// failedWorktreeAdd reports artifacts observed after an unsuccessful add.
// They have not been proven to belong to this call and cannot be rolled back.
func failedWorktreeAdd(ctx context.Context, root, path, branch string, created bool) CreateWorktreeResult {
	result := creationResult(ctx, root, path, branch, created)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		result.Path, result.ownedPath = "", ""
	}
	if created {
		if exists, err := localBranchExists(ctx, root, branch); err == nil && !exists {
			result.Branch, result.ownedBranch, result.BranchCreated, result.ownedBranchCreated = "", "", false, false
		}
	} else if result.Path == "" {
		result.Branch, result.ownedBranch = "", ""
	}
	return result
}

func (r CreateWorktreeResult) creationFailure(ctx context.Context, policy FailureCleanup, cause error) (CreateWorktreeResult, error) {
	if policy == CleanupDeferred {
		return r, cause
	}
	_, cleanupErr := r.Rollback(context.WithoutCancel(ctx), RollbackFreshOwned)
	return r, errors.Join(cause, cleanupErr)
}

func snapshotAcquisition(ctx context.Context, r CreateWorktreeResult) (CreateWorktreeResult, error) {
	e := newLifecycleExecution(r.runner, r.runGit, r.runHook)
	registration, err := e.run(ctx, r.ownedPath, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return r, err
	}
	r.registration = filepath.Clean(strings.TrimSpace(string(registration)))
	r.pathInfo, err = os.Lstat(r.ownedPath)
	if err != nil || !r.pathInfo.IsDir() {
		return r, fmt.Errorf("capture worktree directory: %w", errors.Join(err, ErrWorktreeCleanupIncomplete))
	}
	r.registrationInfo, err = os.Stat(r.registration)
	if err != nil {
		return r, err
	}
	// Windows Stat loads file identity lazily. Capture it now, while these
	// paths still name the acquired directories, instead of during rollback.
	if !os.SameFile(r.pathInfo, r.pathInfo) || !os.SameFile(r.registrationInfo, r.registrationInfo) {
		return r, fmt.Errorf("capture worktree directory identity: %w", ErrWorktreeCleanupIncomplete)
	}
	r.verified = true
	return r, nil
}

// RollbackPolicy selects which changes a caller authorizes rollback to discard.
type RollbackPolicy uint8

const (
	// RollbackUnchanged preserves changes, ignored files, initialized submodules,
	// advanced refs, and a changed HEAD. This is the default for completed creates.
	RollbackUnchanged RollbackPolicy = iota
	// RollbackFreshOwned may discard setup output and changes to HEAD, but still
	// requires the acquired directory and registration. It deletes only the
	// acquired branch, and only if that branch remains at its acquisition commit.
	RollbackFreshOwned
)

// BranchRemoval authorizes cleanup of one local branch after checkout removal.
// ExpectedOID, when set, must still match. Force selects -D instead of -d.
type BranchRemoval struct {
	Name        string
	ExpectedOID string
	Force       bool
}

func (r CreateWorktreeResult) remaining() RollbackResult {
	remaining := RollbackResult{Path: r.ownedPath, Registration: r.registration}
	if r.ownedBranchCreated {
		remaining.Branch = r.ownedBranch
	}
	return remaining
}

func (r CreateWorktreeResult) verifyAcquisition(ctx context.Context) error {
	if !r.verified || r.pathInfo == nil || r.registrationInfo == nil {
		return errors.New("creation evidence is incomplete")
	}
	info, err := os.Lstat(r.ownedPath)
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(r.pathInfo, info) {
		return errors.New("acquired worktree directory changed")
	}
	info, err = os.Lstat(r.registration)
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(r.registrationInfo, info) {
		return errors.New("acquired worktree registration changed")
	}
	registration, err := runLifecycleGit(ctx, r.ownedPath, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	if comparableWorktreePath(strings.TrimSpace(string(registration))) != comparableWorktreePath(r.registration) {
		return errors.New("worktree registration no longer matches acquisition")
	}
	_, headRef, err := lifecycleWorktreeHead(ctx, r.ownedPath)
	if err != nil {
		return err
	}
	return verifyRemovalTarget(ctx, r.projectRoot, r.ownedPath, strings.TrimPrefix(headRef, "refs/heads/"))
}

func prepareBranchRemovals(ctx context.Context, root, observedBranch string, opts RemoveWorktreeOptions) ([]BranchRemoval, error) {
	if opts.RemoveBranch && len(opts.Branches) != 0 {
		return nil, ErrInvalidWorktreeOptions
	}
	branches := append([]BranchRemoval(nil), opts.Branches...)
	if opts.RemoveBranch && observedBranch != "" {
		branches = []BranchRemoval{{Name: observedBranch, Force: true}}
	}
	seen := make(map[string]bool, len(branches))
	for index := range branches {
		branch := &branches[index]
		if err := validateBranchName(ctx, root, branch.Name); err != nil {
			return nil, err
		}
		if seen[branch.Name] {
			return nil, fmt.Errorf("%w: duplicate cleanup branch", ErrInvalidWorktreeOptions)
		}
		seen[branch.Name] = true
		if branch.ExpectedOID != "" {
			oid, err := resolveMergeRequestOID(ctx, root, branch.ExpectedOID)
			if err != nil {
				return nil, err
			}
			branch.ExpectedOID = oid
		}
	}
	return branches, nil
}

func removeBranch(ctx context.Context, root string, removal BranchRemoval) error {
	if removal.ExpectedOID != "" {
		oid, exists, err := lifecycleRefOID(ctx, root, removal.Name)
		if err != nil {
			return err
		}
		if !exists || !strings.EqualFold(oid, removal.ExpectedOID) {
			return fmt.Errorf("%w: branch %s changed", ErrWorktreeCleanupIncomplete, removal.Name)
		}
	}
	flag := "-d"
	if removal.Force {
		flag = "-D"
	}
	out, err := runLifecycleGit(ctx, root, "branch", flag, "--", removal.Name)
	if err != nil {
		return classifyWorktreeGitError(out, err)
	}
	return nil
}

func (r *RemoveWorktreeResult) setRemainingBranch() {
	r.Remaining.Branch = ""
	if len(r.BranchesRemaining) != 0 {
		r.Remaining.Branch = r.BranchesRemaining[0]
	}
}

func (r *RemoveWorktreeResult) inspectRemovedArtifacts(path, registration string) error {
	var errs []error
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		r.CheckoutRemoved, r.Remaining.Path = true, ""
	} else if err != nil {
		errs = append(errs, err)
	}
	if _, err := os.Lstat(registration); errors.Is(err, os.ErrNotExist) {
		r.RegistrationRemoved, r.Remaining.Registration = true, ""
	} else if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func removalRegistration(ctx context.Context, root, path string) (string, error) {
	common, err := lifecycleCommonGitDir(ctx, root)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !entry.IsDir() {
			continue
		}
		registration := filepath.Join(common, "worktrees", entry.Name())
		data, err := fslink.ReadFile(filepath.Join(registration, "gitdir"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(registration, target)
		}
		// Resolve the checkout path before appending .git: when the checkout
		// is gone, resolving its missing .git path cannot reach a linked parent.
		if filepath.Base(target) == ".git" &&
			comparableWorktreePath(filepath.Dir(target)) == comparableWorktreePath(path) {
			return registration, nil
		}
	}
	return "", fmt.Errorf("worktree registration not found: %s", path)
}
