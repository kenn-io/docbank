package qmdexport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func publicationTarget(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return filepath.Join(base, "export")
}

func publishOne(t *testing.T, target string) Receipt {
	t.Helper()
	body := []byte("synthetic retained body\n")
	source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
	receipt, err := Publish(t.Context(), target, "synthetic", []Source{source}, syntheticReader{source.BlobSHA256: body}, Options{})
	require.NoError(t, err)
	return receipt
}

func TestPublishFileSyncFailureBeforeSelectionPreservesCurrent(t *testing.T) {
	// Catches selecting unsynced body/pointer bytes or dropping sync failures.
	for _, failAt := range []int{1, 4} {
		t.Run(map[int]string{1: "body", 4: "pointer"}[failAt], func(t *testing.T) {
			target := publicationTarget(t)
			old, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
			require.NoError(t, err)
			body := []byte("synthetic next body")
			source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
			calls := 0
			selected, err := publishWithHooks(t.Context(), target, "synthetic", []Source{source}, syntheticReader{source.BlobSHA256: body}, Options{}, publishHooks{
				beforeFileSync: func(file *os.File) error {
					calls++
					if calls == failAt {
						require.NoError(t, file.Close())
					}
					return nil
				},
			})
			require.ErrorIs(t, err, os.ErrClosed)
			require.Empty(t, selected.GenerationID)
			current, loadErr := LoadCurrent(target)
			require.NoError(t, loadErr)
			require.Equal(t, old.GenerationID, current.GenerationID)
			staging, readErr := os.ReadDir(filepath.Join(target, ".staging"))
			require.NoError(t, readErr)
			require.Empty(t, staging)
		})
	}
}

func TestPublishReleaseFaultRetainsReceiptAndReleasesActualLock(t *testing.T) {
	// Catches an empty receipt after release or skipping actual lock release.
	target := publicationTarget(t)
	cause := errors.New("synthetic private release failure")
	selected, err := publishWithHooks(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{afterRelease: func() error { return cause }})
	var post *PostPublicationError
	require.ErrorAs(t, err, &post)
	require.Equal(t, "release_incomplete", post.Code)
	require.ErrorIs(t, err, cause)
	require.NotContains(t, err.Error(), cause.Error())
	require.NotEmpty(t, selected.GenerationID)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	again, err := Publish(ctx, target, "synthetic", nil, syntheticReader{}, Options{})
	require.NoError(t, err)
	require.Equal(t, selected.GenerationID, again.GenerationID)
}

func TestPublishErrorPriorityKeepsCleanupStateAndAllCauses(t *testing.T) {
	// Catches losing aggregate cleanup state when confirmation fails first.
	target := publicationTarget(t)
	publishOne(t, target)
	confirmation := errors.New("synthetic confirmation")
	release := errors.New("synthetic release")
	selected, err := publishWithHooks(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{
		beforeConfirmation: func() error { return confirmation }, afterRelease: func() error { return release },
	})
	var post *PostPublicationError
	require.ErrorAs(t, err, &post)
	require.Equal(t, "confirmation_incomplete", post.Code)
	require.Equal(t, 1, post.Cleanup.RemovedGenerations)
	require.ErrorIs(t, err, confirmation)
	require.ErrorIs(t, err, release)
	require.NotEmpty(t, selected.GenerationID)
}

func TestPublishSelectsExactBodyThenEmptyRemovesStale(t *testing.T) {
	// Catches missing pointer commit, body alteration, and stale-text retention.
	target := publicationTarget(t)
	old := publishOne(t, target)
	bodyPath := filepath.Join(old.CollectionPath, old.Manifest.Entries[0].RelativePath)
	body, err := os.ReadFile(bodyPath)
	require.NoError(t, err)
	require.Equal(t, "synthetic retained body\n", string(body))
	empty, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
	require.NoError(t, err)
	require.Empty(t, empty.Manifest.Entries)
	require.NoFileExists(t, bodyPath)
	current, err := LoadCurrent(target)
	require.NoError(t, err)
	require.Equal(t, empty.GenerationID, current.GenerationID)
	pointer, err := os.ReadFile(filepath.Join(target, "CURRENT"))
	require.NoError(t, err)
	require.Equal(t, empty.GenerationID+"\n", string(pointer))
	again, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
	require.NoError(t, err)
	require.Equal(t, empty.GenerationID, again.GenerationID)
}

