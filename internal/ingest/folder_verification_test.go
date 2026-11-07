package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestFolderVerificationIgnoresUnchangedNativeFingerprint(t *testing.T) {
	var digests []string
	scanner, now := publicationScanner(t, func(ctx context.Context, file FolderFile) error {
		digest, err := file.Digest(ctx)
		digests = append(digests, digest)
		return err
	})
	scanner.VerifyInterval = time.Minute
	scanner.watcher.config.CaptureOnChange = true
	name := filepath.Join(scanner.watcher.config.Source, "a.md")
	require.NoError(t, os.WriteFile(name, []byte("first"), 0o600))
	root := publicationRoot(t, scanner)
	var scans []FolderScanResult
	scanner.AfterScan = func(_ context.Context, result FolderScanResult) error { scans = append(scans, result); return nil }
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	require.Len(t, digests, 1)
	initial := *now
	// Model a detector cache whose fingerprint cannot distinguish these bytes.
	// Use the real current handle fingerprint so confined read/recheck still run.
	rewriteSourceWithRestoredMtime(t, name, []byte("other"))
	state, err := (watchSource{root: root, ref: "a.md"}).observe(true)
	require.NoError(t, err)
	observation := scanner.watcher.observations["a.md"]
	observation.fingerprint = state.fingerprint
	scanner.watcher.observations["a.md"] = observation
	*now = initial.Add(59 * time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	require.Len(t, digests, 1, "quiet native hints alone do not force a read")
	*now = initial.Add(time.Minute)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	require.Len(t, digests, 2, "verification must read despite the detector cache")
	sum := sha256.Sum256([]byte("other"))
	assert.Equal(t, hex.EncodeToString(sum[:]), digests[1])
	assert.True(t, scans[0].Scan.FullVerification)
	assert.False(t, scans[1].Scan.FullVerification)
	assert.True(t, scans[2].Scan.FullVerification)
}

func TestFolderVerificationFailedPassKeepsDueTime(t *testing.T) {
	for _, mode := range []string{"read", "unsettled", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			var fail bool
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			scanner, now := publicationScanner(t, func(context.Context, FolderFile) error {
				if fail {
					if mode == "canceled" {
						cancel()
						return context.Canceled
					}
					if mode == "read" {
						return &SourceAccessError{Err: fs.ErrPermission}
					}
				}
				return nil
			})
			scanner.VerifyInterval = time.Minute
			scanner.watcher.immediate = true
			require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "a.md"), []byte("note"), 0o600))
			root := publicationRoot(t, scanner)
			var statuses []FolderScanStatus
			var results []FolderScanResult
			scanner.ScanStatus = func(_ context.Context, status FolderScanStatus) error {
				statuses = append(statuses, status)
				return nil
			}
			scanner.AfterScan = func(_ context.Context, result FolderScanResult) error { results = append(results, result); return nil }
			require.NoError(t, scanner.watcher.scan(ctx, root))
			initial := *now
			*now = initial.Add(time.Minute)
			fail = true
			if mode == "unsettled" {
				scanner.watcher.immediate = false
				require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "new.md"), []byte("new"), 0o600))
			}
			err := scanner.watcher.scan(ctx, root)
			if mode == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			require.NotEmpty(t, statuses)
			last := statuses[len(statuses)-1]
			assert.Equal(t, initial, last.LastVerifiedAt)
			assert.Equal(t, initial.Add(time.Minute), last.VerificationDueAt)
			require.Len(t, results, 2)
			assert.False(t, results[1].Complete)
		})
	}
}

func TestFolderVerificationLongPassReportsLag(t *testing.T) {
	var now *time.Time
	scanner, clock := publicationScanner(t, func(context.Context, FolderFile) error { *now = now.Add(90 * time.Second); return nil })
	now = clock
	scanner.VerifyInterval = time.Minute
	scanner.watcher.immediate = true
	require.NoError(t, os.WriteFile(filepath.Join(scanner.watcher.config.Source, "a.md"), []byte("note"), 0o600))
	var statuses []FolderScanStatus
	scanner.ScanStatus = func(_ context.Context, status FolderScanStatus) error {
		statuses = append(statuses, status)
		return nil
	}
	initial := *now
	require.NoError(t, scanner.watcher.scan(t.Context(), publicationRoot(t, scanner)))
	var overdue bool
	for _, status := range statuses {
		if status.VerificationLag == 90*time.Second && status.VerificationDueAt.Equal(initial) {
			overdue = true
		}
	}
	assert.True(t, overdue, "a long pass reports lag before recording its successful completion")
	// The next serialized event reports the committed schedule. No fallible
	// status callback runs after the inventory completion callback.
	*now = now.Add(time.Second)
	require.NoError(t, scanner.watcher.scan(t.Context(), publicationRoot(t, scanner)))
	require.NotEmpty(t, statuses)
	last := statuses[len(statuses)-1]
	assert.Equal(t, initial.Add(90*time.Second), last.LastVerifiedAt)
	assert.Equal(t, initial.Add(150*time.Second), last.VerificationDueAt)
	assert.Zero(t, last.VerificationLag)
}

