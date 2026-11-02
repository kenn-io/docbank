package managedworktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitworktree "go.kenn.io/kit/git/worktree"
	"go.kenn.io/kit/pathresolve"
)

// Sentinel errors for worktree lifecycle failures the HTTP layer maps to
// distinct problem codes. They are wrapped with operation detail; match
// with errors.Is.
var (
	// ErrWorktreeDestinationExists reports that the worktree target path
	// already exists on disk or is already used by another worktree.
	ErrWorktreeDestinationExists = errors.New(
		"worktree destination already exists",
	)
	// ErrBranchInUse reports that the branch is checked out in another
	// worktree, so it can be neither attached nor deleted.
	ErrBranchInUse = errors.New(
		"branch is checked out in another worktree",
	)
	// ErrBranchAlreadyExists reports that an operation requiring a new branch
	// was given the name of an existing local branch.
	ErrBranchAlreadyExists = errors.New("branch already exists")
	// ErrInvalidBranchName reports a branch name git rejects
	// (`git check-ref-format --branch`).
	ErrInvalidBranchName = errors.New("invalid branch name")
	// ErrHookOutsideProject reports a lifecycle hook script path that
	// resolves outside the project tree. Hooks are arbitrary executables;
	// confining them to the project keeps a registry entry from running
	// code elsewhere on the machine.
	ErrHookOutsideProject = errors.New(
		"lifecycle hook script resolves outside the project",
	)
	// ErrWorktreeCleanupIncomplete reports that rollback preserved a worktree
	// because it contains changes or its branch advanced after creation.
	ErrWorktreeCleanupIncomplete = errors.New("worktree cleanup incomplete")
)

// HookError reports a lifecycle hook script that ran and exited non-zero.
type HookError struct {
	Script   string
	ExitCode int
	Stderr   string
	Cause    error
}

func (e *HookError) Unwrap() error { return e.Cause }

// GitRunner runs one Git command under an application's process policy.
//
// It governs how Kit's own Git commands are executed, not which Git
// installation Kit targets. Merge-request import pins the git found on the
// process PATH into the replacement merge driver, because Git runs that driver
// itself and cannot route it back through this callback. A runner that
// executes some other Git therefore does not redirect the merge driver, and
// import fails outright when no git is on PATH.
type GitRunner func(
	ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
) ([]byte, error)

// HookCommand describes one lifecycle hook invocation.
type HookCommand struct {
	Script string
	Dir    string
	Env    []string
	Stdout io.Writer
	Stderr io.Writer
}

// HookRunner runs one lifecycle hook under an application's process policy.
type HookRunner func(context.Context, HookCommand) error

func (e *HookError) Error() string {
	return fmt.Sprintf(
		"%s failed with exit code %d: %s", e.Script, e.ExitCode, e.Stderr,
	)
}

const defaultHookEnvironmentPrefix = "KIT"

// CreateWorktreeOptions parameterizes CreateWorktreeOnDisk. ProjectRoot and
// Branch are required except for detached checkouts; everything else is optional. Lifecycle script paths
// arrive per call: the caller owns config sourcing (project files, app
// settings) and this package owns execution.
type CreateWorktreeOptions struct {
	// Mode defaults to the existing attach-or-create behavior. Explicit modes
	// never silently create an absent existing branch or reuse a new branch.
	Mode CheckoutMode
	// CheckoutIsolated suppresses programs selected by a checked-out tree.
	Checkout CheckoutPolicy
	// NoCheckout registers the worktree without materializing tracked files.
	NoCheckout bool
	// LockReason locks the new registration with this Git lock reason.
	LockReason     string
	Upstream       UpstreamPolicy
	FailureCleanup FailureCleanup
	// ProjectRoot is the repository checkout git commands run in.
	ProjectRoot string
	// Branch is the branch to attach or create.
	Branch string
	// Path is the worktree destination. When empty it derives from
	// BaseDir (default "<ProjectRoot>-worktrees") plus the slash-slugged
	// branch name.
	Path string
	// BaseDir overrides the derivation base used when Path is empty.
	BaseDir string
	// BaseRef, when set, forces creation of a new Branch starting at this
	// ref (git worktree add <path> -b <branch> -- <ref>). When empty, an
	// existing local Branch is attached and a missing one is created from
	// HEAD.
	BaseRef string
	// SetupScript, when set, runs in the new worktree after git work
	// succeeds. Relative paths resolve against ProjectRoot; the resolved
	// path must stay inside the project tree. A non-zero exit rolls the
	// worktree (and any branch this call created) back.
	SetupScript string
	// WorktreeName is the display name exported to hook scripts; defaults
	// to Branch.
	WorktreeName string
	// HookEnvironmentPrefix prefixes WORKTREE_NAME, WORKTREE_PATH,
	// PROJECT_ROOT, and BRANCH in the hook environment. It defaults to KIT.
	HookEnvironmentPrefix string
	// Runner overrides the Git execution policy. A zero runner preserves the
	// production lifecycle defaults.
	Runner gitcmd.Runner
	// RunGit and RunHook let an application retain its process limiter and
	// platform-specific command policy. Their zero values execute directly.
	RunGit  GitRunner
	RunHook HookRunner
}

