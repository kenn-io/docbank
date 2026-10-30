package ingest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/jobs"
)

type watchObjectKey struct {
	mount watchMount
	file  uint64
}
type watchFileIdentity struct {
	object watchObjectKey
	birth  watchBirthStamp
}
type watchBirthStamp struct {
	seconds     int64
	nanoseconds int64
	present     bool
}
type watchFileState struct {
	fingerprint watchFingerprint
	mount       watchMount
	identity    watchFileIdentity
	info        fs.FileInfo
}
type watchAcceptedSource struct {
	nodeID   int64
	identity watchFileIdentity
	info     fs.FileInfo
}
type watchInventoryEntry struct {
	state      watchFileState
	observedAt time.Time
}
type watchInventory struct {
	files    map[string]watchInventoryEntry
	order    []string
	present  map[string]struct{}
	complete bool
	// blocked names known paths whose structure changed during this pass.
	blocked   []error
	known     map[string]struct{}
	knownDirs map[string]struct{}
}

func validWatchBirth(seconds, nanoseconds int64) watchBirthStamp {
	if seconds <= 0 || nanoseconds < 0 || nanoseconds >= 1e9 {
		return watchBirthStamp{}
	}
	return watchBirthStamp{seconds: seconds, nanoseconds: nanoseconds, present: true}
}
func matchableWatchIdentity(id watchFileIdentity) bool {
	return id.object.file != 0 && id.birth.present &&
		id.birth.seconds > 0 && id.birth.nanoseconds >= 0 && id.birth.nanoseconds < 1e9
}

type watchRenameCandidate struct{ oldRef, newRef string }

func matchWatchRenames(accepted map[string]watchObservation, inventory *watchInventory) []watchRenameCandidate {
	type oldEntry struct {
		ref    string
		source watchAcceptedSource
		count  int
	}
	type newEntry struct {
		ref   string
		state watchFileState
		count int
	}
	oldByKey := make(map[watchObjectKey]oldEntry)
	newByKey := make(map[watchObjectKey]newEntry)
	for ref, observation := range accepted {
		if observation.accepted == nil || observation.accepted.identity.object.file == 0 {
			continue
		}
		key := observation.accepted.identity.object
		value := oldByKey[key]
		value.ref, value.source, value.count = ref, *observation.accepted, value.count+1
		oldByKey[key] = value
	}
	for ref, entry := range inventory.files {
		key := entry.state.identity.object
		if key.file == 0 {
			continue
		}
		value := newByKey[key]
		value.ref, value.state, value.count = ref, entry.state, value.count+1
		newByKey[key] = value
	}
	candidates := make([]watchRenameCandidate, 0)
	for key, old := range oldByKey {
		current := newByKey[key]
		if old.count != 1 || current.count != 1 || current.ref == "" {
			continue
		}
		if _, exists := inventory.present[old.ref]; exists {
			continue
		}
		if observation, exists := accepted[current.ref]; exists && observation.accepted != nil {
			continue
		}
		if !matchableWatchIdentity(old.source.identity) || !matchableWatchIdentity(current.state.identity) ||
			old.source.identity != current.state.identity ||
			!os.SameFile(old.source.info, current.state.info) {
			continue
		}
		candidates = append(candidates, watchRenameCandidate{old.ref, current.ref})
	}
	return candidates
}

// possibleRenameTargets returns new paths that rename matching would pair with
// an accepted source given what this incomplete inventory saw. Importing one
// as a new document could split that source's history if the source was
// renamed. Paths matching would reject anyway import now.
func possibleRenameTargets(accepted map[string]watchObservation, inventory *watchInventory) map[string]struct{} {
	targets := make(map[string]struct{})
	for _, candidate := range matchWatchRenames(accepted, inventory) {
		targets[candidate.newRef] = struct{}{}
	}
	return targets
}

// watchOldRefAbsent proves that the old exact directory-entry spelling is gone.
// Opening the old pathname cannot make this distinction on case-insensitive
// filesystems: it can still resolve to the renamed entry.
func watchOldRefAbsent(root *watchRoot, ref string) bool {
	if err := root.verify(); err != nil {
		return false
	}
	absent, err := watchExactRefAbsent(root.root, strings.Split(ref, "/"))
	afterErr := root.verify()
	return err == nil && afterErr == nil && absent
}

// watchExactRefAbsent uses fresh directory streams so a prior inventory walk
// cannot affect the absence proof. Existing exact entries always occupy the
// old reference, even when another spelling names the same file.
func watchExactRefAbsent(dir *os.Root, components []string) (bool, error) {
	stream, err := dir.Open(".")
	if err != nil {
		return false, err
	}
	entries, readErr := stream.ReadDir(-1)
	closeErr := stream.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return false, err
	}
	name := components[0]
	found := false
	for _, entry := range entries {
		if entry.Name() == name {
			found = true
			break
		}
	}
	if !found {
		return true, nil
	}
	if len(components) == 1 {
		return false, nil
	}

	child, err := openWatchDirectory(dir, name)
	if err != nil {
		return false, err
	}
	absent, walkErr := watchExactRefAbsent(child, components[1:])
	parentInfo, parentErr := dir.Lstat(name)
	childInfo, childErr := child.Stat(".")
	closeErr = child.Close()
	if parentErr == nil && childErr == nil &&
		(parentInfo.Mode()&fs.ModeSymlink != 0 || !parentInfo.IsDir() ||
			!os.SameFile(parentInfo, childInfo)) {
		return false, errors.Join(walkErr, ErrSourceChanged, closeErr)
	}
	if err := errors.Join(walkErr, parentErr, childErr, closeErr); err != nil {
		return false, err
	}
	return absent, nil
}

func (w *Watcher) rebindCandidates(ctx context.Context, root *watchRoot, inventory *watchInventory) (map[string]struct{}, map[string]struct{}, error) {
	deferred := make(map[string]struct{})
	retained := make(map[string]struct{})
	for _, candidate := range matchWatchRenames(w.observations, inventory) {
		old := w.observations[candidate.oldRef]
		entry := inventory.files[candidate.newRef]
		source := watchSource{root: root, ref: candidate.newRef}
		if w.beforeRebind != nil {
			w.beforeRebind(candidate.newRef)
		}
		state, err := source.observe(false)
		if err != nil || state.identity != entry.state.identity ||
			!os.SameFile(state.info, entry.state.info) ||
			!watchOldRefAbsent(root, candidate.oldRef) {
			deferred[candidate.newRef] = struct{}{}
			retained[candidate.oldRef] = struct{}{}
			continue
		}
		stale := false
		jobs.ReportBusy(ctx, true)
		err = w.mutate(func() error {
			current, observeErr := source.observe(false)
			if observeErr != nil {
				stale = true
				return observeErr
			}
			if current.identity != entry.state.identity || !os.SameFile(current.info, entry.state.info) ||
				!watchOldRefAbsent(root, candidate.oldRef) {
				stale = true
				return ErrSourceChanged
			}
			_, err := w.ing.Store.RebindWatchedSource(ctx, w.config.Name, candidate.oldRef, candidate.newRef, old.accepted.nodeID)
			return err
		})
		jobs.ReportBusy(ctx, false)
		if stale {
			deferred[candidate.newRef] = struct{}{}
			retained[candidate.oldRef] = struct{}{}
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		old.fingerprint = entry.state.fingerprint
		old.stableSince = entry.observedAt
		old.processed = false
		w.observations[candidate.newRef] = old
		delete(w.observations, candidate.oldRef)
	}
	return deferred, retained, nil
}
