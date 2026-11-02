package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"sort"
)

var (
	// ErrLastSnapshot means the selection would leave no recovery points.
	ErrLastSnapshot = errors.New("backup: refusing to forget the last snapshot")
	// ErrSnapshotRequired means a retained incremental snapshot needs a selected parent.
	ErrSnapshotRequired = errors.New("backup: snapshot required by a retained snapshot")
)

// ForgetOptions selects exact recovery points to remove. Retention policy
// (for example, keeping daily or monthly snapshots) belongs to the caller.
type ForgetOptions struct {
	SnapshotIDs []string // required; duplicates are ignored
	DryRun      bool     // validate and report the selection without deleting manifests
	AllowEmpty  bool     // explicitly allow deleting the last recovery point
	ForceUnlock bool     // only controls repository lock recovery, not AllowEmpty
	// Precondition, when set, runs while Forget holds the exclusive repository
	// lock and before it reads or removes any manifest. An error aborts the
	// forget with nothing removed. Callers use it to recheck state that the
	// lock protects, such as which application may manage the repository.
	Precondition func() error
}

// ForgetResult reports a dependency-safe removal order. Forgotten may be
// partial on error; a directory-sync error means its last removal may not be
// durable. Neither slice reports reclaimed storage: packs and indexes stay intact.
type ForgetResult struct {
	Selected  []string
	Forgotten []string
}

// Forget removes selected snapshot manifests under the exclusive repository
// lock. It rejects the entire selection if any retained SQLite incremental
// snapshot needs a selected parent. Children are removed and directory-synced
// before parents, so interruption does not break the remaining recovery points.
// Directory durability follows pack.SyncDir's platform contract.
//
// Forget does not erase backed-up bytes or reclaim pack space. The operation
// needs no source application, database, or content directory.
func Forget(ctx context.Context, r *Repo, opts ForgetOptions) (_ *ForgetResult, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(opts.SnapshotIDs) == 0 {
		return nil, errors.New("backup: forget requires snapshot IDs")
	}
	lock, err := r.AcquireExclusiveLockContext(ctx, "forget", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, lock.Release()) }()
	if opts.Precondition != nil {
		if err := opts.Precondition(); err != nil {
			return nil, fmt.Errorf("backup: forget precondition: %w", err)
		}
	}

	snapshots, err := r.openRepositoryDir(snapshotsDirName)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, snapshots.Close()) }()
	// Sync the directory we actually modify, not a path resolved after removal.
	dir, err := snapshots.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, dir.Close()) }()
	selected, err := planForget(ctx, snapshots.FS(), opts)
	if err != nil {
		return nil, err
	}
	result := &ForgetResult{Selected: selected}
	if opts.DryRun {
		return result, nil
	}
	for _, id := range selected {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := snapshots.Remove(id + manifestExt); err != nil {
			return result, fmt.Errorf("backup: forgetting snapshot %s: %w", id, err)
		}
		result.Forgotten = append(result.Forgotten, id)
		// As with pack.SyncDir, directory syncing is a no-op on Windows.
		if runtime.GOOS != "windows" {
			if err := dir.Sync(); err != nil {
				return result, fmt.Errorf("backup: snapshot %s removed but directory sync failed: %w", id, err)
			}
		}
	}
	return result, nil
}

// Match CleanStaging's boundary: refuse a symlink and retain the validated
// directory handle for enumeration, dependency reads, removal, and syncing.
func (r *Repo) openRepositoryDir(name string) (*os.Root, error) {
	path := r.Path(name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("backup: checking %s directory: %w", name, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("backup: %s must be a directory, not a symlink", name)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("backup: opening %s directory: %w", name, err)
	}
	held, err := root.Stat(".")
	if err != nil || !os.SameFile(info, held) {
		_ = root.Close()
		return nil, errors.Join(fmt.Errorf("backup: %s directory changed while opening it", name), err)
	}
	return root, nil
}

func planForget(ctx context.Context, snapshots fs.FS, opts ForgetOptions) ([]string, error) {
	manifests, err := listSnapshots(snapshots)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(manifests))
	for _, m := range manifests {
		existing[m.SnapshotID] = true
	}
	selected := make(map[string]bool, len(opts.SnapshotIDs))
	for _, id := range opts.SnapshotIDs {
		if !existing[id] {
			return nil, fmt.Errorf("backup: snapshot %q: %w", id, os.ErrNotExist)
		}
		selected[id] = true
	}
	if len(selected) == len(manifests) && !opts.AllowEmpty {
		return nil, ErrLastSnapshot
	}

	// Use the same dependency walk as restore/verify. Actual chain lengths,
	// not timestamps or the declared depth, put every child before its parent.
	depths := make(map[string]int, len(selected))
	var order []string
	for _, m := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chain, err := walkManifestChain(m, func(id string) (*Manifest, error) {
			return loadManifest(snapshots, id)
		})
		if err != nil {
			return nil, err
		}
		if selected[m.SnapshotID] {
			depths[m.SnapshotID] = len(chain)
			order = append(order, m.SnapshotID)
			continue
		}
		for _, dependency := range chain[1:] {
			if selected[dependency.SnapshotID] {
				return nil, fmt.Errorf("%w: %s is needed by %s", ErrSnapshotRequired, dependency.SnapshotID, m.SnapshotID)
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return depths[order[i]] > depths[order[j]] })
	return order, nil
}