func TestPublishRevalidatesRootAfterSourceCallback(t *testing.T) {
	// Catches using read-only preflight authority after a callback swaps roots.
	target := publicationTarget(t)
	old := publishOne(t, target)
	body := []byte("synthetic replacement source")
	source := syntheticSource(2, "00000000-0000-4000-8000-000000000002", body)
	displaced := target + "-preserved"
	reader := &mutatingSyntheticReader{contents: syntheticReader{source.BlobSHA256: body}, mutate: func() {
		require.NoError(t, os.Rename(target, displaced))
		require.NoError(t, os.Mkdir(target, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(target, "synthetic-sentinel"), []byte("preserve me"), 0o600))
	}}
	selected, err := Publish(t.Context(), target, "synthetic", []Source{source}, reader, Options{})
	require.Error(t, err)
	require.Empty(t, selected.GenerationID)
	current, loadErr := LoadCurrent(displaced)
	require.NoError(t, loadErr)
	require.Equal(t, old.GenerationID, current.GenerationID)
	value, readErr := os.ReadFile(filepath.Join(target, "synthetic-sentinel"))
	require.NoError(t, readErr)
	require.Equal(t, "preserve me", string(value))
}

func TestPublishRejectsSameIDDamagedGenerationWithoutReplacingIt(t *testing.T) {
	// Catches stamp-only reuse and overwriting an existing generation directory.
	target := publicationTarget(t)
	old := publishOne(t, target)
	bodyPath := filepath.Join(old.CollectionPath, old.Manifest.Entries[0].RelativePath)
	require.NoError(t, os.WriteFile(bodyPath, []byte("Synthetic retained body\n"), 0o600))
	body := []byte("synthetic retained body\n")
	source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
	selected, err := Publish(t.Context(), target, "synthetic", []Source{source}, syntheticReader{source.BlobSHA256: body}, Options{})
	require.Error(t, err)
	require.Empty(t, selected.GenerationID)
	value, readErr := os.ReadFile(bodyPath)
	require.NoError(t, readErr)
	require.Equal(t, "Synthetic retained body\n", string(value))
	value, readErr = os.ReadFile(filepath.Join(target, "CURRENT"))
	require.NoError(t, readErr)
	require.Equal(t, old.GenerationID+"\n", string(value))
}

func TestPublishRechecksReservedOwnershipBeforeSelection(t *testing.T) {
	// Catches selecting a new pointer after ownership becomes invalid between
	// the initial locked claim and the selection callback boundary.
	target := publicationTarget(t)
	old := publishOne(t, target)
	selected, err := publishWithHooks(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{beforeCurrent: func() {
		require.NoError(t, os.WriteFile(filepath.Join(target, markerName), []byte("{}\n"), 0o600))
	}})
	require.Error(t, err)
	require.Empty(t, selected.GenerationID)
	pointer, readErr := os.ReadFile(filepath.Join(target, "CURRENT"))
	require.NoError(t, readErr)
	require.Equal(t, old.GenerationID+"\n", string(pointer))
	marker, readErr := os.ReadFile(filepath.Join(target, markerName))
	require.NoError(t, readErr)
	require.Equal(t, "{}\n", string(marker))
}

