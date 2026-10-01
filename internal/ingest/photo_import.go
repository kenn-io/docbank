package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

type PhotoImportCandidate struct {
	Path string
	Kind store.PhotoSourceKind
}

type PhotoImportGroup struct {
	Members []PhotoImportCandidate
}

type PhotoImportOptions struct {
	SettleInterval time.Duration
	Mutate         func(context.Context, func() error) error
	ActivityBegin  func()
	ActivityEnd    func()
	// Cancelled reports operator cancellation during preparation and group admission.
	Cancelled func(context.Context) (bool, error)
	// Progress runs once with done=0 after discovery and after every group.
	Progress func(ctx context.Context, done, total int, receipt store.PhotoImportReceipt) error
}

// DefaultPhotoImportSettleInterval is how long every discovered file must stay
// unchanged before the import reads it.
const DefaultPhotoImportSettleInterval = time.Second

const photoImportRetention = 30 * 24 * time.Hour

type PhotoImportReport struct {
	Receipt   store.PhotoImportReceipt
	Total     int
	Cancelled bool
	Errors    []FileError
}

// GroupPhotoCandidates returns deterministic same-folder, same-stem groups.
// A video always keeps its own group.
func GroupPhotoCandidates(candidates []PhotoImportCandidate) []PhotoImportGroup {
	grouped := make(map[string]*PhotoImportGroup)
	for _, candidate := range candidates {
		key := store.PhotoImportGroupKey(candidate.Path, candidate.Kind)
		group := grouped[key]
		if group == nil {
			group = &PhotoImportGroup{}
			grouped[key] = group
		}
		group.Members = append(group.Members, candidate)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	groups := make([]PhotoImportGroup, 0, len(keys))
	for _, key := range keys {
		group := *grouped[key]
		sort.Slice(group.Members, func(i, j int) bool { return group.Members[i].Path < group.Members[j].Path })
		groups = append(groups, group)
	}
	return groups
}

func discoverPhotoCandidates(ctx context.Context, root string) ([]PhotoImportCandidate, int64, error) {
	return discoverPhotoCandidatesChecked(root, ctx.Err)
}

// discoverPhotoCandidatesChecked returns the supported camera files under
// root and how many other regular files it skipped.
func discoverPhotoCandidatesChecked(root string, check func() error) ([]PhotoImportCandidate, int64, error) {
	if root == "" {
		return nil, 0, errors.New("photo import root is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Lstat(absRoot)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		absRoot, err = filepath.EvalSymlinks(absRoot)
		if err != nil {
			return nil, 0, err
		}
	}
	var candidates []PhotoImportCandidate
	var unsupported int64
	err = filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := check(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		source := store.ClassifyPhotoSource(entry.Name())
		if source.Kind == store.PhotoSourceUnsupported {
			unsupported++
			return nil
		}
		candidates = append(candidates, PhotoImportCandidate{Path: path, Kind: source.Kind})
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("discover photo import sources: %w", err)
	}
	return candidates, unsupported, nil
}

type photoImportObservation struct {
	fingerprint localFileFingerprint
	hash        string
}

func observePhotoImportSource(ctx context.Context, path string) (observation photoImportObservation, retErr error) {
	return observePhotoImportSourceChecked(path, ctx.Err)
}

func observePhotoImportSourceChecked(path string, check func() error) (observation photoImportObservation, retErr error) {
	f, err := blob.OpenNoFollow(path)
	if err != nil {
		return observation, photoSourceGone(path, err)
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return observation, err
	}
	if !info.Mode().IsRegular() {
		return observation, errors.New("not a regular file")
	}
	observation.fingerprint = fingerprintFileInfo(info)
	digest := sha256.New()
	buffer := make([]byte, 64*1024)
	var size int64
	for {
		if err := check(); err != nil {
			return observation, err
		}
		n, readErr := f.Read(buffer)
		_, _ = digest.Write(buffer[:n])
		size += int64(n)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return observation, readErr
		}
	}
	after, err := f.Stat()
	if err != nil {
		return observation, err
	}
	current, err := blob.OpenNoFollow(path)
	if err != nil {
		return observation, photoSourceGone(path, err)
	}
	currentInfo, statErr := current.Stat()
	if err := errors.Join(statErr, current.Close()); err != nil {
		return observation, err
	}
	if size != observation.fingerprint.size || !observation.fingerprint.matches(fingerprintFileInfo(after)) || !currentInfo.Mode().IsRegular() || !observation.fingerprint.matches(fingerprintFileInfo(currentInfo)) {
		return observation, ErrSourceChanged
	}
	observation.hash = hex.EncodeToString(digest.Sum(nil))
	return observation, nil
}

func (ing *Ingester) readPhotoImportMember(ctx context.Context, candidate PhotoImportCandidate, observation photoImportObservation) (store.PhotoImportMember, error) {
	content, err := ing.readLocalFile(ctx, candidate.Path, candidate.Path, nil, &observation.fingerprint)
	if err != nil {
		return store.PhotoImportMember{}, photoSourceGone(candidate.Path, err)
	}
	if content.hash != observation.hash || content.size != observation.fingerprint.size {
		return store.PhotoImportMember{}, errors.Join(ErrSourceChanged, ing.cleanupLoose(content.hash))
	}
	source := store.ClassifyPhotoSource(candidate.Path)
	return store.PhotoImportMember{
		Name: filepath.Base(candidate.Path), Role: source.Role,
		BlobHash: content.hash, Size: content.size, MediaType: source.MediaType,
		OriginalPath: candidate.Path, OriginalMtime: content.mtime, Physical: content.physical,
	}, nil
}

// photoSourceGone counts a file deleted since the scan as changed, like any
// other change since the scan; other read failures stay failures.
func photoSourceGone(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Lstat(path); errors.Is(statErr, fs.ErrNotExist) {
			return errors.Join(ErrSourceChanged, err)
		}
	}
	return err
}

