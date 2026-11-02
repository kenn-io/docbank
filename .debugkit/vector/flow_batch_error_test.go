package vector_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/vector"
)

type fillProviderError struct {
	code int
}

func (e *fillProviderError) Error() string {
	return fmt.Sprintf("provider status %d", e.code)
}

func TestFillSharedErrorClassifierFailsClosedWithoutProbes(t *testing.T) {
	providerErr := &fillProviderError{code: 503}
	for _, tc := range []struct {
		name            string
		classifier      func(error) bool
		wantClassifiers int
	}{
		{name: "nil classifier"},
		{name: "classifier false", wantClassifiers: 1, classifier: func(err error) bool {
			var got *fillProviderError
			require.ErrorAs(t, err, &got)
			return false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newMemStore()
			store.content = map[int64]string{1: "one", 2: "two", 3: "three"}
			var calls, classifiers, hooks int
			enc := func(context.Context, []string) ([][]float32, error) {
				calls++
				return nil, providerErr
			}
			classifier := tc.classifier
			if classifier != nil {
				classifier = func(err error) bool {
					classifiers++
					return tc.classifier(err)
				}
			}
			_, err := vector.Fill(t.Context(), store, 7, enc,
				vector.WithFillScanBatch[int64](3),
				vector.WithFillBatch[int64](vector.WithBatchSize(3)),
				vector.WithFillBatchErrorIsolation[int64](classifier),
				vector.WithFillEncodeError[int64](func(int64, error) bool {
					hooks++
					return true
				}),
			)
			require.Error(err)
			var got *fillProviderError
			require.ErrorAs(err, &got)
			assert.Same(providerErr, got)
			assert.Equal(1, calls)
			assert.Equal(tc.wantClassifiers, classifiers)
			assert.Zero(hooks)
		})
	}
}

func TestFillSharedErrorRejectedFirstProbeStopsDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(int64, error) bool
	}{
		{name: "nil hook"},
		{name: "false hook", hook: func(int64, error) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newMemStore()
			store.content = map[int64]string{1: "poison one", 2: "poison two"}
			var calls, classifiers, hooks int
			enc := func(context.Context, []string) ([][]float32, error) {
				calls++
				return nil, &fillProviderError{code: 400}
			}
			hook := tc.hook
			if hook != nil {
				hook = func(doc int64, err error) bool {
					hooks++
					return tc.hook(doc, err)
				}
			}
			_, err := vector.Fill(t.Context(), store, 7, enc,
				vector.WithFillScanBatch[int64](2),
				vector.WithFillBatch[int64](vector.WithBatchSize(2)),
				vector.WithFillBatchErrorIsolation[int64](func(error) bool {
					classifiers++
					return true
				}),
				vector.WithFillEncodeError[int64](hook),
			)
			require.Error(err)
			var providerErr *fillProviderError
			require.ErrorAs(err, &providerErr)
			assert.Equal(2, calls, "one shared call plus the first probe")
			assert.Equal(1, classifiers)
			assert.Equal(map[bool]int{true: 1, false: 0}[tc.hook != nil], hooks)
		})
	}
}

func TestFillSharedErrorAllowsTwoPoisonDocuments(t *testing.T) {
	assert := assert.New(t)
	store := newMemStore()
	store.content = map[int64]string{1: "poison one", 2: "poison two"}
	var calls int
	hooks := map[int64]int{}
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, &fillProviderError{code: 400}
	}
	stats, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { return true }),
		vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
			hooks[doc]++
			return true
		}),
	)
	require.NoError(t, err)
	assert.Equal(3, calls)
	assert.Equal(map[int64]int{1: 1, 2: 1}, hooks)
	assert.Equal(2, stats.Skipped)
	assert.Zero(stats.Documents)
}

func TestFillSharedInvalidVectorRejectedWithoutProbe(t *testing.T) {
	assert := assert.New(t)
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad", 3: "later"}
	var calls, classifiers, hooks int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls++
		return [][]float32{{1}, {0}, {1}}, nil
	}
	_, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](3),
		vector.WithFillBatch[int64](vector.WithBatchSize(3)),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool {
			classifiers++
			return true
		}),
		vector.WithFillEncodeError[int64](func(doc int64, err error) bool {
			hooks++
			assert.Equal(int64(2), doc)
			var invalid *vector.InvalidVectorError
			require.ErrorAs(t, err, &invalid)
			assert.Equal(0, invalid.Chunk)
			return false
		}),
	)
	require.Error(t, err)
	assert.Equal(1, calls)
	assert.Zero(classifiers)
	assert.Equal(1, hooks)
}

func TestFillSharedInvalidVectorNilHookRejectsWithoutProbe(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad"}
	var calls, classifiers int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return [][]float32{{1}, {0}}, nil
	}
	_, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { classifiers++; return true }),
	)
	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Zero(t, classifiers)
}