func TestPublishConcurrentCallsSerializeAndRetirePriorSelection(t *testing.T) {
	// Catches bypassing the stable publication lock during selection or cleanup.
	target := publicationTarget(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	ready, release, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	type result struct {
		receipt Receipt
		err     error
	}
	firstDone, secondDone := make(chan result, 1), make(chan result, 1)
	body := []byte("synthetic concurrent body")
	source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
	go func() {
		receipt, err := publishWithHooks(ctx, target, "synthetic", []Source{source}, syntheticReader{source.BlobSHA256: body}, Options{}, publishHooks{beforeCurrent: func() {
			close(ready)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}})
		firstDone <- result{receipt, err}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("first publication did not reach selection")
	}
	lockBefore, err := os.Stat(filepath.Join(target, lockName))
	require.NoError(t, err)
	go func() {
		receipt, err := publishWithHooks(ctx, target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{ownership: ownershipHooks{waitingOnLock: func() { close(waiting) }}})
		secondDone <- result{receipt, err}
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("second publication did not contend")
	}
	close(release)
	var first, second result
	select {
	case first = <-firstDone:
	case <-ctx.Done():
		t.Fatal("first publication did not finish")
	}
	select {
	case second = <-secondDone:
	case <-ctx.Done():
		t.Fatal("second publication did not finish")
	}
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotEqual(t, first.receipt.GenerationID, second.receipt.GenerationID)
	require.NoDirExists(t, first.receipt.CollectionPath)
	current, err := LoadCurrent(target)
	require.NoError(t, err)
	require.Equal(t, second.receipt.GenerationID, current.GenerationID)
	lockAfter, err := os.Stat(filepath.Join(target, lockName))
	require.NoError(t, err)
	require.True(t, os.SameFile(lockBefore, lockAfter))
}

type publicationCatalog struct {
	sources []Source
	calls   int
}

func (c *publicationCatalog) QMDExportSources(context.Context, int) ([]Source, error) {
	c.calls++
	return c.sources, nil
}

func TestPublishPreflightRejectsBeforeCallbacks(t *testing.T) {
	// Catches catalog/blob access before invalid scalar or filesystem requests.
	for _, invalid := range []string{"nil-context", "canceled", "collection", "options", "root", "populated"} {
		t.Run(invalid, func(t *testing.T) {
			target := publicationTarget(t)
			ctx, collection, options := t.Context(), "synthetic", Options{}
			switch invalid {
			case "nil-context":
				ctx = nil
			case "canceled":
				ctx = canceledContext()
			case "collection":
				collection = "../invalid"
			case "options":
				options.MaxDocuments = -1
			case "root":
				target = "relative"
			case "populated":
				require.NoError(t, os.Mkdir(target, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(target, "synthetic-sentinel"), []byte("preserve me"), 0o600))
			}
			catalog := &publicationCatalog{}
			reader := &countingSyntheticReader{contents: syntheticReader{}}
			_, err := PublishActive(ctx, target, collection, catalog, reader, options)
			require.Error(t, err)
			require.Zero(t, catalog.calls)
			require.Zero(t, reader.opens)
			if invalid == "populated" {
				value, err := os.ReadFile(filepath.Join(target, "synthetic-sentinel"))
				require.NoError(t, err)
				require.Equal(t, "preserve me", string(value))
			}
		})
	}
	target := publicationTarget(t)
	catalog := &publicationCatalog{}
	receipt, err := PublishActive(t.Context(), target, "synthetic", catalog, syntheticReader{}, Options{})
	require.NoError(t, err)
	require.NotEmpty(t, receipt.GenerationID)
	require.Equal(t, 1, catalog.calls)
}

func TestPublishCancellationBeforeAndAfterSelection(t *testing.T) {
	// Catches treating a committed pointer as a rollback on late cancellation.
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
			target := publicationTarget(t)
			old := publishOne(t, target)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			hooks := publishHooks{beforeCurrent: cancel}
			if after {
				hooks = publishHooks{afterCurrent: cancel}
			}
			selected, err := publishWithHooks(ctx, target, "synthetic", nil, syntheticReader{}, Options{}, hooks)
			require.ErrorIs(t, err, context.Canceled)
			current, loadErr := LoadCurrent(target)
			require.NoError(t, loadErr)
			var post *PostPublicationError
			if after {
				require.ErrorAs(t, err, &post)
				require.Equal(t, "publication_canceled", post.Code)
				require.NotEmpty(t, selected.GenerationID)
				require.Equal(t, selected.GenerationID, current.GenerationID)
				require.NotEqual(t, old.GenerationID, selected.GenerationID)
			} else {
				require.NotErrorAs(t, err, &post)
				require.Empty(t, selected.GenerationID)
				require.Equal(t, old.GenerationID, current.GenerationID)
			}
		})
	}
}

func requirePublicationRootEntries(t *testing.T, target string) {
	t.Helper()
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.ElementsMatch(t, []string{markerName, lockName, "CURRENT", "generations", ".staging"}, names)
}

func TestPublishCanceledPointerDoesNotPoisonLaterPublication(t *testing.T) {
	// Catches abandoning a preselection pointer after cancellation, including
	// the same-generation reuse path where there is no generation ledger left.
	for _, reuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-generation", true: "same-generation"}[reuse], func(t *testing.T) {
			target := publicationTarget(t)
			old := publishOne(t, target)
			var sources []Source
			reader := syntheticReader{}
			if reuse {
				body := []byte("synthetic retained body\n")
				source := syntheticSource(1, "00000000-0000-4000-8000-000000000001", body)
				sources, reader = []Source{source}, syntheticReader{source.BlobSHA256: body}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			t.Cleanup(cancel)
			selected, err := publishWithHooks(ctx, target, "synthetic", sources, reader, Options{}, publishHooks{beforeCurrent: cancel})
			require.ErrorIs(t, err, context.Canceled)
			require.Empty(t, selected.GenerationID)
			pointer, err := os.ReadFile(filepath.Join(target, "CURRENT"))
			require.NoError(t, err)
			require.Equal(t, old.GenerationID+"\n", string(pointer))
			requirePublicationRootEntries(t, target)
			// Recovery must succeed with a fresh context, not select with an error.
			again, err := Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
			require.NoError(t, err)
			require.NotEmpty(t, again.GenerationID)
			requirePublicationRootEntries(t, target)
			require.NoDirExists(t, old.CollectionPath)
			stages, err := os.ReadDir(filepath.Join(target, ".staging"))
			require.NoError(t, err)
			require.Empty(t, stages)
			generations, err := os.ReadDir(filepath.Join(target, "generations"))
			require.NoError(t, err)
			require.Len(t, generations, 1)
		})
	}
}