// ImportPhotoDirectory imports every supported camera file under root into
// destination, one group transaction at a time. Files settle once for the
// whole scan outside the mutation gate; each group holds the gate only while
// it rechecks, publishes bytes, and commits. A group whose files changed
// since the scan counts as changed.
func (ing *Ingester) ImportPhotoDirectory(ctx context.Context, root, destination string, opts PhotoImportOptions) (report PhotoImportReport, retErr error) {
	operatorCancelled := errors.New("photo import cancelled by operator")
	var nextCancelCheck time.Time
	var cancelCheckErr error
	check := func(force bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if report.Cancelled {
			return operatorCancelled
		}
		if opts.Cancelled == nil || (!force && time.Now().Before(nextCancelCheck)) {
			return nil
		}
		cancelled, err := opts.Cancelled(ctx)
		nextCancelCheck = time.Now().Add(100 * time.Millisecond)
		if err != nil {
			cancelCheckErr = err
			return err
		}
		if cancelled {
			report.Cancelled = true
			return operatorCancelled
		}
		return nil
	}
	defer func() {
		if errors.Is(retErr, operatorCancelled) {
			retErr = ctx.Err()
		}
	}()
	checkPreparation := func() error { return check(false) }
	mutate := func(fn func() error) error {
		if opts.Mutate != nil {
			return opts.Mutate(ctx, fn)
		}
		return fn()
	}
	progress := func(done int) error {
		if opts.Progress == nil {
			return nil
		}
		return opts.Progress(ctx, done, report.Total, report.Receipt)
	}
	if opts.ActivityBegin != nil {
		opts.ActivityBegin()
		if opts.ActivityEnd != nil {
			defer opts.ActivityEnd()
		}
	}
	if err := check(true); err != nil {
		return report, err
	}
	candidates, unsupported, err := discoverPhotoCandidatesChecked(root, checkPreparation)
	if err != nil {
		return report, err
	}
	report.Receipt.Unsupported = unsupported
	groups := GroupPhotoCandidates(candidates)
	report.Total = len(groups)
	if destination == "" {
		destination = "/"
	}
	var dest store.Node
	var run store.IngestRun
	err = mutate(func() error {
		if err := check(true); err != nil {
			return err
		}
		var err error
		if dest, err = ing.Store.EnsurePhotoImportDestination(ctx, destination); err != nil {
			return fmt.Errorf("resolving photo import destination: %w", err)
		}
		run, err = ing.Store.BeginIngest(ctx, "photo-import", root)
		return err
	})
	if err != nil {
		return report, err
	}
	if err := progress(0); err != nil {
		return report, err
	}
	observations := make(map[string]photoImportObservation, len(candidates))
	observeErrors := make(map[string]error)
	for _, candidate := range candidates {
		if err := checkPreparation(); err != nil {
			return report, err
		}
		observation, err := observePhotoImportSourceChecked(candidate.Path, checkPreparation)
		if err != nil {
			if cancelCheckErr != nil || report.Cancelled || ctx.Err() != nil {
				return report, err
			}
			observeErrors[candidate.Path] = fmt.Errorf("observing %s: %w", candidate.Path, err)
			continue
		}
		observations[candidate.Path] = observation
	}
	if opts.SettleInterval > 0 && len(candidates) > 0 {
		if err := checkPreparation(); err != nil {
			return report, err
		}
		if err := func() error {
			timer := time.NewTimer(opts.SettleInterval)
			defer timer.Stop()
			var ticks <-chan time.Time
			if opts.Cancelled != nil {
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				ticks = ticker.C
			}
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return nil
				case <-ticks:
					if err := checkPreparation(); err != nil {
						return err
					}
				}
			}
		}(); err != nil {
			return report, err
		}
	}
	report.Errors = make([]FileError, 0)
	for index, group := range groups {
		if err := check(true); err != nil {
			return report, err
		}
		result, groupErr := ing.importPhotoGroup(ctx, run, dest.ID, group, observations, observeErrors, mutate, func() error {
			return check(true)
		})
		switch {
		case groupErr != nil && ctx.Err() != nil:
			return report, ctx.Err()
		case errors.Is(groupErr, operatorCancelled):
			return report, nil
		case groupErr != nil && cancelCheckErr != nil:
			return report, groupErr
		case errors.Is(groupErr, ErrSourceChanged):
			// A file that changed since the scan waits for the next run.
			report.Receipt.Changed++
		case groupErr != nil:
			report.Receipt.Failed++
			report.Errors = append(report.Errors, FileError{Path: group.Members[0].Path, Err: groupErr})
		case result.Ambiguity != nil:
			report.Receipt.Ambiguous++
			report.Receipt.Ambiguities = append(report.Receipt.Ambiguities, *result.Ambiguity)
		case result.Added:
			report.Receipt.Added++
		default:
			report.Receipt.Skipped++
		}
		if err := progress(index + 1); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (ing *Ingester) importPhotoGroup(
	ctx context.Context, run store.IngestRun, destinationID int64, group PhotoImportGroup,
	observations map[string]photoImportObservation, observeErrors map[string]error, mutate func(func() error) error,
	checkAdmission func() error,
) (store.PhotoImportResult, error) {
	for _, candidate := range group.Members {
		if err := observeErrors[candidate.Path]; err != nil {
			return store.PhotoImportResult{}, photoSourceGone(candidate.Path, err)
		}
	}
	var result store.PhotoImportResult
	err := mutate(func() error {
		return ing.Blobs.WithMutation(ctx, func() (retErr error) {
			if err := checkAdmission(); err != nil {
				return err
			}
			members := make([]store.PhotoImportMember, 0, len(group.Members))
			defer func() {
				for _, member := range members {
					retErr = mutationCleanupResult(retErr, ing.cleanupLoose(member.BlobHash))
				}
			}()
			for _, candidate := range group.Members {
				member, err := ing.readPhotoImportMember(ctx, candidate, observations[candidate.Path])
				if err != nil {
					return err
				}
				members = append(members, member)
			}
			for i, candidate := range group.Members {
				fresh, err := observePhotoImportSource(ctx, candidate.Path)
				if err != nil {
					return err
				}
				initial := observations[candidate.Path]
				if !fresh.fingerprint.matches(initial.fingerprint) || fresh.hash != initial.hash || fresh.hash != members[i].BlobHash || fresh.fingerprint.size != members[i].Size {
					return ErrSourceChanged
				}
			}
			var err error
			result, err = ing.Store.IngestPhotoGroup(ctx, run, store.PhotoImportGroup{
				DestinationID: destinationID, Members: members,
			})
			return err
		})
	})
	return result, err
}

// PhotoImportRunner runs photo_import operations as supervised jobs, sharing
// the storage operation lifecycle: claim, progress, cancel, finish, resume.
type PhotoImportRunner struct {
	Ingester *Ingester
	Options  PhotoImportOptions
}

func (r PhotoImportRunner) Start(supervisor *jobs.Supervisor, operationID string) error {
	if supervisor == nil {
		return errors.New("photo import runner requires a job supervisor")
	}
	return supervisor.Start("storage:"+operationID, func(ctx context.Context) error {
		return r.Run(ctx, operationID)
	})
}

// Resume restarts photo imports a stopped daemon left queued or running.
// A rerun skips content already imported.
func (r PhotoImportRunner) Resume(ctx context.Context, supervisor *jobs.Supervisor) error {
	operations, err := r.Ingester.Store.ResumableStorageOperations(ctx)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if operation.Kind != store.StorageOperationKindPhotoImport {
			continue
		}
		if err := r.Start(supervisor, operation.ID); err != nil && !errors.Is(err, jobs.ErrDuplicate) {
			return fmt.Errorf("resuming photo import %s: %w", operation.ID, err)
		}
	}
	return nil
}