// CreateWorktreeResult reports what CreateWorktreeOnDisk did.
type CreateWorktreeResult struct {
	Path   string
	Branch string
	// BranchCreated reports whether this call created the branch (as
	// opposed to attaching a pre-existing local branch). Rollback uses an
	// immutable private ownership snapshot rather than these report fields.
	BranchCreated      bool
	HookRan            bool
	HookScript         string
	projectRoot        string
	runner             gitcmd.Runner
	runGit             GitRunner
	runHook            HookRunner
	ownedPath          string
	ownedBranch        string
	ownedBranchCreated bool
	branchOID          string
	headOID            string
	headRef            string
	registration       string
	pathInfo           fs.FileInfo
	registrationInfo   fs.FileInfo
	verified           bool
	checkoutDeferred   bool
}

// RollbackResult identifies worktree artifacts that remained after rollback.
type RollbackResult struct {
	Path         string
	Branch       string
	Registration string
	// Unverified means acquisition evidence was absent or no longer matched.
	Unverified bool
}

// Rollback unwinds the worktree represented by this creation result.
func (r CreateWorktreeResult) Rollback(ctx context.Context, policies ...RollbackPolicy) (RollbackResult, error) {
	policy := RollbackUnchanged
	if len(policies) > 1 {
		return r.remaining(), ErrInvalidWorktreeOptions
	}
	if len(policies) == 1 {
		policy = policies[0]
	}
	if policy > RollbackFreshOwned {
		return r.remaining(), ErrInvalidWorktreeOptions
	}
	ctx = withLifecycleExecution(ctx, r.runner, r.runGit, r.runHook)
	return r.rollbackOwned(ctx, policy)
}

// CreateWorktreeOnDisk performs the git side of worktree creation: it
// derives and validates the destination, runs `git worktree add`
// (attaching an existing branch or creating a new one), and runs the
// optional setup hook. On hook failure the worktree — and the branch this
// call created — are rolled back so a retry does not trip
// ErrWorktreeDestinationExists.
func CreateWorktreeOnDisk(
	ctx context.Context, opts CreateWorktreeOptions,
) (CreateWorktreeResult, error) {
	return createWorktreeOnDisk(ctx, opts, nil, false)
}

