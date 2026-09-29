package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.kenn.io/docbank/internal/store"
)

type PhotoImportCandidate struct {
	Path      string
	Kind      store.PhotoSourceKind
	MediaType string
}

type PhotoImportGroup struct {
	Key      string
	Folder   string
	Stem     string
	Isolated bool
	Members  []PhotoImportCandidate
}

type PhotoImportOptions struct {
	SettleInterval time.Duration
	Mutate         func(context.Context, func() error) error
	ActivityBegin  func()
	ActivityEnd    func()
	RunID          string
	Choice         *store.PhotoImportChoice
	Progress       func(PhotoImportProgress)
}

// DefaultPhotoImportSettleInterval keeps a source file stable before reading.
const DefaultPhotoImportSettleInterval = time.Second

type PhotoImportProgress struct {
	Done      int
	Total     int
	Added     int
	Skipped   int
	Failed    int
	Ambiguous int
	Path      string
}

type PhotoImportReport struct {
	Run       store.PhotoImportRun
	Added     int
	Skipped   int
	Failed    int
	Ambiguous int
	Errors    []FileError
}

func photoImportGroupKey(candidate PhotoImportCandidate) string {
	return store.PhotoImportGroupKey(candidate.Path, candidate.Kind)
}

// GroupPhotoCandidates returns deterministic same-folder, same-stem groups.
// A video always keeps its own group. Multiple RAW files remain in one group
// so the store can report an explicit ambiguity.
func GroupPhotoCandidates(candidates []PhotoImportCandidate) []PhotoImportGroup {
	grouped := make(map[string]*PhotoImportGroup)
	for _, candidate := range candidates {
		key := photoImportGroupKey(candidate)
		group := grouped[key]
		if group == nil {
			folder, stem := store.PhotoImportSourceKey(candidate.Path)
			group = &PhotoImportGroup{Key: key, Folder: folder, Stem: stem}
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

func photoImportChoiceMatchesMember(choice *store.PhotoImportChoice, member store.PhotoImportMember) bool {
	if choice == nil {
		return false
	}
	return choice.RawSourcePath != "" && filepath.Clean(choice.RawSourcePath) == filepath.Clean(member.OriginalPath) &&
		(member.BlobHash == "" || choice.RawBlobHash == "" || choice.RawBlobHash == member.BlobHash)
}

func partitionPhotoImportGroup(group PhotoImportGroup, choice *store.PhotoImportChoice) []PhotoImportGroup {
	if choice == nil || choice.GroupKey != "" && choice.GroupKey != group.Key {
		return []PhotoImportGroup{group}
	}
	var raw []PhotoImportCandidate
	for _, member := range group.Members {
		if member.Kind == store.PhotoSourceRAW {
			raw = append(raw, member)
		}
	}
	choiceNamesIncomingRaw := len(raw) == 1 && choice.RawSourcePath != "" &&
		filepath.Clean(choice.RawSourcePath) == filepath.Clean(raw[0].Path)
	if len(raw) == 1 && !choiceNamesIncomingRaw &&
		(choice.RawAssetID != "" || choice.RawFileID != "") {
		primary := group
		primary.Members = nil
		for _, member := range group.Members {
			if member.Kind != store.PhotoSourceRAW {
				primary.Members = append(primary.Members, member)
			}
		}
		standalone := group
		standalone.Key = group.Key + "|raw|" + filepath.Clean(raw[0].Path)
		standalone.Stem = filepath.Base(raw[0].Path)
		standalone.Isolated = true
		standalone.Members = raw
		if len(primary.Members) == 0 {
			return []PhotoImportGroup{standalone}
		}
		return []PhotoImportGroup{primary, standalone}
	}
	if len(raw) < 2 {
		return []PhotoImportGroup{group}
	}
	chosen := -1
	for index, member := range group.Members {
		if member.Kind != store.PhotoSourceRAW || !photoImportChoiceMatchesMember(choice, store.PhotoImportMember{OriginalPath: member.Path}) {
			continue
		}
		if chosen >= 0 {
			return []PhotoImportGroup{group}
		}
		chosen = index
	}
	if chosen < 0 && (choice.RawAssetID == "" || choice.RawFileID == "") {
		return []PhotoImportGroup{group}
	}
	primary := group
	primary.Members = nil
	if chosen >= 0 {
		primary.Members = append(primary.Members, group.Members[chosen])
	}
	for index, member := range group.Members {
		if index != chosen && member.Kind != store.PhotoSourceRAW {
			primary.Members = append(primary.Members, member)
		}
	}
	var groups []PhotoImportGroup
	if len(primary.Members) > 0 {
		groups = append(groups, primary)
	}
	for index, member := range group.Members {
		if index == chosen || member.Kind != store.PhotoSourceRAW {
			continue
		}
		standalone := group
		standalone.Key = group.Key + "|raw|" + filepath.Clean(member.Path)
		standalone.Stem = filepath.Base(member.Path)
		standalone.Isolated = true
		standalone.Members = []PhotoImportCandidate{member}
		groups = append(groups, standalone)
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

func settlePhotoImportCandidate(ctx context.Context, ing *Ingester, candidate PhotoImportCandidate, interval time.Duration) (store.PhotoImportMember, error) {
	fingerprint, err := observeLocalFileFingerprint(candidate.Path)
	if err != nil {
		return store.PhotoImportMember{}, fmt.Errorf("observing %s: %w", candidate.Path, err)
	}
	if interval > 0 {
		timer := time.NewTimer(interval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return store.PhotoImportMember{}, ctx.Err()
		case <-timer.C:
		}
	}
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

func (ing *Ingester) ImportPhotoDirectory(ctx context.Context, root, destination string, opts PhotoImportOptions) (report PhotoImportReport, retErr error) {
	if err := store.ValidatePhotoImportChoice(opts.Choice); err != nil {
		return report, err
	}
	mutate := func(mutationCtx context.Context, fn func() error) error {
		if opts.Mutate != nil {
			return opts.Mutate(mutationCtx, fn)
		}
		return fn()
	}
	runMutation := func(fn func() error) error { return mutate(ctx, fn) }
	finalMutation := func(fn func() error) error { return mutate(context.Background(), fn) }
	finishEarly := func(err error) (PhotoImportReport, error) {
		var finishErr error
		if opts.RunID != "" {
			state := store.PhotoImportStateFailed
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				state = store.PhotoImportStateCancelled
			}
			finishErr = finalMutation(func() error {
				_, finishErr := ing.Store.FinishPhotoImportRun(context.Background(), opts.RunID, state, err.Error())
				return finishErr
			})
			if final, readErr := ing.Store.PhotoImportRun(context.Background(), opts.RunID); readErr == nil {
				report.Run = final
			}
		}
		return report, errors.Join(err, finishErr)
	}
	candidates, err := discoverPhotoCandidates(ctx, root)
	if err != nil {
		return finishEarly(err)
	}
	groups := GroupPhotoCandidates(candidates)
	expandedGroups := make([]PhotoImportGroup, 0, len(groups))
	for _, group := range groups {
		expandedGroups = append(expandedGroups, partitionPhotoImportGroup(group, opts.Choice)...)
	}
	groups = expandedGroups
	if destination == "" {
		destination = "/"
	}
	var dest store.Node
	err = runMutation(func() error {
		var destinationErr error
		dest, destinationErr = ing.Store.EnsurePhotoImportDestination(ctx, destination)
		return destinationErr
	})
	if err != nil {
		return finishEarly(fmt.Errorf("resolving photo import destination: %w", err))
	}
	if opts.RunID == "" {
		var startErr error
		startedRun, startErr := func() (store.PhotoImportRun, error) {
			var started store.PhotoImportRun
			err := runMutation(func() error {
				var createErr error
				started, createErr = ing.Store.StartPhotoImportRun(ctx, root, destination, int64(len(groups)))
				return createErr
			})
			return started, err
		}()
		if startErr != nil {
			return report, startErr
		}
		report.Run = startedRun
		opts.RunID = startedRun.ID
	} else {
		report.Run, err = ing.Store.PhotoImportRun(ctx, opts.RunID)
		if err != nil {
			return finishEarly(err)
		}
	}
	if report.Run.TotalGroups != int64(len(groups)) {
		var updated store.PhotoImportRun
		err = runMutation(func() error {
			var updateErr error
			updated, updateErr = ing.Store.SetPhotoImportTotalGroups(ctx, opts.RunID, int64(len(groups)))
			return updateErr
		})
		if err != nil {
			return finishEarly(err)
		}
		report.Run = updated
	}
	var run store.IngestRun
	err = runMutation(func() error {
		var beginErr error
		run, beginErr = ing.Store.BeginIngest(ctx, "photo-import", root)
		return beginErr
	})
	if err != nil {
		return finishEarly(err)
	}
	if err := runMutation(func() error { return ing.Store.EnsurePhotoImportAllowed(ctx) }); err != nil {
		return finishEarly(err)
	}
	if opts.SettleInterval < 0 {
		opts.SettleInterval = 0
	}
	if opts.ActivityBegin != nil {
		opts.ActivityBegin()
		if opts.ActivityEnd != nil {
			defer opts.ActivityEnd()
		}
	}
	report.Errors = make([]FileError, 0)
	updateProgress := func(added, skipped, failed, ambiguous int64, detail *store.PhotoImportAmbiguity) error {
		if opts.RunID == "" {
			return nil
		}
		return runMutation(func() error {
			_, err := ing.Store.UpdatePhotoImportProgress(ctx, opts.RunID, added, skipped, failed, ambiguous, detail)
			return err
		})
	}
	for index, group := range groups {
		if err := ctx.Err(); err != nil {
			retErr = err
			break
		}
		current, runErr := ing.Store.PhotoImportRun(ctx, opts.RunID)
		if runErr != nil {
			retErr = runErr
			break
		}
		if current.CancelRequested {
			retErr = context.Canceled
			break
		}
		var (
			members    []store.PhotoImportMember
			groupReads []FileError
			result     store.PhotoImportResult
		)
		process := func() error {
			current, currentErr := ing.Store.PhotoImportRun(ctx, opts.RunID)
			if currentErr != nil {
				return currentErr
			}
			if current.CancelRequested {
				return context.Canceled
			}
			members = make([]store.PhotoImportMember, 0, len(group.Members))
			groupReads = nil
			for _, candidate := range group.Members {
				member, readErr := settlePhotoImportCandidate(ctx, ing, candidate, opts.SettleInterval)
				if readErr != nil {
					groupReads = append(groupReads, FileError{Path: candidate.Path, Err: readErr})
					continue
				}
				members = append(members, member)
			}
			if len(groupReads) > 0 {
				for _, member := range members {
					_ = ing.cleanupLoose(member.BlobHash)
				}
				return fmt.Errorf("reading photo import group: %w", groupReads[0].Err)
			}
			choice := opts.Choice
			if choice != nil && choice.GroupKey != "" && choice.GroupKey != group.Key {
				choice = nil
			}
			photoGroup := store.PhotoImportGroup{Key: group.Key, SourceFolder: group.Folder, Stem: group.Stem,
				DestinationID: dest.ID, RunID: opts.RunID, Isolated: group.Isolated, Members: members, Choice: choice}
			photoResult, processErr := ing.Store.IngestPhotoGroup(ctx, run, photoGroup)
			result = photoResult
			for _, member := range members {
				_ = ing.cleanupLoose(member.BlobHash)
			}
			return processErr
		}
		if opts.Mutate != nil {
			err = opts.Mutate(ctx, process)
		} else {
			err = process()
		}
		report.Errors = append(report.Errors, groupReads...)
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			retErr = err
		case err == nil:
			if result.Added {
				report.Added++
			} else {
				report.Skipped++
			}
		case errors.Is(err, store.ErrPhotoImportAmbiguous):
			report.Ambiguous++
			report.Errors = append(report.Errors, FileError{Path: group.Members[0].Path, Err: err})
			if opts.RunID != "" {
				ambiguityErr, ok := errors.AsType[*store.PhotoImportAmbiguityError](err)
				if ok {
					retErr = updateProgress(0, 0, 0, 1, &ambiguityErr.PhotoImportAmbiguity)
				} else {
					retErr = updateProgress(0, 0, 0, 1, nil)
				}
			}
		default:
			report.Failed++
			if len(groupReads) == 0 {
				report.Errors = append(report.Errors, FileError{Path: group.Members[0].Path, Err: err})
			}
			if opts.RunID != "" {
				retErr = updateProgress(0, 0, 1, 0, nil)
			}
		}
		if opts.Progress != nil {
			opts.Progress(PhotoImportProgress{Done: index + 1, Total: len(groups), Added: report.Added, Skipped: report.Skipped, Failed: report.Failed, Ambiguous: report.Ambiguous, Path: group.Members[0].Path})
		}
		if retErr != nil {
			break
		}
	}
	finishRun := func(state, errorText string) error {
		if opts.RunID == "" {
			return nil
		}
		return finalMutation(func() error {
			_, err := ing.Store.FinishPhotoImportRun(context.Background(), opts.RunID, state, errorText)
			return err
		})
	}
	var finishErr error
	if retErr != nil && errors.Is(retErr, context.Canceled) {
		finishErr = finishRun(store.PhotoImportStateCancelled, retErr.Error())
	} else if retErr != nil {
		finishErr = finishRun(store.PhotoImportStateFailed, retErr.Error())
	} else if report.Ambiguous > 0 {
		finishErr = finishRun(store.PhotoImportStateAmbiguous, "")
	} else if report.Failed > 0 {
		finishErr = finishRun(store.PhotoImportStateFailed, report.Errors[0].Err.Error())
	} else {
		finishErr = finishRun(store.PhotoImportStateCompleted, "")
	}
	retErr = errors.Join(retErr, finishErr)
	if final, readErr := ing.Store.PhotoImportRun(context.Background(), opts.RunID); readErr == nil {
		report.Run = final
	}
	return report, retErr
}