func (r PhotoImportRunner) Run(ctx context.Context, operationID string) error {
	metadata := r.Ingester.Store
	operation, err := metadata.ClaimStorageOperation(ctx, operationID)
	if err != nil {
		return err
	}
	opts := r.Options
	opts.Cancelled = func(ctx context.Context) (bool, error) {
		current, err := metadata.StorageOperation(ctx, operationID)
		return current.CancelRequested, err
	}
	opts.Progress = func(ctx context.Context, done, total int, receipt store.PhotoImportReceipt) error {
		if done == 0 {
			return metadata.SetStorageOperationTotal(ctx, operationID, int64(total))
		}
		receipt.Ambiguities = nil
		encoded, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		return metadata.AdvanceStorageOperation(ctx, operationID, "", int64(done), 0, 0, string(encoded))
	}
	var request store.PhotoImportRequest
	var report PhotoImportReport
	// A resumed pass recounts every group, so clear the interrupted pass's count and receipt together.
	importErr := metadata.AdvanceStorageOperation(ctx, operationID, "", 0, 0, 0, "{}")
	if importErr == nil {
		importErr = json.Unmarshal([]byte(operation.RequestJSON), &request)
	}
	if importErr == nil {
		report, importErr = r.Ingester.ImportPhotoDirectory(ctx, request.SourceRoot, request.Destination, opts)
	}
	if ctx.Err() != nil {
		// Daemon shutdown is not an operator cancellation; the next start resumes.
		return ctx.Err()
	}
	state, failure := store.StorageOperationCompleted, ""
	switch {
	case report.Cancelled:
		state = store.StorageOperationCancelled
	case importErr != nil:
		state, failure = store.StorageOperationFailed, importErr.Error()
	case len(report.Errors) > 0:
		state = store.StorageOperationFailed
		failure = fmt.Sprintf("%d photo groups failed; first %s: %v", len(report.Errors), report.Errors[0].Path, report.Errors[0].Err)
	}
	receipt, err := json.Marshal(report.Receipt)
	if err != nil {
		return errors.Join(importErr, err)
	}
	finishErr := metadata.FinishStorageOperation(context.WithoutCancel(ctx), operationID, state,
		string(receipt), failure, time.Now().Add(photoImportRetention))
	return errors.Join(importErr, finishErr)
}