func TestFolderVerificationEqualDigestDoesNotUpload(t *testing.T) {
	var accepted string
	uploads, processed := 0, 0
	scanner, now := publicationScanner(t, func(ctx context.Context, file FolderFile) error {
		processed++
		digest, err := file.Digest(ctx)
		if err == nil && digest != accepted {
			uploads++
			accepted = digest
		}
		return err
	})
	scanner.VerifyInterval = time.Minute
	scanner.watcher.immediate = true
	name := filepath.Join(scanner.watcher.config.Source, "a.md")
	require.NoError(t, os.WriteFile(name, []byte("same"), 0o600))
	root := publicationRoot(t, scanner)
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	*now = now.Add(time.Minute)
	require.NoError(t, os.Chtimes(name, *now, *now))
	require.NoError(t, scanner.watcher.scan(t.Context(), root))
	assert.Equal(t, 2, processed, "full verification repeats stable processing")
	assert.Equal(t, 1, uploads, "digest equality remains the upload boundary")
}

func TestFolderVerificationStatusFailurePreventsComplete(t *testing.T) {
	scanner, _ := publicationScanner(t, nil)
	scanner.VerifyInterval = time.Minute
	failure := errors.New("synthetic status failure")
	scanner.ScanStatus = func(context.Context, FolderScanStatus) error { return failure }
	completed := false
	scanner.AfterScan = func(_ context.Context, result FolderScanResult) error { completed = result.Complete; return nil }
	require.ErrorIs(t, scanner.ScanOnce(t.Context(), 1), failure)
	assert.False(t, completed)
}

func TestFolderVerificationStatusCancellationPreventsComplete(t *testing.T) {
	scanner, _ := publicationScanner(t, nil)
	scanner.VerifyInterval = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	scanner.ScanStatus = func(context.Context, FolderScanStatus) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return nil
	}
	completed := false
	scanner.AfterScan = func(_ context.Context, result FolderScanResult) error {
		completed = result.Complete
		return nil
	}
	require.ErrorIs(t, scanner.ScanOnce(ctx, 1), context.Canceled)
	assert.False(t, completed)
	assert.True(t, scanner.lastVerifiedAt.IsZero())
}

// A fallible status sink must not turn a committed inventory into a failed
// scan: its last invocation belongs before the completion callback.
func TestFolderVerificationStatusCannotFailAfterCompletion(t *testing.T) {
	scanner, _ := publicationScanner(t, nil)
	scanner.VerifyInterval = time.Minute
	committed := false
	scanner.AfterScan = func(_ context.Context, result FolderScanResult) error {
		committed = result.Complete
		return nil
	}
	scanner.ScanStatus = func(context.Context, FolderScanStatus) error {
		if committed {
			return errors.New("status sink failed after inventory committed")
		}
		return nil
	}
	require.NoError(t, scanner.ScanOnce(t.Context(), 1))
	require.True(t, committed)
}

func TestFolderVerificationRejectsNegativeInterval(t *testing.T) {
	scanner, _ := publicationScanner(t, nil)
	scanner.VerifyInterval = -time.Second
	require.Error(t, scanner.ScanOnce(t.Context(), 1))
	require.Error(t, scanner.Run(t.Context()))
}