func TestFillSharedInvalidVectorRecoversOnlyOtherSlices(t *testing.T) {
	assert := assert.New(t)
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad", 3: "later"}
	var calls [][]string
	hooks := map[int64]int{}
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		calls = append(calls, append([]string(nil), texts...))
		out := make([][]float32, len(texts))
		for i, text := range texts {
			out[i] = []float32{1}
			if text == "bad" {
				out[i] = []float32{0}
			}
		}
		return out, nil
	}
	stats, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](3),
		vector.WithFillBatch[int64](vector.WithBatchSize(3)),
		vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
			hooks[doc]++
			return true
		}),
	)
	require.NoError(t, err)
	assert.Equal([][]string{{"good", "bad", "later"}, {"good"}, {"later"}}, calls)
	assert.Equal(map[int64]int{2: 1}, hooks)
	assert.Equal(2, stats.Documents)
	assert.Equal(1, stats.Skipped)
}

func TestFillSharedInvalidRecoveryFailureUsesProbeRules(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newMemStore()
	store.content = map[int64]string{1: "bad", 2: "neighbor"}
	var calls, classifiers int
	hooks := map[int64]int{}
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			return [][]float32{{0}, {1}}, nil
		}
		return nil, &fillProviderError{code: 400}
	}
	_, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { classifiers++; return true }),
		vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
			hooks[doc]++
			return doc == 1
		}),
	)
	require.Error(err)
	var providerErr *fillProviderError
	require.ErrorAs(err, &providerErr)
	assert.Equal(2, calls)
	assert.Zero(classifiers, "recovery failures are never reclassified")
	assert.Equal(map[int64]int{1: 1, 2: 1}, hooks)
}

func TestFillSharedInvalidVectorOutOfRangeIsFatal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newMemStore()
	store.content = map[int64]string{1: "one", 2: "two"}
	var calls, classifiers, hooks int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		return nil, &vector.InvalidVectorError{Chunk: 2, Component: -1, Reason: "zero norm"}
	}
	_, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { classifiers++; return true }),
		vector.WithFillEncodeError[int64](func(int64, error) bool { hooks++; return true }),
	)
	require.ErrorContains(err, "invalid vector chunk 2 outside batch of 2 chunks")
	var invalid *vector.InvalidVectorError
	require.ErrorAs(err, &invalid)
	assert.Equal(1, calls)
	assert.Zero(classifiers)
	assert.Zero(hooks)
}

func TestFillSharedInvalidVectorPreservesCompanionCauses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newMemStore()
	store.content = map[int64]string{1: "good", 2: "bad"}
	providerErr := &fillProviderError{code: 422}
	sentinel := errors.New("companion sentinel")
	enc := func(context.Context, []string) ([][]float32, error) {
		return nil, errors.Join(
			&vector.InvalidVectorError{Chunk: 1, Component: -1, Reason: "zero norm"},
			providerErr,
			sentinel,
		)
	}
	_, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
		vector.WithFillEncodeError[int64](func(doc int64, err error) bool {
			assert.Equal(int64(2), doc)
			var invalid *vector.InvalidVectorError
			require.ErrorAs(err, &invalid)
			assert.Equal(0, invalid.Chunk)
			var gotProvider *fillProviderError
			require.ErrorAs(err, &gotProvider)
			assert.Same(providerErr, gotProvider)
			assert.ErrorIs(err, sentinel)
			return false
		}),
	)
	require.Error(err)
	var invalid *vector.InvalidVectorError
	require.ErrorAs(err, &invalid)
	assert.Equal(0, invalid.Chunk)
	var gotProvider *fillProviderError
	require.ErrorAs(err, &gotProvider)
	assert.Same(providerErr, gotProvider)
	assert.ErrorIs(err, sentinel)
}

