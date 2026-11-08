package ingest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func publicationScanner(t *testing.T, process func(context.Context, FolderFile) error) (*FolderScanner, *time.Time) {
	t.Helper()
	if process == nil {
		process = func(ctx context.Context, file FolderFile) error {
			_, err := file.Digest(ctx)
			return err
		}
	}
	scanner, err := NewFolderScanner(config.WatchConfig{
		Name: "synthetic", Source: t.TempDir(), Destination: "/notes",
		SettleTime: config.Duration(2 * time.Second), ScanInterval: config.Duration(5 * time.Second),
	}, process, func(context.Context, string, error) error { return nil })
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	scanner.watcher.now = func() time.Time { return now }
	scanner.watcher.rejected = make(map[string]struct{})
	return scanner, &now
}

func publicationRoot(t *testing.T, scanner *FolderScanner) *watchRoot {
	t.Helper()
	root, err := scanner.openRoot()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	return root
}

func TestFolderPublicationHookOrderIncludesQuietFiles(t *testing.T) {
	var processed int
	scanner, _ := publicationScanner(t, func(ctx context.Context, file FolderFile) error {
		processed++
		_, err := file.Digest(ctx)
		return err
	})
	require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "a.md"), []byte("alpha"), 0o600))
	scanner.watcher.immediate = true
	root := publicationRoot(t, scanner)
	var events []string
	var present []FolderPresence
	var scans []FolderScanResult
	scanner.BeforeScan = func(ctx context.Context, scan FolderScan) error {
		require.NoError(t, ctx.Err())
		assert.Equal(t, uint64(len(scans)+1), scan.Number)
		events = append(events, "before")
		return nil
	}
	scanner.ObservePresent = func(_ context.Context, item FolderPresence) (bool, error) {
		events = append(events, "present")
		present = append(present, item)
		return item.Scan == 3, nil
	}
	scanner.AfterScan = func(_ context.Context, result FolderScanResult) error {
		events = append(events, "after")
		scans = append(scans, result)
		return nil
	}
	for range 3 {
		require.NoError(t, scanner.watcher.scan(t.Context(), root))
	}
	assert.Equal(t, []string{"before", "present", "after", "before", "present", "after", "before", "present", "after"}, events)
	require.Len(t, present, 3)
	assert.False(t, present[0].Processed)
	assert.True(t, present[1].Processed)
	assert.NotEmpty(t, present[1].Digest)
	assert.True(t, present[1].Stable)
	assert.Equal(t, "a.md", present[1].Ref)
	assert.Equal(t, 2, processed, "quiet files are observed, but only explicitly forced ones repeat processing")
	for _, result := range scans {
		assert.True(t, result.Complete)
		assert.EqualValues(t, 1, result.Observed)
		assert.Zero(t, result.Failed)
	}
}

func TestFolderPublicationUnsettledCannotComplete(t *testing.T) {
	processed := false
	scanner, now := publicationScanner(t, func(context.Context, FolderFile) error { processed = true; return nil })
	require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "a.md"), []byte("alpha"), 0o600))
	root := publicationRoot(t, scanner)
	var results []FolderScanResult
	scanner.ObservePresent = func(_ context.Context, item FolderPresence) (bool, error) {
		assert.Equal(t, len(results) > 0, item.Stable)
		return true, nil // Force never bypasses the stability window.
	}
	scanner.AfterScan = func(_ context.Context, result FolderScanResult) error { results = append(results, result); return nil }
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	require.Len(t, results, 1)
	assert.False(t, processed)
	assert.False(t, results[0].Complete)
	assert.EqualValues(t, 1, results[0].Unsettled)
	*now = now.Add(2 * time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.True(t, processed)
	assert.True(t, results[1].Complete)
}

func TestFolderPublicationCallbackFailureCannotComplete(t *testing.T) {
	for _, stage := range []string{"before", "present", "after"} {
		t.Run(stage, func(t *testing.T) {
			scanner, _ := publicationScanner(t, nil)
			require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "a.md"), []byte("alpha"), 0o600))
			failure := errors.New("synthetic callback failure")
			scanner.BeforeScan = func(context.Context, FolderScan) error {
				if stage == "before" {
					return failure
				}
				return nil
			}
			scanner.ObservePresent = func(context.Context, FolderPresence) (bool, error) {
				if stage == "present" {
					return false, failure
				}
				return false, nil
			}
			var completed bool
			scanner.AfterScan = func(_ context.Context, result FolderScanResult) error {
				if stage == "after" {
					return failure
				}
				completed = result.Complete
				return nil
			}
			require.ErrorIs(t, scanner.ScanOnce(t.Context(), 1), failure)
			assert.False(t, completed)
		})
	}
}