func TestFolderPublicationPollingDegradation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scanner, _ := publicationScanner(t, nil)
		scanner.VerifyInterval = time.Minute
		scanner.watcher.now = time.Now
		scanner.watcher.config.CaptureOnChange = true
		notifier := newFakeWatchNotificationSource(scanner.watcher.config.Source)
		attempts := 0
		scanner.watcher.newNotifications = func(string, exclusions) (watchNotificationSource, error) {
			attempts++
			if attempts == 1 {
				return nil, fs.ErrNotExist
			}
			return notifier, nil
		}
		var statuses []FolderScanStatus
		scanner.ScanStatus = func(_ context.Context, status FolderScanStatus) error {
			statuses = append(statuses, status)
			return nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- scanner.Run(ctx) }()
		synctest.Wait()
		time.Sleep(5 * time.Second) // Virtual time only; the real scan timer drives recovery.
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		var degraded, recovered bool
		for _, status := range statuses {
			if status.Degraded {
				degraded = true
			} else if degraded {
				recovered = true
			}
		}
		assert.True(t, degraded)
		assert.True(t, recovered)
		select {
		case <-notifier.closed:
		default:
			t.Fatal("notification source outlived cancellation")
		}
	})
}

func TestFolderVerificationSchedulingProperty(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("Hegel engine unavailable; deterministic scanner tests still run")
	}
	hegel.Test(t, func(ht *hegel.T) {
		interval := time.Duration(hegel.Draw(ht, hegel.Integers(1, 120))) * time.Second
		scanner, now := publicationScanner(t, nil)
		scanner.VerifyInterval = interval
		root := publicationRoot(t, scanner)
		var reject bool
		var result FolderScanResult
		var status FolderScanStatus
		failure := errors.New("synthetic incomplete receipt")
		scanner.AfterScan = func(_ context.Context, r FolderScanResult) error {
			result = r
			if reject {
				return failure
			}
			return nil
		}
		scanner.ScanStatus = func(_ context.Context, s FolderScanStatus) error { status = s; return nil }
		var last time.Time
		for range 8 {
			*now = now.Add(time.Duration(hegel.Draw(ht, hegel.Integers(0, 180))) * time.Second)
			reject = hegel.Draw(ht, hegel.Booleans())
			full := last.IsZero() || !now.Before(last.Add(interval))
			previous := last
			err := scanner.watcher.scan(t.Context(), root)
			if reject != errors.Is(err, failure) {
				ht.Fatalf("failure outcome mismatch: %v", err)
			}
			if result.Scan.FullVerification != full {
				ht.Fatalf("full verification %v, want %v", result.Scan.FullVerification, full)
			}
			if full && !reject {
				last = *now
			}
			if !status.LastVerifiedAt.Equal(previous) {
				ht.Fatalf("last reported verification %v, want previous completed pass %v", status.LastVerifiedAt, previous)
			}
			if !scanner.lastVerifiedAt.Equal(last) {
				ht.Fatalf("last completed verification %v, want %v", scanner.lastVerifiedAt, last)
			}
		}
	}, hegel.WithDatabase(t.TempDir()))
}

type failingPublicationNotifications struct{ *fakeWatchNotificationSource }

func (f failingPublicationNotifications) EndScan() error {
	return errors.New("synthetic mid-scan notification failure")
}

func TestFolderVerificationNotificationStatusBeforeCompletion(t *testing.T) {
	for _, transition := range []string{"degraded", "recovered"} {
		t.Run(transition, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				scanner, _ := publicationScanner(t, nil)
				scanner.watcher.now = time.Now
				scanner.watcher.config.CaptureOnChange = true
				notifier := newFakeWatchNotificationSource(scanner.watcher.config.Source)
				attempts := 0
				scanner.watcher.newNotifications = func(string, exclusions) (watchNotificationSource, error) {
					attempts++
					if transition == "degraded" {
						return failingPublicationNotifications{notifier}, nil
					}
					if attempts == 1 {
						return nil, fs.ErrNotExist
					}
					return notifier, nil
				}
				failure := errors.New("synthetic transition status sink failure")
				scanner.ScanStatus = func(_ context.Context, status FolderScanStatus) error {
					if transition == "degraded" && status.Degraded || transition == "recovered" && status.Scan.Number > 1 && !status.Degraded {
						return failure
					}
					return nil
				}
				var results []FolderScanResult
				scanner.AfterScan = func(_ context.Context, result FolderScanResult) error { results = append(results, result); return nil }
				done := make(chan error, 1)
				go func() { done <- scanner.Run(t.Context()) }()
				synctest.Wait()
				if transition == "recovered" {
					time.Sleep(5 * time.Second)
					synctest.Wait()
				}
				require.ErrorIs(t, <-done, failure)
				require.NotEmpty(t, results)
				require.False(t, results[len(results)-1].Complete, "a failed scan status must precede the affected completion receipt")
				select {
				case <-notifier.closed:
				default:
					t.Fatal("notification source outlived failed run")
				}
			})
		})
	}
}