func createWorktreeOnDisk(ctx context.Context, opts CreateWorktreeOptions, preparedIsolation *untrustedTreeIsolation, fetchUpstream bool) (CreateWorktreeResult, error) {
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, opts.RunHook)
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	branch := strings.TrimSpace(opts.Branch)
	if opts.Mode > CheckoutDetached || opts.Checkout > CheckoutIsolated || opts.FailureCleanup > CleanupDeferred {
		return CreateWorktreeResult{}, ErrInvalidWorktreeOptions
	}
	if err := validateUpstreamPolicy(opts.Upstream); err != nil {
		return CreateWorktreeResult{}, err
	}
	if opts.Mode == CheckoutDetached {
		if branch != "" || opts.Upstream.Action == UpstreamTrack {
			return CreateWorktreeResult{}, ErrInvalidWorktreeOptions
		}
		if strings.TrimSpace(opts.Path) == "" {
			return CreateWorktreeResult{}, errors.New("detached checkout requires a path")
		}
	} else if err := validateBranchName(ctx, root, branch); err != nil {
		return CreateWorktreeResult{}, err
	}
	hookScript, err := resolveHookScript(root, opts.SetupScript)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	path, err := resolveWorktreeDestination(root, branch, opts.Path, opts.BaseDir)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	branchExisted := false
	if branch != "" {
		branchExisted, err = localBranchExists(ctx, root, branch)
		if err != nil {
			return CreateWorktreeResult{}, err
		}
	}
	mode := opts.Mode
	if mode == CheckoutAuto {
		mode = CheckoutNewBranch
		if branchExisted && opts.BaseRef == "" {
			mode = CheckoutExistingBranch
		}
	}
	if mode == CheckoutNewBranch && branchExisted {
		return CreateWorktreeResult{}, fmt.Errorf("%w: %s", ErrBranchAlreadyExists, branch)
	}
	if mode == CheckoutExistingBranch && !branchExisted {
		return CreateWorktreeResult{}, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	}
	if mode == CheckoutExistingBranch && opts.BaseRef != "" {
		return CreateWorktreeResult{}, fmt.Errorf("%w: an existing branch cannot have a base override", ErrInvalidWorktreeOptions)
	}
	var isolation untrustedTreeIsolation
	if preparedIsolation != nil {
		isolation = *preparedIsolation
	} else if opts.Checkout == CheckoutIsolated {
		if err := validateWorktreeConfigCompatibility(ctx, root); err != nil {
			return CreateWorktreeResult{}, err
		}
		if err := validateUntrustedTreeCheckoutGitVersion(ctx, root); err != nil {
			return CreateWorktreeResult{}, err
		}
		isolation, err = prepareUntrustedTreeIsolation(ctx, root)
		if err != nil {
			return CreateWorktreeResult{}, err
		}
	}
	if opts.Checkout == CheckoutIsolated {
		if err := rejectConfigSourcesInsideWorktree(ctx, root, path, isolation.runner); err != nil {
			return CreateWorktreeResult{}, err
		}
	}
	startRef := opts.BaseRef
	if mode == CheckoutExistingBranch {
		startRef = "refs/heads/" + branch
	}
	if startRef == "" {
		startRef = "HEAD"
	}
	startOID, err := resolveMergeRequestOID(ctx, root, startRef)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	args := []string{"worktree", "add"}
	if opts.NoCheckout || opts.Checkout == CheckoutIsolated {
		args = append(args, "--no-checkout")
	}
	if opts.LockReason != "" {
		args = append(args, "--lock", "--reason", opts.LockReason)
	}
	if mode == CheckoutNewBranch && opts.Upstream.Action != UpstreamDefault {
		args = append(args, "--no-track")
	}
	if mode == CheckoutDetached {
		args = append(args, "--detach")
	}
	if mode == CheckoutNewBranch {
		args = append(args, "-b", branch)
	}
	args = append(args, path, "--", startRef)
	if mode == CheckoutExistingBranch {
		args[len(args)-1] = branch
	}
	addRunner := lifecycleRunner(ctx)
	if opts.Checkout == CheckoutIsolated {
		addRunner = isolation.runner
	}
	out, addErr := runLifecycleGitWithRunner(ctx, addRunner, root, args...)
	if addErr != nil && !gitcmd.IsExitCode(addErr, 0) {
		return failedWorktreeAdd(ctx, root, path, branch, mode == CheckoutNewBranch), classifyWorktreeGitError(out, addErr)
	}
	// Git has acquired the checkout. Caller cancellation must not prevent us
	// from recording the evidence needed to honor the cleanup policy.
	captureCtx, cancelCapture := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	evidenceCtx := withLifecycleExecution(captureCtx, addRunner, lifecycleGitRunner(ctx), lifecycleHookRunner(ctx))
	result, err := snapshotCreateWorktreeResult(evidenceCtx, root, path, branch, mode == CheckoutNewBranch)
	cancelCapture()
	cause := errors.Join(addErr, ctx.Err(), err)
	result.checkoutDeferred = opts.NoCheckout
	if err != nil {
		return result.creationFailure(ctx, opts.FailureCleanup, cause)
	}
	// Checkout hooks may move HEAD. Keep the acquired commit and branch as the
	// cleanup anchors, independently of later materialization and hooks.
	result.headOID, result.branchOID = startOID, startOID
	result.headRef = ""
	if branch != "" {
		result.headRef = "refs/heads/" + branch
	}
	if cause != nil {
		return result.creationFailure(ctx, opts.FailureCleanup, cause)
	}
	if opts.Checkout == CheckoutIsolated {
		// Registration makes the directory identity available for case aliases.
		// Check before materialization can activate any files from the tree.
		err = rejectConfigSourcesInsideWorktree(ctx, path, path, isolation.runner)
		if err == nil {
			err = rejectCommandScopeIsolationOverrides(ctx, path, lifecycleRunner(ctx))
		}
		if err == nil {
			var completed untrustedTreeIsolation
			completed, err = completeUntrustedTreeIsolation(ctx, path, isolation)
			if err == nil {
				isolation = completed
			}
		}
		if err == nil {
			err = persistUntrustedTreeIsolation(ctx, root, path, isolation)
		}
		if err == nil && !opts.NoCheckout {
			err = materializeUntrustedTree(ctx, path, isolation)
		}
		if err == nil {
			err = rejectConfigSourcesInsideWorktree(ctx, path, path, isolation.runner)
		}
		result.runner = isolation.runner
		if err != nil {
			return result.creationFailure(ctx, opts.FailureCleanup, err)
		}
	}
	execution := newLifecycleExecution(result.runner, result.runGit, result.runHook)
	if fetchUpstream && opts.Upstream.Action == UpstreamTrack && opts.Upstream.Condition == TrackingIfHeadMatches {
		// Imports compare a fresh tip using the destination's effective remote
		// configuration, which can depend on the new branch or git directory.
		// Run only after isolation is installed, including deferred checkouts.
		if _, err := execution.run(ctx, path, "fetch", "--no-tags", "--no-write-fetch-head",
			"--no-recurse-submodules", "--", opts.Upstream.Remote, opts.Upstream.Ref); err != nil {
			return result.creationFailure(ctx, opts.FailureCleanup, err)
		}
	}
	if err := execution.setUpstream(ctx, root, path, opts.Upstream); err != nil {
		return result.creationFailure(ctx, opts.FailureCleanup, err)
	}
	if hookScript != "" {
		if err := runLifecycleHook(ctx, hookScript, root, path, branch, opts.WorktreeName, opts.HookEnvironmentPrefix); err != nil {
			return result.creationFailure(ctx, opts.FailureCleanup, err)
		}
		result.HookRan, result.HookScript = true, hookScript
	}
	return result, nil
}

