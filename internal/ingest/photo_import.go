package ingest

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

type PhotoImportCandidate struct {
	Path      string
	Kind      store.PhotoSourceKind
	MediaType string
}

type PhotoImportGroup struct {
	Folder  string
	Stem    string
	Members []PhotoImportCandidate
}

type PhotoImportOptions struct {
	SettleInterval time.Duration
	Mutate         func(context.Context, func() error) error
	ActivityBegin  func()
	ActivityEnd    func()
	// Cancelled reports an operator cancellation; the importer asks once per group.
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
			folder, stem := store.PhotoImportSourceKey(candidate.Path)
			group = &PhotoImportGroup{Folder: folder, Stem: stem}
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

func discoverPhotoCandidates(ctx context.Context, root string) ([]PhotoImportCandidate, error) {
	if root == "" {
		return nil, errors.New("photo import root is empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absRoot)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		absRoot, err = filepath.EvalSymlinks(absRoot)
		if err != nil {
			return nil, err
		}
	}
	var candidates []PhotoImportCandidate
	err = filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
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
			return nil
		}
		candidates = append(candidates, PhotoImportCandidate{Path: path, Kind: source.Kind, MediaType: source.MediaType})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover photo import sources: %w", err)
	}
	return candidates, nil
}

func (ing *Ingester) readPhotoImportMember(ctx context.Context, candidate PhotoImportCandidate, fingerprint localFileFingerprint) (store.PhotoImportMember, error) {
	content, err := ing.readLocalFile(ctx, candidate.Path, candidate.Path, nil, &fingerprint)
	if err != nil {
		return store.PhotoImportMember{}, err
	}
	source := store.ClassifyPhotoSource(candidate.Path)
	return store.PhotoImportMember{
		Name: filepath.Base(candidate.Path), Role: source.Role,
		BlobHash: content.hash, Size: content.size, MediaType: source.MediaType,
		OriginalPath: candidate.Path, OriginalMtime: content.mtime, Physical: content.physical,
	}, nil
}

// ImportPhotoDirectory imports every supported camera file under root into
// destination, one group transaction at a time. Files settle once for the
// whole scan outside the mutation gate; each group holds the gate only while
// it rechecks, publishes bytes, and commits. A group whose files changed
// since the scan counts as skipped.
func (ing *Ingester) ImportPhotoDirectory(ctx context.Context, root, destination string, opts PhotoImportOptions) (report PhotoImportReport, retErr error) {
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
	candidates, err := discoverPhotoCandidates(ctx, root)
	if err != nil {
		return report, err
	}
	groups := GroupPhotoCandidates(candidates)
	report.Total = len(groups)
	if destination == "" {
		destination = "/"
	}
	var dest store.Node
	var run store.IngestRun
	err = mutate(func() error {
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
	if opts.ActivityBegin != nil {
		opts.ActivityBegin()
		if opts.ActivityEnd != nil {
			defer opts.ActivityEnd()
		}
	}
	fingerprints := make(map[string]localFileFingerprint, len(candidates))
	observeErrors := make(map[string]error)
	for _, candidate := range candidates {
		fingerprint, err := observeLocalFileFingerprint(candidate.Path)
		if err != nil {
			observeErrors[candidate.Path] = fmt.Errorf("observing %s: %w", candidate.Path, err)
			continue
		}
		fingerprints[candidate.Path] = fingerprint
	}
	if opts.SettleInterval > 0 && len(candidates) > 0 {
		timer := time.NewTimer(opts.SettleInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return report, ctx.Err()
		case <-timer.C:
		}
	}
	report.Errors = make([]FileError, 0)
	for index, group := range groups {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if opts.Cancelled != nil {
			cancelled, err := opts.Cancelled(ctx)
			if err != nil {
				return report, err
			}
			if cancelled {
				report.Cancelled = true
				return report, nil
			}
		}
		result, groupErr := ing.importPhotoGroup(ctx, run, dest.ID, group, fingerprints, observeErrors, mutate)
		switch {
		case groupErr != nil && ctx.Err() != nil:
			return report, ctx.Err()
		case errors.Is(groupErr, ErrSourceChanged):
			// A file that changed since the scan waits for the next run.
			report.Receipt.Skipped++
		case groupErr != nil:
			report.Receipt.Failed++
			report.Errors = append(report.Errors, FileError{Path: group.Members[0].Path, Err: groupErr})
		case result.Ambiguity != nil:
			report.Receipt.Ambiguous++
			if len(report.Receipt.Ambiguities) < store.PhotoImportMaxAmbiguities {
				report.Receipt.Ambiguities = append(report.Receipt.Ambiguities, *result.Ambiguity)
			}
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
	fingerprints map[string]localFileFingerprint, observeErrors map[string]error, mutate func(func() error) error,
) (store.PhotoImportResult, error) {
	for _, candidate := range group.Members {
		if err := observeErrors[candidate.Path]; err != nil {
			return store.PhotoImportResult{}, err
		}
	}
	var result store.PhotoImportResult
	err := mutate(func() error {
		return ing.Blobs.WithMutation(ctx, func() error {
			members := make([]store.PhotoImportMember, 0, len(group.Members))
			defer func() {
				for _, member := range members {
					_ = ing.cleanupLoose(member.BlobHash)
				}
			}()
			for _, candidate := range group.Members {
				member, err := ing.readPhotoImportMember(ctx, candidate, fingerprints[candidate.Path])
				if err != nil {
					return err
				}
				members = append(members, member)
			}
			var err error
			result, err = ing.Store.IngestPhotoGroup(ctx, run, store.PhotoImportGroup{
				SourceFolder: group.Folder, Stem: group.Stem, DestinationID: destinationID, Members: members,
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
		encoded, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		return metadata.AdvanceStorageOperation(ctx, operationID, "", int64(done), 0, 0, string(encoded))
	}
	var request store.PhotoImportRequest
	var report PhotoImportReport
	importErr := json.Unmarshal([]byte(operation.RequestJSON), &request)
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