func TestPublishCanceledPointerSyncPreservesEveryCause(t *testing.T) {
	// Catches skipping pointer rollback when cancellation accompanies another
	// preselection failure, or dropping any original/release cause.
	target := publicationTarget(t)
	old := publishOne(t, target)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	syncCause, releaseCause := errors.New("synthetic pointer sync"), errors.New("synthetic release")
	calls := 0
	selected, err := publishWithHooks(ctx, target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{
		beforeFileSync: func(*os.File) error {
			calls++
			if calls == 3 {
				cancel()
				return syncCause
			} // Empty manifest, stamp, pointer.
			return nil
		}, afterRelease: func() error { return releaseCause },
	})
	require.Empty(t, selected.GenerationID)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, syncCause)
	require.ErrorIs(t, err, releaseCause)
	require.NotContains(t, err.Error(), syncCause.Error())
	requirePublicationRootEntries(t, target)
	pointer, readErr := os.ReadFile(filepath.Join(target, "CURRENT"))
	require.NoError(t, readErr)
	require.Equal(t, old.GenerationID+"\n", string(pointer))
	_, err = Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
	require.NoError(t, err)
}

func TestPublishCanceledPointerSubstitutionPreservesReplacement(t *testing.T) {
	// Catches deleting a substituted private file just because its name was
	// this invocation's temporary pointer name.
	target := publicationTarget(t)
	old := publishOne(t, target)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	var pointerInfo os.FileInfo
	var pointerPath string
	var denied bool
	saved := filepath.Join(filepath.Dir(target), "synthetic-displaced-pointer")
	t.Cleanup(func() {
		err := os.Remove(saved)
		if !errors.Is(err, os.ErrNotExist) {
			require.NoError(t, err)
		}
	})
	calls := 0
	selected, err := publishWithHooks(ctx, target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{
		beforeFileSync: func(file *os.File) error {
			calls++
			if calls == 3 {
				var err error
				pointerInfo, err = file.Stat()
				require.NoError(t, err)
			}
			return nil
		}, beforeCurrent: func() {
			entries, err := os.ReadDir(target)
			require.NoError(t, err)
			for _, entry := range entries {
				info, err := entry.Info()
				require.NoError(t, err)
				if os.SameFile(info, pointerInfo) {
					pointerPath = filepath.Join(target, entry.Name())
					break
				}
			}
			require.NotEmpty(t, pointerPath)
			if err := os.Rename(pointerPath, saved); err != nil {
				require.Equal(t, "windows", runtime.GOOS, "unexpected denial of movable synthetic pointer: %v", err)
				info, statErr := os.Stat(pointerPath)
				require.NoError(t, statErr)
				require.True(t, os.SameFile(info, pointerInfo))
				t.Logf("native movable-pointer interference denied: %v", err)
				denied = true
			} else {
				require.NoError(t, os.WriteFile(pointerPath, []byte("synthetic replacement sentinel\n"), 0o600))
			}
			cancel()
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, selected.GenerationID)
	pointer, readErr := os.ReadFile(filepath.Join(target, "CURRENT"))
	require.NoError(t, readErr)
	require.Equal(t, old.GenerationID+"\n", string(pointer))
	if denied {
		require.NoFileExists(t, pointerPath)
		_, err = Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
		require.NoError(t, err)
	} else {
		require.ErrorIs(t, err, errIdentity)
		value, readErr := os.ReadFile(pointerPath)
		require.NoError(t, readErr)
		require.Equal(t, "synthetic replacement sentinel\n", string(value))
		selected, err = Publish(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{})
		var post *PostPublicationError
		require.ErrorAs(t, err, &post)
		require.Equal(t, "cleanup_incomplete", post.Code)
		require.Equal(t, 1, post.Cleanup.UnknownEntries)
		require.NotEmpty(t, selected.GenerationID)
		value, readErr = os.ReadFile(pointerPath)
		require.NoError(t, readErr)
		require.Equal(t, "synthetic replacement sentinel\n", string(value))
	}
}

func TestPublishConfirmationFailureRetainsSelectedReceipt(t *testing.T) {
	// Catches loss of the selected receipt or disclosure of callback errors.
	target := publicationTarget(t)
	publishOne(t, target)
	cause := errors.New("synthetic private sentinel")
	selected, err := publishWithHooks(t.Context(), target, "synthetic", nil, syntheticReader{}, Options{}, publishHooks{
		beforeConfirmation: func() error { return cause },
	})
	var post *PostPublicationError
	require.ErrorAs(t, err, &post)
	require.Equal(t, "confirmation_incomplete", post.Code)
	require.Equal(t, 1, post.Cleanup.RemovedGenerations)
	require.ErrorIs(t, err, cause)
	require.NotContains(t, err.Error(), cause.Error())
	current, loadErr := LoadCurrent(target)
	require.NoError(t, loadErr)
	require.Equal(t, selected.GenerationID, current.GenerationID)
}