func snapshotCreateWorktreeResult(ctx context.Context, root, path, branch string, branchCreated bool) (CreateWorktreeResult, error) {
	result := creationResult(ctx, root, path, branch, branchCreated)
	if branch != "" {
		out, err := runLifecycleGit(ctx, root, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			return result, fmt.Errorf("resolve created worktree branch: %w", err)
		}
		result.branchOID = strings.TrimSpace(string(out))
	}
	headOID, headRef, err := lifecycleWorktreeHead(ctx, path)
	if err != nil {
		return result, err
	}
	result.headOID, result.headRef = headOID, headRef
	return snapshotAcquisition(ctx, result)
}

func (r CreateWorktreeResult) rollbackOwned(ctx context.Context, policy RollbackPolicy) (RollbackResult, error) {
	remaining := r.remaining()
	if err := r.verifyAcquisition(ctx); err != nil {
		remaining.Unverified = true
		return remaining, errors.Join(ErrWorktreeCleanupIncomplete, err)
	}
	headOID, headRef, err := lifecycleWorktreeHead(ctx, r.ownedPath)
	if err != nil {
		return remaining, errors.Join(ErrWorktreeCleanupIncomplete, err)
	}
	if policy == RollbackUnchanged {
		if !strings.EqualFold(headOID, r.headOID) || headRef != r.headRef {
			return remaining, fmt.Errorf("%w: worktree HEAD changed", ErrWorktreeCleanupIncomplete)
		}
		if r.ownedBranch != "" {
			oid, exists, err := lifecycleRefOID(ctx, r.projectRoot, r.ownedBranch)
			if err != nil || !exists || !strings.EqualFold(oid, r.branchOID) {
				return remaining, fmt.Errorf("created worktree branch changed: %w", errors.Join(ErrWorktreeCleanupIncomplete, err))
			}
		}
		dirty, err := r.hasRollbackArtifacts(ctx)
		if err != nil {
			return remaining, errors.Join(ErrWorktreeCleanupIncomplete, err)
		}
		if dirty {
			return remaining, fmt.Errorf("%w: created worktree contains changes", ErrWorktreeCleanupIncomplete)
		}
	}
	var branches []BranchRemoval
	if r.ownedBranchCreated {
		branches = []BranchRemoval{{Name: r.ownedBranch, ExpectedOID: r.branchOID, Force: true}}
	}
	result, err := RemoveWorktreeFromDisk(ctx, RemoveWorktreeOptions{
		ProjectRoot: r.projectRoot, Path: r.ownedPath, Branch: strings.TrimPrefix(headRef, "refs/heads/"),
		Force: true, Branches: branches, Runner: r.runner, RunGit: r.runGit,
	})
	if err != nil {
		err = errors.Join(ErrWorktreeCleanupIncomplete, err)
	}
	// Removal may fail before constructing its own report. Acquisition evidence
	// remains authoritative until an individual removal effect is confirmed.
	if result.CheckoutRemoved {
		remaining.Path = ""
	}
	if result.RegistrationRemoved {
		remaining.Registration = ""
	}
	if slices.Contains(result.BranchesRemoved, r.ownedBranch) {
		remaining.Branch = ""
	}
	return remaining, err
}

func (r CreateWorktreeResult) hasRollbackArtifacts(ctx context.Context) (bool, error) {
	if r.checkoutDeferred {
		// With --no-checkout Git leaves the index absent, so status describes
		// tracked files as deleted even though nothing has changed since create.
		absent, err := worktreeIndexAbsent(ctx, r.ownedPath)
		if err != nil {
			return false, err
		}
		if absent {
			entries, err := os.ReadDir(r.ownedPath)
			if err != nil {
				return false, fmt.Errorf("inspect deferred worktree contents: %w", err)
			}
			return len(entries) != 1 || entries[0].Name() != ".git", nil
		}
	}
	return worktreeHasRollbackArtifacts(ctx, r.ownedPath)
}

func worktreeIndexAbsent(ctx context.Context, path string) (bool, error) {
	out, err := runLifecycleGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return false, fmt.Errorf("locate deferred worktree index: %w", err)
	}
	_, err = os.Lstat(strings.TrimSpace(string(out)))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect deferred worktree index: %w", err)
	}
	return false, nil
}

func lifecycleRefOID(ctx context.Context, root, branch string) (string, bool, error) {
	out, err := runLifecycleGit(ctx, root, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", false, err
	}
	oid := strings.TrimSpace(string(out))
	return oid, oid != "", nil
}