func TestFillRejectedProbeBackpressuresAndCancelsWorkers(t *testing.T) {
	require := require.New(t)
	store := newMemStore()
	store.content = map[int64]string{
		1: "one", 2: "two", 3: "three", 4: "four", 5: "five", 6: "six",
	}
	ctx, cancel := context.WithCancel(t.Context())
	secondStarted := make(chan struct{})
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	secondCanceled := make(chan struct{})
	thirdStarted := make(chan struct{})
	var releaseProbeOnce sync.Once
	release := func() { releaseProbeOnce.Do(func() { close(releaseProbe) }) }
	enc := func(ctx context.Context, texts []string) ([][]float32, error) {
		switch fmt.Sprint(texts) {
		case "[one two]":
			select {
			case <-secondStarted:
				return nil, &fillProviderError{code: 400}
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "[three four]":
			close(secondStarted)
			<-ctx.Done()
			close(secondCanceled)
			return nil, ctx.Err()
		case "[one]":
			close(probeStarted)
			select {
			case <-releaseProbe:
				return nil, &fillProviderError{code: 400}
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "[five six]":
			close(thirdStarted)
			return [][]float32{{1}, {1}}, nil
		default:
			return nil, fmt.Errorf("unexpected texts %q", texts)
		}
	}

	done := make(chan error, 1)
	fillReturned := make(chan struct{})
	go func() {
		defer close(fillReturned)
		_, err := vector.Fill(ctx, store, 7, enc,
			vector.WithFillScanBatch[int64](6),
			vector.WithFillBatch[int64](vector.WithBatchSize(2)),
			vector.WithFillConcurrency[int64](2),
			vector.WithFillBatchErrorIsolation[int64](func(error) bool { return true }),
			vector.WithFillEncodeError[int64](func(int64, error) bool { return false }),
		)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-fillReturned:
		case <-time.After(5 * time.Second):
			assert.Fail(t, "Fill goroutine did not stop during cleanup")
		}
	})

	select {
	case <-probeStarted:
	case <-time.After(5 * time.Second):
		require.FailNow("collector did not start the first probe")
	}
	select {
	case <-thirdStarted:
		require.FailNow("failed-result worker started a third job during collection")
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: shows the failed-result worker starts no third job while the probe is held; an absence has no event to wait on
	}
	release()
	select {
	case err := <-done:
		require.Error(err)
	case <-time.After(5 * time.Second):
		require.FailNow("Fill did not abort after hook rejection")
	}
	select {
	case <-secondCanceled:
	case <-time.After(5 * time.Second):
		require.FailNow("in-flight worker did not observe cancellation")
	}
	select {
	case <-thirdStarted:
		assert.Fail(t, "third job started after collection rejected the failure")
	default:
	}
}

func TestFillLateSharedFailureFiltersDecidedDocument(t *testing.T) {
	assert := assert.New(t)
	store := newMemStore()
	store.content = map[int64]string{1: "abc", 2: "d"}
	releaseShared := make(chan struct{})
	var hookCalls, classifierCalls atomic.Int32
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		switch fmt.Sprint(texts) {
		case "[a b]":
			return nil, &fillProviderError{code: 400}
		case "[c d]":
			<-releaseShared
			return nil, &fillProviderError{code: 400}
		case "[d]":
			return [][]float32{{1}}, nil
		default:
			return nil, fmt.Errorf("unexpected texts %q", texts)
		}
	}
	stats, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillSplit[int64](vector.SplitOptions{MaxRunes: 1}),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
		vector.WithFillConcurrency[int64](2),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool {
			classifierCalls.Add(1)
			return true
		}),
		vector.WithFillEncodeError[int64](func(doc int64, _ error) bool {
			hookCalls.Add(1)
			assert.Equal(int64(1), doc)
			close(releaseShared)
			return true
		}),
	)
	require.NoError(t, err)
	assert.Equal(int32(1), hookCalls.Load())
	assert.Equal(int32(1), classifierCalls.Load())
	assert.Equal(1, stats.Skipped)
	assert.Equal(1, stats.Documents)
	assert.True(store.embedded[2][7])
}

func TestFillWrappedProbeDeadlineAbortsWithoutHook(t *testing.T) {
	store := newMemStore()
	store.content = map[int64]string{1: "one", 2: "two"}
	var calls, hooks int
	enc := func(context.Context, []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			return nil, &fillProviderError{code: 400}
		}
		return nil, fmt.Errorf("encoder timeout: %w", context.DeadlineExceeded)
	}
	_, err := vector.Fill(t.Context(), store, 7, enc,
		vector.WithFillScanBatch[int64](2),
		vector.WithFillBatch[int64](vector.WithBatchSize(2)),
		vector.WithFillBatchErrorIsolation[int64](func(error) bool { return true }),
		vector.WithFillEncodeError[int64](func(int64, error) bool { hooks++; return true }),
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 2, calls)
	assert.Zero(t, hooks)
}

func TestFillBatchClassifierExclusions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   map[int64]string
		batchSize int
		encodeErr error
		wantHook  int
	}{
		{
			name:      "single document",
			content:   map[int64]string{1: "one"},
			batchSize: 2,
			encodeErr: &fillProviderError{code: 400},
			wantHook:  1,
		},
		{
			name:      "shared cancellation",
			content:   map[int64]string{1: "one", 2: "two"},
			batchSize: 2,
			encodeErr: fmt.Errorf("stopped: %w", context.Canceled),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			store := newMemStore()
			store.content = tc.content
			var classifiers, hooks int
			_, err := vector.Fill(t.Context(), store, 7,
				func(context.Context, []string) ([][]float32, error) { return nil, tc.encodeErr },
				vector.WithFillScanBatch[int64](len(tc.content)),
				vector.WithFillBatch[int64](vector.WithBatchSize(tc.batchSize)),
				vector.WithFillBatchErrorIsolation[int64](func(error) bool { classifiers++; return true }),
				vector.WithFillEncodeError[int64](func(int64, error) bool { hooks++; return true }),
			)
			if errors.Is(tc.encodeErr, context.Canceled) {
				require.ErrorIs(err, context.Canceled)
			} else {
				require.NoError(err)
			}
			assert.Zero(classifiers)
			assert.Equal(tc.wantHook, hooks)
		})
	}
}
