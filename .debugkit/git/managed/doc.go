// Package managedworktree provides the named worktree lifecycle used by
// interactive developer tools.
//
// It creates branch-backed or detached worktrees, imports pull or merge request
// heads with push tracking, runs optional lifecycle hooks, detects dirty state,
// and removes worktrees and branches with classified errors.
//
// Creation can defer cleanup so an application can record identity and decide
// how to handle a partial acquisition. Rollback uses private directory and
// registration evidence; editing public result fields never grants ownership.
// The default rollback preserves local changes. Fresh-owned rollback may discard
// changes in the acquired checkout, but preserves an advanced owned branch.
// Missing evidence preserves artifacts and reports incomplete cleanup.
//
// Worktree-scoped tracking preserves compatible shared branch routing and
// rejects conflicts. Changing or clearing shared routing requires repository
// scope. Routing inherited from other files or command options is rejected;
// leaving tracking makes no writes. Explicit push defaults stay worktree-local
// even when branch routing uses repository scope.
//
// Removal distinguishes checkout, registration, and branch outcomes, including
// effects completed before a Git error. Branch cleanup starts only after the
// checkout and registration are gone. These operations take no repository lock;
// applications must serialize their own competing lifecycle calls.
// Prepared hooks let an application run scripts outside that lock.
package managedworktree