func lifecycleWorktreeHead(
	ctx context.Context, path string,
) (oid string, symbolicRef string, err error) {
	out, err := runLifecycleGit(ctx, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", "", fmt.Errorf(
			"resolve worktree HEAD: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	oid = strings.TrimSpace(string(out))
	if refOut, refErr := runLifecycleGit(
		ctx, path, "symbolic-ref", "--quiet", "HEAD",
	); refErr == nil {
		symbolicRef = strings.TrimSpace(string(refOut))
	} else if !gitcmd.IsExitCode(refErr, 1) {
		return "", "", fmt.Errorf(
			"resolve worktree symbolic HEAD: %w: %s",
			refErr, strings.TrimSpace(string(refOut)),
		)
	}
	return oid, symbolicRef, nil
}

// RemoveWorktreeOptions parameterizes RemoveWorktreeFromDisk. ProjectRoot
// and Path are required.
type RemoveWorktreeOptions struct {
	ProjectRoot string
	Path        string
	// Branch is the branch the worktree must still have attached and is deleted
	// when RemoveBranch is set. An empty Branch requires a detached worktree and
	// makes RemoveBranch a no-op.
	Branch string
	// Force passes --force to git worktree remove so dirty or locked
	// worktrees still go. Policy checks (refusing dirty removal without
	// force) belong to the caller.
	Force        bool
	RemoveBranch bool
	// Branches lists refs authorized for cleanup independently of Branch.
	// It cannot be combined with RemoveBranch.
	Branches []BranchRemoval
	// TeardownScript, when set, runs in the worktree before removal.
	// Relative paths resolve against ProjectRoot and must stay inside the
	// project tree. A non-zero exit aborts the removal. The hook is
	// skipped when the worktree path is already gone.
	TeardownScript string
	// WorktreeName is the display name exported to hook scripts; defaults
	// to Branch.
	WorktreeName          string
	HookEnvironmentPrefix string
	Runner                gitcmd.Runner
	RunGit                GitRunner
	RunHook               HookRunner
}

// RemoveWorktreeResult reports what RemoveWorktreeFromDisk did.
type RemoveWorktreeResult struct {
	HookRan    bool
	HookScript string
	// These fields report absence after removal, including an already absent
	// checkout. An error may accompany either completed effect.
	CheckoutRemoved     bool
	RegistrationRemoved bool
	BranchesRemoved     []string
	BranchesRemaining   []string
	Remaining           RollbackResult
}

// RemoveWorktreeFromDisk performs the git side of worktree removal: it
// runs the optional teardown hook, removes the worktree (or prunes the
// stale registration when the path is already gone), and optionally
// deletes the branch.
func RemoveWorktreeFromDisk(
	ctx context.Context, opts RemoveWorktreeOptions,
) (RemoveWorktreeResult, error) {
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, opts.RunHook)
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	path, err := absRequired(opts.Path, "worktree path")
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	branch := strings.TrimSpace(opts.Branch)
	hookScript, err := resolveHookScript(root, opts.TeardownScript)
	if err != nil {
		return RemoveWorktreeResult{}, err
	}

	branches, err := prepareBranchRemovals(ctx, root, branch, opts)
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	info, statErr := os.Lstat(path)
	pathExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return RemoveWorktreeResult{}, fmt.Errorf("stat worktree path: %w", statErr)
	}
	if statErr == nil && !info.IsDir() {
		return RemoveWorktreeResult{}, fmt.Errorf("worktree path is not a directory: %s", path)
	}
	if pathExists {
		err = verifyRemovalTarget(ctx, root, path, branch)
	} else {
		err = verifyRegisteredRemovalTarget(ctx, root, path, branch)
	}
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	registration, err := removalRegistration(ctx, root, path)
	if err != nil {
		return RemoveWorktreeResult{}, err
	}
	result := RemoveWorktreeResult{Remaining: RollbackResult{Path: path, Registration: registration}}
	for _, removal := range branches {
		result.BranchesRemaining = append(result.BranchesRemaining, removal.Name)
	}
	result.setRemainingBranch()
	if !pathExists {
		result.CheckoutRemoved, result.Remaining.Path = true, ""
	}
	if hookScript != "" && pathExists {
		if hookErr := runLifecycleHook(ctx, hookScript, root, path, branch, opts.WorktreeName, opts.HookEnvironmentPrefix); hookErr != nil {
			return result, hookErr
		}
		result.HookRan, result.HookScript = true, hookScript
	}
	if pathExists {
		err = verifyRemovalTarget(ctx, root, path, branch)
	} else {
		err = verifyRegisteredRemovalTarget(ctx, root, path, branch)
	}
	if err != nil {
		return result, err
	}
	args := []string{"worktree", "remove"}
	if opts.Force {
		// Git requires force twice to remove a locked worktree.
		args = append(args, "--force", "--force")
	} else if !pathExists {
		args = append(args, "--force")
	}
	args = append(args, path)
	out, removeErr := runLifecycleGit(ctx, root, args...)
	inspectErr := result.inspectRemovedArtifacts(path, registration)
	if removeErr != nil || inspectErr != nil {
		return result, errors.Join(classifyWorktreeGitError(out, removeErr), inspectErr)
	}
	if !result.CheckoutRemoved || !result.RegistrationRemoved {
		return result, ErrWorktreeCleanupIncomplete
	}
	for _, removal := range branches {
		removeErr := removeBranch(ctx, root, removal)
		if removeErr != nil {
			// Cancellation may follow the deletion; inspect its effect without resuming cleanup.
			inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			exists, inspectErr := localBranchExists(inspectCtx, root, removal.Name)
			cancel()
			if inspectErr != nil || exists {
				return result, errors.Join(removeErr, inspectErr)
			}
		}
		result.BranchesRemoved = append(result.BranchesRemoved, removal.Name)
		result.BranchesRemaining = result.BranchesRemaining[1:]
		result.setRemainingBranch()
		if removeErr != nil {
			return result, removeErr
		}
	}
	return result, nil
}

