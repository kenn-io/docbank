package checkout

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/text/unicode/norm"
)

const receiptPathLimit = 100

type ScanPath struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

type ScanResult struct {
	Results     map[string]int64 `json:"checkout_results,omitempty"`
	HashedBytes int64            `json:"hashed_bytes"`
	TotalBytes  int64            `json:"total_bytes"`
	Files       int64            `json:"files"`
	Paths       []ScanPath       `json:"paths"`
	Untracked   []string         `json:"untracked"`
}

func verifyRootIdentity(root *os.Root, want string) error {
	identity, err := rootIdentity(root)
	if err != nil {
		return err
	}
	if identity == "" || identity != want {
		return fmt.Errorf("%w: destination identity changed; rebind the verified folder", store.ErrInvalidPhotoCheckout)
	}
	return nil
}

func rootIdentity(root *os.Root) (string, error) {
	file, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	return home.FileIdentity(file)
}

func (s Service) settlement() time.Duration {
	if s.settleTime > 0 {
		return s.settleTime
	}
	return 2 * time.Second
}

func openCheckoutRoot(c store.PhotoCheckout) (*os.Root, error) {
	if c.Destination == "" || c.RetiredAt != nil || c.State == "unbound" {
		return nil, fmt.Errorf("%w: checkout is retired or unbound", store.ErrInvalidPhotoCheckout)
	}
	root, err := os.OpenRoot(c.Destination)
	if err != nil {
		return nil, err
	}
	if err := verifyRootIdentity(root, c.DestinationIdentity); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

type scanHooks struct {
	root     *os.Root
	before   func(context.Context, string) error
	progress func(context.Context, int64, bool) error
	only     map[string]bool
	finalize func(context.Context) error
}

func lookupTrackedEntry(tracked, normalized map[string]store.PhotoCheckoutEntry, ref string, lstat func(string) (os.FileInfo, error), readDir func(string) ([]os.DirEntry, error)) (store.PhotoCheckoutEntry, bool) {
	if entry, ok := tracked[ref]; ok {
		return entry, true
	}
	entry := normalized[norm.NFC.String(ref)]
	if entry.Path == "" {
		return store.PhotoCheckoutEntry{}, false
	}
	want, err := lstat(filepath.FromSlash(entry.Path))
	if err != nil {
		return store.PhotoCheckoutEntry{}, false
	}
	actual, err := lstat(filepath.FromSlash(ref))
	if err != nil || !os.SameFile(want, actual) {
		return store.PhotoCheckoutEntry{}, false
	}
	listing, err := readDir(filepath.ToSlash(filepath.Dir(filepath.FromSlash(ref))))
	if err != nil {
		return store.PhotoCheckoutEntry{}, false
	}
	for _, child := range listing {
		if child.Name() == filepath.Base(filepath.FromSlash(entry.Path)) {
			return store.PhotoCheckoutEntry{}, false
		}
	}
	return entry, true
}

func (s Service) scanWorking(ctx context.Context, c store.PhotoCheckout, process func(context.Context, ingest.FolderFile, store.PhotoCheckoutEntry) error, save func([]store.PhotoCheckoutObservation) error, hooks scanHooks) (ScanResult, error) {
	result := ScanResult{Results: make(map[string]int64), Paths: []ScanPath{}, Untracked: []string{}}
	root := hooks.root
	if root == nil {
		var err error
		root, err = openCheckoutRoot(c)
		if err != nil {
			return result, err
		}
		defer func() { _ = root.Close() }()
	}
	entries, err := s.Store.PhotoCheckoutEntries(ctx, c.ID)
	if err != nil {
		return result, err
	}
	tracked := make(map[string]store.PhotoCheckoutEntry, len(entries))
	normalized := make(map[string]store.PhotoCheckoutEntry, len(entries))
	resolved := make(map[string]store.PhotoCheckoutEntry, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if prior, exists := tracked[entry.Path]; exists {
			return result, fmt.Errorf("%w: checkout path %q is shared by files %s and %s", store.ErrInvalidPhotoCheckout, entry.Path, prior.FileID, entry.FileID)
		}
		tracked[entry.Path] = entry
		key := norm.NFC.String(entry.Path)
		if _, exists := normalized[key]; exists {
			normalized[key] = store.PhotoCheckoutEntry{} // Ambiguous spellings never alias.
		} else {
			normalized[key] = entry
		}
	}
	var observations []store.PhotoCheckoutObservation
	observationByID := make(map[string]int, len(entries))
	scanner, err := ingest.NewFolderScanner(config.WatchConfig{Name: "photo-checkout", Source: c.Destination, Destination: "/", SettleTime: config.Duration(s.settlement()), ScanInterval: config.Duration(time.Second)}, func(ctx context.Context, file ingest.FolderFile) error {
		ref := filepath.ToSlash(file.Ref)
		entry := resolved[ref]
		seen[entry.Path] = true
		if hooks.only != nil && !hooks.only[entry.FileID] {
			return nil
		}
		file.Progress = func(n int64) error {
			if hooks.progress != nil {
				return hooks.progress(ctx, n, false)
			}
			return nil
		}
		hash, err := file.Digest(ctx)
		if err != nil {
			return err
		}
		identity, err := home.FileIdentity(file.File)
		if err != nil {
			return err
		}
		mtime := file.Info.ModTime().UTC().Format(time.RFC3339Nano)
		now := time.Now().UTC()
		since := now
		if entry.ObservedIdentity == identity && entry.ObservedHash == hash && entry.ObservedSize == file.Info.Size() && entry.ObservedMtime == mtime {
			if prior, err := time.Parse(time.RFC3339Nano, entry.ObservedSince); err == nil {
				since = prior
			}
		}
		state := "pending"
		if hash == entry.Hash && file.Info.Size() == entry.Size {
			state = "clean"
		} else if now.Sub(since) >= s.settlement() {
			state = "edited"
		}
		observation := store.PhotoCheckoutObservation{FileID: entry.FileID, Identity: identity, Hash: hash, Size: file.Info.Size(), Mtime: mtime, Since: since.Format(time.RFC3339Nano), State: state}
		observationIndex := len(observations)
		observations = append(observations, observation)
		observationByID[entry.FileID] = observationIndex
		if process != nil && state == "edited" {
			entry.ObservedIdentity, entry.ObservedHash, entry.ObservedSize, entry.ObservedMtime = identity, hash, file.Info.Size(), mtime
			if err := process(ctx, file, entry); err != nil {
				if errors.Is(err, store.ErrStaleRevision) {
					observations[observationIndex].State = "conflict"
				} else {
					return err
				}
			}
		}
		if hooks.progress != nil {
			return hooks.progress(ctx, 0, true)
		}
		return nil
	}, func(_ context.Context, ref string, err error) error {
		return fmt.Errorf("checkout path %q: %w", ref, err)
	})
	if err != nil {
		return result, err
	}
	scanner.VerifyRoot = func(opened *os.Root) error {
		return verifyRootIdentity(opened, c.DestinationIdentity)
	}
	scanner.BeforeFile = func(ctx context.Context, ref string) error {
		ref = filepath.ToSlash(ref)
		if filepublish.IsRootStagePath(ref) {
			return filepath.SkipDir
		}
		entry, ok := lookupTrackedEntry(tracked, normalized, ref, root.Lstat, func(parent string) ([]os.DirEntry, error) {
			return fs.ReadDir(root.FS(), parent)
		})
		if !ok {
			result.Results["untracked"]++
			if len(result.Untracked) < receiptPathLimit {
				result.Untracked = append(result.Untracked, ref)
			}
			return filepath.SkipDir
		}
		resolved[ref] = entry
		if hooks.before != nil && (hooks.only == nil || hooks.only[entry.FileID]) {
			return hooks.before(ctx, ref)
		}
		return nil
	}
	scanner.SourceChanged = func(_ context.Context, ref string) error {
		ref = filepath.ToSlash(ref)
		entry, ok := resolved[ref]
		if !ok {
			return nil
		}
		seen[entry.Path] = true
		pending := store.PhotoCheckoutObservation{FileID: entry.FileID, Since: time.Now().UTC().Format(time.RFC3339Nano), State: "pending"}
		if i, ok := observationByID[entry.FileID]; ok {
			observations[i] = pending
		} else {
			observationByID[entry.FileID] = len(observations)
			observations = append(observations, pending)
		}
		return nil
	}
	scanner.Excluded = func(_ context.Context, ref string) error {
		ref = filepath.ToSlash(ref)
		for name := range tracked {
			if name == ref || strings.HasPrefix(name, ref+"/") {
				return fmt.Errorf("checkout path %q is excluded by scanner", ref)
			}
		}
		return nil
	}
	scanErr := scanner.ScanOnce(ctx, 1)
	if scanErr == nil {
		scanErr = checkDestinationIdentity(root, c.Destination)
	}
	if scanErr == nil {
		for _, entry := range entries {
			if seen[entry.Path] || hooks.only != nil && !hooks.only[entry.FileID] {
				continue
			}
			info, err := root.Lstat(filepath.FromSlash(entry.Path))
			if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && !info.Mode().IsRegular() {
				if err == nil {
					err = errors.New("tracked path was excluded or is not a regular file")
				}
				return result, fmt.Errorf("checkout path %q: %w", entry.Path, err)
			}
			// A case-only rename can still resolve through the recorded spelling.
			// Unmatched regular files remain untracked; the recorded path is missing.
			observations = append(observations, store.PhotoCheckoutObservation{FileID: entry.FileID, State: "missing"})
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if save != nil && scanErr != nil {
		return result, scanErr
	}
	if hooks.finalize != nil && scanErr == nil {
		if err := hooks.finalize(ctx); err != nil {
			return result, err
		}
	}
	if save == nil {
		save = func(observations []store.PhotoCheckoutObservation) error {
			return s.mutate(ctx, func() error {
				return s.Store.ObservePhotoCheckoutEntries(ctx, c.ID, c.DestinationIdentity, observations)
			})
		}
	}
	if err := save(observations); err != nil {
		return result, errors.Join(scanErr, err)
	}
	err = result.summarize(ctx, s.Store, c.ID)
	return result, errors.Join(scanErr, err)
}

func (s Service) rebind(ctx context.Context, id, destination, operationID string, root *os.Root, hooks scanHooks) (ScanResult, error) {
	c, err := s.Store.PhotoCheckout(ctx, id)
	if err != nil {
		return ScanResult{}, err
	}
	proof, err := s.checkRootProtection(ctx, root, destination, id)
	if err != nil {
		return ScanResult{}, err
	}
	identity, err := rootIdentity(root)
	if err != nil {
		return ScanResult{}, err
	}
	c.Destination, c.DestinationIdentity, c.State = destination, identity, "completed"
	hooks.root = root
	return s.scanWorking(ctx, c, nil, func(observations []store.PhotoCheckoutObservation) error {
		present := false
		for _, o := range observations {
			if o.Identity != "" {
				present = true
			} else if o.State != "missing" {
				return ingest.ErrSourceChanged
			}
		}
		if !present {
			return fmt.Errorf("%w: folder contains no tracked regular files", store.ErrInvalidPhotoCheckout)
		}
		if err := creationMu.Acquire(ctx, 1); err != nil {
			return fmt.Errorf("waiting for checkout creation: %w", err)
		}
		defer creationMu.Release(1)
		if err := s.validateRootOverlap(ctx, destination, id); err != nil {
			return err
		}
		checked, err := s.Store.PhotoCheckout(ctx, id)
		if err != nil {
			return err
		}
		if err := store.ValidatePhotoCheckoutRebindDestination(checked, identity); err != nil {
			return err
		}
		return s.mutate(ctx, func() error {
			latest, err := s.Store.PhotoCheckout(ctx, id)
			if err != nil {
				return err
			}
			if latest.Destination != checked.Destination || latest.DestinationIdentity != checked.DestinationIdentity {
				return store.ErrPhotoCheckoutConflict
			}
			if err := s.recheckRootProtection(ctx, root, destination, id, proof); err != nil {
				return err
			}
			return s.Store.BindPhotoCheckout(ctx, id, destination, identity, operationID, observations)
		})
	}, hooks)
}

func (r *ScanResult) summarize(ctx context.Context, catalog *store.Store, id string) error {
	counts, entries, err := catalog.PhotoCheckoutWorkingSummary(ctx, id, receiptPathLimit)
	if err != nil {
		return err
	}
	for state, count := range counts {
		r.Results[state] += count
	}
	for _, e := range entries {
		r.Paths = append(r.Paths, ScanPath{Path: e.Path, State: e.WorkingState})
	}
	return nil
}