func TestFolderPublicationIncompleteEvidence(t *testing.T) {
	for _, mode := range []string{"disappears", "read rejected", "retry", "changed", "boundary", "root replaced"} {
		t.Run(mode, func(t *testing.T) {
			scanner, _ := publicationScanner(t, func(context.Context, FolderFile) error {
				switch mode {
				case "read rejected":
					return &SourceAccessError{Err: fs.ErrPermission}
				case "retry":
					return ErrRetryLater
				case "changed":
					return ErrSourceChanged
				}
				return nil
			})
			name := filepath.Join(scanner.watcher.config.Source, "a.md")
			require.NoError(t, os.WriteFile(name, []byte("alpha"), 0o600))
			if mode == "boundary" {
				require.NoError(t, os.Remove(name))
				if err := os.Symlink("missing", name); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			scanner.watcher.beforeObserve = func(string) {
				if mode == "disappears" {
					require.NoError(t, os.Remove(name))
				}
				if mode == "root replaced" {
					root := scanner.watcher.config.Source
					if err := os.Rename(root, root+"-moved"); err != nil {
						t.Skipf("opened root rename unavailable: %v", err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(root + "-moved") })
					require.NoError(t, os.Mkdir(root, 0o700))
				}
			}
			var results []FolderScanResult
			scanner.AfterScan = func(_ context.Context, result FolderScanResult) error { results = append(results, result); return nil }
			err := scanner.ScanOnce(t.Context(), 1)
			if mode == "root replaced" {
				require.ErrorIs(t, err, ErrSourceChanged)
			}
			require.Len(t, results, 1)
			assert.False(t, results[0].Complete, "continued/skipped failures are not absence evidence")
			if mode == "root replaced" {
				assert.EqualValues(t, 1, results[0].Failed, "replacement interrupts one file observation; final root verification adds no source failure")
			} else {
				assert.Positive(t, results[0].Failed)
			}
		})
	}
}

func TestFolderPublicationEmptyAndExcludedScopeCompletes(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty", false: "excluded"}[empty], func(t *testing.T) {
			scanner, _ := publicationScanner(t, nil)
			if !empty {
				require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "attachment.txt"), []byte("not a note"), 0o600))
				scanner.BeforeFile = func(context.Context, string) error { return filepath.SkipDir }
			}
			var result FolderScanResult
			scanner.AfterScan = func(_ context.Context, r FolderScanResult) error { result = r; return nil }
			require.NoError(t, scanner.ScanOnce(t.Context(), 1))
			assert.True(t, result.Complete)
			assert.Zero(t, result.Observed)
			assert.Zero(t, result.Failed)
		})
	}
}

func TestFolderPublicationAfterScanJoinsWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release, after := make(chan struct{}), make(chan struct{}), make(chan struct{})
		scanner, _ := publicationScanner(t, func(context.Context, FolderFile) error {
			close(entered)
			<-release
			return nil
		})
		require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "a.md"), []byte("note"), 0o600))
		scanner.AfterScan = func(_ context.Context, result FolderScanResult) error {
			assert.EqualValues(t, 1, result.Observed)
			assert.True(t, result.Complete)
			close(after)
			return nil
		}
		done := make(chan error, 1)
		go func() { done <- scanner.ScanOnce(t.Context(), 3) }()
		<-entered
		synctest.Wait()
		select {
		case <-after:
			t.Error("inventory completed before the final worker exited")
		default:
		}
		close(release)
		require.NoError(t, <-done)
		select {
		case <-after:
		default:
			t.Error("completed scan did not report inventory")
		}
	})
}

// A change after the completion receipt belongs to the next scan. It must not
// make the already-completed one-shot inventory look uncommitted to its caller.
func TestFolderPublicationRootChangeAfterCompletionKeepsReceipt(t *testing.T) {
	scanner, _ := publicationScanner(t, nil)
	root := scanner.watcher.config.Source
	moved := root + "-moved"
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	completed := false
	scanner.AfterScan = func(_ context.Context, result FolderScanResult) error {
		require.True(t, result.Complete)
		completed = true
		if err := os.Rename(root, moved); err != nil {
			t.Skipf("opened root rename unavailable: %v", err)
		}
		require.NoError(t, os.Mkdir(root, 0o700))
		return nil
	}
	require.NoError(t, scanner.ScanOnce(t.Context(), 1))
	require.True(t, completed)
}

func TestFolderPublicationFailureCountsOnlySources(t *testing.T) {
	for _, mode := range []string{"file", "status", "root"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("synthetic terminal failure")
			scanner, _ := publicationScanner(t, func(context.Context, FolderFile) error { return failure })
			if mode == "root" {
				failure = ErrSourceChanged
				root := scanner.watcher.config.Source
				moved := root + "-moved"
				t.Cleanup(func() { _ = os.RemoveAll(moved) })
				scanner.BeforeScan = func(context.Context, FolderScan) error {
					if err := os.Rename(root, moved); err != nil {
						t.Skipf("opened root rename unavailable: %v", err)
					}
					return os.Mkdir(root, 0o700)
				}
			} else if mode == "file" {
				require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "a.md"), []byte("note"), 0o600))
			} else {
				calls := 0
				scanner.ScanStatus = func(context.Context, FolderScanStatus) error {
					calls++
					if calls == 2 {
						return failure
					}
					return nil
				}
			}
			var result FolderScanResult
			scanner.AfterScan = func(_ context.Context, r FolderScanResult) error { result = r; return nil }
			require.ErrorIs(t, scanner.ScanOnce(t.Context(), 1), failure)
			require.False(t, result.Complete)
			if mode == "file" {
				require.EqualValues(t, 1, result.Failed, "one failed file must not be counted again as a scan error")
			} else {
				require.Zero(t, result.Failed, "a scan-only failure has no failed source")
			}
		})
	}
}