// WorktreeIsDirty reports whether the worktree at path has uncommitted
// changes (staged, unstaged, or untracked).
func WorktreeIsDirty(ctx context.Context, path string) (bool, error) {
	out, err := runLifecycleGit(
		ctx, path, "status", "--porcelain", "--untracked-files=all",
		"--ignore-submodules=none",
	)
	if err != nil {
		return false, fmt.Errorf(
			"check worktree dirty state: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func worktreeHasRollbackArtifacts(ctx context.Context, path string) (bool, error) {
	out, err := runLifecycleGit(
		ctx, path, "status", "--porcelain", "--untracked-files=all",
		"--ignored=matching", "--ignore-submodules=none",
	)
	if err != nil {
		return false, fmt.Errorf(
			"check worktree rollback state: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	if strings.TrimSpace(string(out)) != "" {
		return true, nil
	}
	return worktreeHasInitializedSubmodules(ctx, path)
}

func worktreeHasInitializedSubmodules(
	ctx context.Context, path string,
) (bool, error) {
	out, err := runLifecycleGit(
		ctx, path, "submodule", "status", "--recursive",
	)
	if err != nil {
		return false, fmt.Errorf(
			"inspect initialized submodules before rollback: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	for line := range bytes.SplitSeq(out, []byte{'\n'}) {
		if len(line) != 0 && line[0] != '-' {
			return true, nil
		}
	}
	return false, nil
}

func verifyRemovalTarget(
	ctx context.Context, root, path, expectedBranch string,
) error {
	if err := verifyRemovalTargetRepository(ctx, root, path, expectedBranch); err != nil {
		return err
	}
	_, headRef, err := lifecycleWorktreeHead(ctx, path)
	if err != nil {
		return err
	}
	wantRef := ""
	if branch := strings.TrimSpace(expectedBranch); branch != "" {
		wantRef = "refs/heads/" + branch
	}
	if headRef != wantRef {
		return fmt.Errorf(
			"worktree branch changed: expected %q, found %q",
			wantRef, headRef,
		)
	}
	return nil
}

// verifyRemovalTargetRepository checks the registration and repository of a
// linked checkout. Callers that already read HEAD compare its branch themselves.
func verifyRemovalTargetRepository(ctx context.Context, root, path, expectedBranch string) error {
	if err := verifyRegisteredRemovalTarget(ctx, root, path, expectedBranch); err != nil {
		return err
	}
	rootCommon, err := lifecycleCommonGitDir(ctx, root)
	if err != nil {
		return err
	}
	pathCommon, err := lifecycleCommonGitDir(ctx, path)
	if err != nil {
		return fmt.Errorf("inspect worktree repository: %w", err)
	}
	if !pathsEqualForGOOS(rootCommon, pathCommon, runtime.GOOS) {
		return fmt.Errorf(
			"worktree path belongs to a different repository: %s", path,
		)
	}
	return nil
}

func pathsEqualForGOOS(left, right, goos string) bool {
	if goos == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func verifyRegisteredRemovalTarget(
	ctx context.Context, root, path, expectedBranch string,
) error {
	out, err := runLifecycleGit(
		ctx, root, "worktree", "list", "--porcelain",
	)
	if err != nil {
		return fmt.Errorf(
			"list registered worktrees: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	wantPath := comparableWorktreePath(path)
	wantBranch := strings.TrimSpace(expectedBranch)
	for index, entry := range gitworktree.ParsePorcelain(string(out)) {
		if comparableWorktreePath(entry.Path) != wantPath {
			continue
		}
		// Git documents the primary worktree as the first porcelain entry.
		// It is never a valid managed-worktree removal target.
		if index == 0 {
			return fmt.Errorf(
				"refusing to remove primary worktree: %s", path,
			)
		}
		if entry.Branch != wantBranch ||
			(wantBranch == "" && !entry.Detached) {
			return fmt.Errorf(
				"stale worktree registration branch changed: expected %q, found %q",
				wantBranch, entry.Branch,
			)
		}
		return nil
	}
	return fmt.Errorf("worktree registration not found: %s", path)
}

func comparableWorktreePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = filepath.Clean(path)
	}
	if resolved, resolveErr := pathresolve.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	} else if parent, parentErr := pathresolve.EvalSymlinks(
		filepath.Dir(absolute),
	); parentErr == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	absolute = filepath.Clean(absolute)
	if filepath.Separator == '\\' {
		absolute = strings.ToLower(absolute)
	}
	return absolute
}

func lifecycleCommonGitDir(ctx context.Context, path string) (string, error) {
	out, err := runLifecycleGit(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf(
			"resolve common Git directory: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	common, err = filepath.Abs(common)
	if err != nil {
		return "", fmt.Errorf("resolve common Git directory path: %w", err)
	}
	if resolved, resolveErr := pathresolve.EvalSymlinks(common); resolveErr == nil {
		common = resolved
	}
	return filepath.Clean(common), nil
}

func absRequired(raw, label string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	return abs, nil
}

func validateBranchName(
	ctx context.Context, root, branch string,
) error {
	out, err := runLifecycleGit(
		ctx, root, "check-ref-format", "--branch", branch,
	)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if gitcmd.IsExitCode(err, 128) {
		return fmt.Errorf("%w: %q", ErrInvalidBranchName, branch)
	}
	return fmt.Errorf(
		"validate branch name %q: %w: %s",
		branch, err, strings.TrimSpace(string(out)),
	)
}

// resolveHookScript resolves a caller-supplied hook script path against the
// project root and rejects paths that escape it. Both sides of the
// containment check are canonicalized through symlink resolution so a
// symlink inside the project cannot smuggle in a script that lives outside
// it. An empty raw path means no hook and resolves to "".
func resolveHookScript(projectRoot, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	resolved := trimmed
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(projectRoot, resolved)
	}
	resolved = filepath.Clean(resolved)
	if !pathWithinRoot(canonicalizePath(projectRoot), canonicalizePath(resolved)) {
		return "", fmt.Errorf("%w: %q", ErrHookOutsideProject, raw)
	}
	return resolved, nil
}

// resolveMergeRequestHookScript accepts only a caller-trusted script that
// already exists outside the not-yet-created merge-request worktree.
func resolveMergeRequestHookScript(
	projectRoot, worktreePath, raw string,
) (string, error) {
	script, err := resolveHookScript(projectRoot, raw)
	if err != nil || script == "" {
		return script, err
	}
	resolved, err := pathresolve.EvalSymlinks(script)
	if err != nil {
		return "", fmt.Errorf(
			"merge request setup hook must already exist: %w", err,
		)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf(
			"inspect merge request setup hook: %w", err,
		)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("merge request setup hook must be a regular file")
	}
	if pathWithinRoot(
		comparableWorktreePath(worktreePath),
		comparableWorktreePath(resolved),
	) {
		return "", errors.New("merge request setup hook must be outside its worktree destination")
	}
	return resolved, nil
}

// canonicalizePath resolves symlinks and junctions in the deepest existing
// parent of path and re-appends the part that does not exist yet. A missing
// hook then compares in the same spelling as its resolved project root and
// fails later at execution time rather than here. A path that cannot be
// resolved for any other reason keeps its lexical form.
func canonicalizePath(path string) string {
	current := filepath.Clean(path)
	missing := ""
	for {
		resolved, err := pathresolve.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, missing)
		}
		parent := filepath.Dir(current)
		if !errors.Is(err, fs.ErrNotExist) || parent == current {
			return path
		}
		missing = filepath.Join(filepath.Base(current), missing)
		current = parent
	}
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveWorktreeDestination returns the validated absolute worktree
// destination. An explicit path wins; otherwise the destination derives
// from baseDir (default "<root>-worktrees") plus the slash-slugged branch.
// The destination must not already exist.
func resolveWorktreeDestination(
	root, branch, explicitPath, baseDir string,
) (string, error) {
	var dest string
	if strings.TrimSpace(explicitPath) != "" {
		abs, err := filepath.Abs(strings.TrimSpace(explicitPath))
		if err != nil {
			return "", fmt.Errorf("resolve worktree path: %w", err)
		}
		dest = abs
	} else {
		base := strings.TrimSpace(baseDir)
		if base == "" {
			base = root + "-worktrees"
		}
		if err := os.MkdirAll(base, 0o755); err != nil {
			return "", fmt.Errorf("create worktree base dir: %w", err)
		}
		// Canonicalize the base so derived paths agree with what git
		// and discovery report (macOS /tmp vs /private/tmp).
		if resolved, err := pathresolve.EvalSymlinks(base); err == nil {
			base = resolved
		}
		slug := strings.ReplaceAll(branch, "/", "-")
		abs, err := filepath.Abs(filepath.Join(base, slug))
		if err != nil {
			return "", fmt.Errorf("resolve worktree path: %w", err)
		}
		dest = abs
	}
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf(
			"%w: %s", ErrWorktreeDestinationExists, dest,
		)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat worktree destination: %w", err)
	}
	return dest, nil
}

func localBranchExists(
	ctx context.Context, root, branch string,
) (bool, error) {
	out, err := runLifecycleGit(
		ctx, root, "show-ref", "--verify", "--quiet",
		"refs/heads/"+branch,
	)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if gitcmd.IsExitCode(err, 1) {
		return false, nil
	}
	return false, fmt.Errorf(
		"inspect local branch %q: %w: %s",
		branch, err, strings.TrimSpace(string(out)),
	)
}

func runLifecycleGit(
	ctx context.Context, dir string, args ...string,
) ([]byte, error) {
	return runLifecycleGitWithRunner(ctx, lifecycleRunner(ctx), dir, args...)
}

func runLifecycleGitWithRunner(
	ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
) ([]byte, error) {
	execution := newLifecycleExecution(runner, lifecycleGitRunner(ctx), nil)
	return execution.run(ctx, dir, args...)
}

type lifecycleExecutionContextKey struct{}

type lifecycleExecution struct {
	runner  gitcmd.Runner
	runGit  GitRunner
	runHook HookRunner
}

func withLifecycleExecution(
	ctx context.Context, runner gitcmd.Runner, runGit GitRunner, runHook HookRunner,
) context.Context {
	return context.WithValue(ctx, lifecycleExecutionContextKey{}, newLifecycleExecution(runner, runGit, runHook))
}

func lifecycleRunner(ctx context.Context) gitcmd.Runner {
	if execution, ok := ctx.Value(lifecycleExecutionContextKey{}).(lifecycleExecution); ok && execution.runner.Env != nil {
		return execution.runner
	}
	return gitcmd.Runner{Env: os.Environ(), StripEnv: true}
}

func lifecycleGitRunner(ctx context.Context) GitRunner {
	execution, _ := ctx.Value(lifecycleExecutionContextKey{}).(lifecycleExecution)
	return execution.runGit
}

func lifecycleHookRunner(ctx context.Context) HookRunner {
	execution, _ := ctx.Value(lifecycleExecutionContextKey{}).(lifecycleExecution)
	return execution.runHook
}

// classifyWorktreeGitError maps well-known git stderr phrases onto the
// package sentinels so the HTTP layer can answer with distinct problem
// codes instead of a generic failure.
func classifyWorktreeGitError(out []byte, err error) error {
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	switch {
	// "'X' is already checked out at ..." (older git, worktree add),
	// "'X' is already used by worktree at ..." (worktree add),
	// "cannot delete branch 'X' used by worktree at ..." (branch -D).
	case strings.Contains(detail, "is already checked out at"),
		strings.Contains(detail, "used by worktree at"):
		return fmt.Errorf("%w: %w: %s", ErrBranchInUse, err, detail)
	case strings.Contains(detail, "a branch named") &&
		strings.Contains(detail, "already exists"):
		return fmt.Errorf("%w: %w: %s", ErrBranchAlreadyExists, err, detail)
	case strings.Contains(detail, "already exists"):
		return fmt.Errorf("%w: %w: %s", ErrWorktreeDestinationExists, err, detail)
	}
	return fmt.Errorf("git: %w: %s", err, detail)
}

// runLifecycleHook executes a hook script in the worktree directory with the
// lifecycle environment while honoring ctx cancellation. Stdout is discarded;
// stderr is captured into the HookError a non-zero exit produces.
func runLifecycleHook(
	ctx context.Context,
	script, projectRoot, worktreePath, branch, worktreeName,
	environmentPrefix string,
) error {
	return executeLifecycleHook(ctx, lifecycleHookRunner(ctx), script, projectRoot, worktreePath, branch, worktreeName, environmentPrefix)
}

func executeLifecycleHook(
	ctx context.Context, run HookRunner,
	script, projectRoot, worktreePath, branch, worktreeName, environmentPrefix string,
) error {
	name := strings.TrimSpace(worktreeName)
	if name == "" {
		name = branch
	}
	prefix := strings.TrimSpace(environmentPrefix)
	if prefix == "" {
		prefix = defaultHookEnvironmentPrefix
	}
	environment := append(
		withoutGitRepositoryBindings(os.Environ()),
		prefix+"_WORKTREE_NAME="+name,
		prefix+"_WORKTREE_PATH="+worktreePath,
		prefix+"_PROJECT_ROOT="+projectRoot,
		prefix+"_BRANCH="+branch,
	)
	var stderr bytes.Buffer
	command := HookCommand{
		Script: script, Dir: worktreePath, Env: environment,
		Stdout: io.Discard, Stderr: &stderr,
	}
	var err error
	if run != nil {
		err = run(ctx, command)
	} else {
		cmd := exec.CommandContext(ctx, command.Script)
		cmd.Dir = command.Dir
		cmd.Env = command.Env
		cmd.Stdout = command.Stdout
		cmd.Stderr = command.Stderr
		err = cmd.Run()
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("run lifecycle hook %s: %w", script, errors.Join(ctxErr, err))
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return &HookError{
				Script:   script,
				ExitCode: exitErr.ExitCode(),
				Stderr:   strings.TrimSpace(stderr.String()),
				Cause:    err,
			}
		}
		return fmt.Errorf("run lifecycle hook %s: %w", script, err)
	}
	return nil
}
