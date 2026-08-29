package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

func TestDaemonLeaseEnsuresInitialClientOnce(t *testing.T) {
	want := client.New("http://unused.invalid", "synthetic-key")
	var ensures atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		ensures.Add(1)
		return want, nil
	}, func(*client.Client) error { return nil })

	for range 2 {
		got, err := daemonRead(t.Context(), lease, func(c *client.Client) (*client.Client, error) {
			return c, nil
		})
		require.NoError(t, err)
		assert.Same(t, want, got)
	}
	assert.Equal(t, int32(1), ensures.Load())
}

func TestDaemonLeaseRejectsForbiddenEffectiveKeyOnInitialAcquisitionWithoutLeak(t *testing.T) {
	const forbidden = "synthetic-forbidden-mcp-bearer"
	current := client.New("http://unused.invalid", forbidden)
	closed := newClosedClients()
	var ensures atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		ensures.Add(1)
		return current, nil
	}, closed.close)
	require.NoError(t, lease.bindAPIKeyExclusion(client.NewAPIKeyExclusionPolicy(forbidden)))
	called := false

	_, err := daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
		called = true
		return struct{}{}, nil
	})

	require.ErrorIs(t, err, errDaemonCredentialReuse)
	assert.Equal(t, errDaemonCredentialReuse.Error(), err.Error())
	assert.False(t, called)
	assert.Equal(t, int32(1), ensures.Load())
	require.Eventually(t, func() bool { return closed.count(current) == 1 }, time.Second, time.Millisecond)
	forbiddenHash := sha256.Sum256([]byte(forbidden))
	formatted := fmt.Sprintf("%v | %+v | %#v | lease=%#v", err, err, err, lease)
	assert.NotContains(t, formatted, forbidden)
	assert.NotContains(t, formatted, hex.EncodeToString(forbiddenHash[:]))
}

func TestDaemonLeaseAllowsEffectiveKeyDifferentFromForbiddenBearer(t *testing.T) {
	want := client.New("http://unused.invalid", "independent-daemon-key")
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return want, nil
	}, func(*client.Client) error { return nil })
	require.NoError(t, lease.bindAPIKeyExclusion(
		client.NewAPIKeyExclusionPolicy("synthetic-mcp-bearer")))

	got, err := daemonRead(t.Context(), lease, func(c *client.Client) (*client.Client, error) {
		return c, nil
	})

	require.NoError(t, err)
	assert.Same(t, want, got)
}

func TestDaemonLeaseRejectsForbiddenKeyAfterIdleReacquisition(t *testing.T) {
	const forbidden = "synthetic-forbidden-mcp-bearer"
	first := client.New("http://unused.invalid", "independent-daemon-key")
	forbiddenReplacement := client.New("http://unused.invalid", forbidden)
	clients := []*client.Client{first, forbiddenReplacement}
	var ensures atomic.Int32
	closed := newClosedClients()
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		index := int(ensures.Add(1) - 1)
		require.Less(t, index, len(clients))
		return clients[index], nil
	}, closed.close)
	require.NoError(t, lease.bindAPIKeyExclusion(client.NewAPIKeyExclusionPolicy(forbidden)))

	current, err := lease.acquire(t.Context())
	require.NoError(t, err)
	lease.discard(current) // Model an idle daemon connection becoming invalid.
	_, err = daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
		return struct{}{}, errors.New("forbidden replacement must not dispatch")
	})

	require.ErrorIs(t, err, errDaemonCredentialReuse)
	assert.Equal(t, int32(2), ensures.Load())
	assert.Equal(t, 1, closed.count(first))
	require.Eventually(t, func() bool {
		return closed.count(forbiddenReplacement) == 1
	}, time.Second, time.Millisecond)
}

func TestDaemonReadRejectsForbiddenKeyOnTransportReplacement(t *testing.T) {
	const forbidden = "synthetic-forbidden-mcp-bearer"
	old := disconnectedDaemonClient(t)
	forbiddenReplacement := client.New("http://unused.invalid", forbidden)
	clients := []*client.Client{old, forbiddenReplacement}
	var ensures atomic.Int32
	closed := newClosedClients()
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		index := int(ensures.Add(1) - 1)
		require.Less(t, index, len(clients))
		return clients[index], nil
	}, closed.close)
	require.NoError(t, lease.bindAPIKeyExclusion(client.NewAPIKeyExclusionPolicy(forbidden)))
	calls := 0

	_, err := daemonRead(t.Context(), lease, func(c *client.Client) (string, error) {
		calls++
		return "", c.Health(t.Context())
	})

	require.ErrorIs(t, err, errDaemonCredentialReuse)
	assert.Equal(t, 1, calls, "a rejected replacement must never receive the retried read")
	assert.Equal(t, int32(2), ensures.Load())
	assert.Equal(t, 1, closed.count(old))
	require.Eventually(t, func() bool {
		return closed.count(forbiddenReplacement) == 1
	}, time.Second, time.Millisecond)
}

func TestDaemonLeaseSharesForbiddenKeyAcquisitionFailureAcrossWaiters(t *testing.T) {
	const (
		workers   = 16
		forbidden = "synthetic-forbidden-mcp-bearer"
	)
	old := disconnectedDaemonClient(t)
	forbiddenReplacement := client.New("http://unused.invalid", forbidden)
	clients := []*client.Client{old, forbiddenReplacement}
	closed := newClosedClients()
	var ensures atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		index := int(ensures.Add(1) - 1)
		require.Less(t, index, len(clients))
		return clients[index], nil
	}, closed.close)
	require.NoError(t, lease.bindAPIKeyExclusion(client.NewAPIKeyExclusionPolicy(forbidden)))
	_, err := daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
		return struct{}{}, nil
	})
	require.NoError(t, err)

	release := make(chan struct{})
	var arrived atomic.Int32
	var releaseOnce sync.Once
	errorsByWorker := make(chan error, workers)
	for range workers {
		go func() {
			_, readErr := daemonRead(t.Context(), lease, func(c *client.Client) (struct{}, error) {
				if c != old {
					return struct{}{}, errors.New("forbidden replacement reached dispatch")
				}
				if arrived.Add(1) == workers {
					releaseOnce.Do(func() { close(release) })
				}
				<-release
				return struct{}{}, c.Health(t.Context())
			})
			errorsByWorker <- readErr
		}()
	}
	for range workers {
		workerErr := <-errorsByWorker
		require.ErrorIs(t, workerErr, errDaemonCredentialReuse)
		assert.Equal(t, errDaemonCredentialReuse.Error(), workerErr.Error())
	}
	assert.Equal(t, int32(2), ensures.Load(), "all waiters must share one rejected replacement")
	assert.Equal(t, 1, closed.count(old))
	require.Eventually(t, func() bool {
		return closed.count(forbiddenReplacement) == 1
	}, time.Second, time.Millisecond)
}

func TestDaemonLeaseRecoversFromStaleRuntimeOnNextSafeCall(t *testing.T) {
	want := client.New("http://unused.invalid", "synthetic-key")
	var ensures atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		if ensures.Add(1) == 1 {
			return nil, fmt.Errorf("stale /private/vault/runtime with key synthetic-secret: %w",
				client.ErrTransientDaemonAcquisition)
		}
		return want, nil
	}, func(*client.Client) error { return nil })

	called := false
	_, err := daemonRead(t.Context(), lease, func(*client.Client) (string, error) {
		called = true
		return "", nil
	})
	require.ErrorIs(t, err, errDaemonUnavailable)
	assert.Equal(t, errDaemonUnavailable.Error(), err.Error())
	assert.NotContains(t, err.Error(), "synthetic-secret")
	assert.NotContains(t, err.Error(), "/private/vault")
	assert.False(t, called)

	got, err := daemonRead(t.Context(), lease, func(c *client.Client) (*client.Client, error) {
		return c, nil
	})
	require.NoError(t, err)
	assert.Same(t, want, got)
	assert.Equal(t, int32(2), ensures.Load())
}

func TestDaemonLeaseRecoversSafeReadsAfterDaemonLoss(t *testing.T) {
	for _, cause := range []string{"daemon restart", "idle shutdown"} {
		t.Run(cause, func(t *testing.T) {
			old := disconnectedDaemonClient(t)
			replacement := client.New("http://unused.invalid", "replacement-key")
			clients := []*client.Client{old, replacement}
			var ensures atomic.Int32
			closed := newClosedClients()
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				index := int(ensures.Add(1) - 1)
				require.Less(t, index, len(clients))
				return clients[index], nil
			}, closed.close)

			calls := 0
			got, err := daemonRead(t.Context(), lease, func(c *client.Client) (string, error) {
				calls++
				if c == old {
					return "", c.Health(t.Context())
				}
				return "recovered", nil
			})
			require.NoError(t, err)
			assert.Equal(t, "recovered", got)
			assert.Equal(t, 2, calls)
			assert.Equal(t, int32(2), ensures.Load())
			assert.Equal(t, 1, closed.count(old))
			assert.Zero(t, closed.count(replacement))
		})
	}
}

func TestDaemonLeaseConcurrentFailuresCreateOneReplacement(t *testing.T) {
	const workers = 16
	old := disconnectedDaemonClient(t)
	replacement := client.New("http://unused.invalid", "replacement-key")
	var ensures atomic.Int32
	closed := newClosedClients()
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		if ensures.Add(1) == 1 {
			return old, nil
		}
		return replacement, nil
	}, closed.close)

	_, err := daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
		return struct{}{}, nil
	})
	require.NoError(t, err)

	release := make(chan struct{})
	var arrived atomic.Int32
	var releaseOnce sync.Once
	var calls atomic.Int32
	errorsByWorker := make(chan error, workers)
	for range workers {
		go func() {
			_, readErr := daemonRead(t.Context(), lease, func(c *client.Client) (string, error) {
				calls.Add(1)
				if c == old {
					if arrived.Add(1) == workers {
						releaseOnce.Do(func() { close(release) })
					}
					<-release
					return "", c.Health(t.Context())
				}
				return "recovered", nil
			})
			errorsByWorker <- readErr
		}()
	}
	for range workers {
		require.NoError(t, <-errorsByWorker)
	}
	assert.Equal(t, int32(2), ensures.Load(), "all callers must share one replacement")
	assert.Equal(t, int32(workers*2), calls.Load())
	assert.Equal(t, 1, closed.count(old))
	assert.Zero(t, closed.count(replacement))
}

func TestDaemonLeaseCancellationDoesNotReacquireOrReplay(t *testing.T) {
	t.Run("before acquisition", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var ensures atomic.Int32
		lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
			ensures.Add(1)
			return nil, errors.New("must not be called")
		}, func(*client.Client) error { return nil })

		_, err := daemonRead(ctx, lease, func(*client.Client) (struct{}, error) {
			return struct{}{}, errors.New("must not be called")
		})
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, ensures.Load())
	})

	t.Run("during request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		current := client.New("http://unused.invalid", "synthetic-key")
		var ensures atomic.Int32
		closed := newClosedClients()
		lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
			ensures.Add(1)
			return current, nil
		}, closed.close)

		calls := 0
		_, err := daemonRead(ctx, lease, func(c *client.Client) (struct{}, error) {
			calls++
			cancel()
			return struct{}{}, c.Health(ctx)
		})
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, calls)
		assert.Equal(t, int32(1), ensures.Load())
		assert.Zero(t, closed.count(current))
	})
}

func TestDaemonLeaseCancellationPropagatesWhileAnotherCallerEnsures(t *testing.T) {
	ensureStarted := make(chan struct{})
	releaseEnsure := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseEnsure) }) })
	current := client.New("http://unused.invalid", "synthetic-key")
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		close(ensureStarted)
		<-releaseEnsure
		return current, nil
	}, func(*client.Client) error { return nil })

	firstDone := make(chan error, 1)
	go func() {
		_, err := daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
			return struct{}{}, nil
		})
		firstDone <- err
	}()
	<-ensureStarted

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	secondDone := make(chan error, 1)
	go func() {
		_, err := daemonRead(ctx, lease, func(*client.Client) (struct{}, error) {
			return struct{}{}, errors.New("canceled caller must not run")
		})
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(250 * time.Millisecond):
		releaseOnce.Do(func() { close(releaseEnsure) })
		<-firstDone
		<-secondDone
		t.Fatal("canceled caller remained blocked behind daemon acquisition")
	}
	releaseOnce.Do(func() { close(releaseEnsure) })
	require.NoError(t, <-firstDone)
}

func TestDaemonLeaseInitiatorCancellationDoesNotCancelSharedAcquisition(t *testing.T) {
	ensureStarted := make(chan struct{})
	releaseEnsure := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseEnsure) }) })
	current := client.New("http://unused.invalid", "synthetic-key")
	var ensures atomic.Int32
	var startedOnce sync.Once
	lease := newDaemonLeaseWith(func(ctx context.Context) (*client.Client, error) {
		ensures.Add(1)
		startedOnce.Do(func() { close(ensureStarted) })
		select {
		case <-releaseEnsure:
			return current, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, func(*client.Client) error { return nil })

	initiatorCtx, cancelInitiator := context.WithCancel(t.Context())
	initiatorDone := make(chan error, 1)
	go func() {
		_, err := daemonRead(initiatorCtx, lease, func(*client.Client) (struct{}, error) {
			return struct{}{}, errors.New("canceled initiator must not run")
		})
		initiatorDone <- err
	}()
	<-ensureStarted

	waiterDone := make(chan error, 1)
	go func() {
		got, err := daemonRead(t.Context(), lease, func(c *client.Client) (*client.Client, error) {
			return c, nil
		})
		if err == nil && got != current {
			err = errors.New("healthy waiter received the wrong client")
		}
		waiterDone <- err
	}()
	cancelInitiator()

	select {
	case err := <-initiatorDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(250 * time.Millisecond):
		t.Fatal("canceled acquisition initiator did not return promptly")
	}
	releaseOnce.Do(func() { close(releaseEnsure) })
	require.NoError(t, <-waiterDone)
	assert.Equal(t, int32(1), ensures.Load(), "waiters must share one acquisition")
}

func TestDaemonLeaseAcquisitionUsesBoundedLeaseContext(t *testing.T) {
	const timeout = 25 * time.Millisecond
	var ensures atomic.Int32
	lease := newDaemonLeaseWithAcquisitionContext(
		func(ctx context.Context) (*client.Client, error) {
			ensures.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		func(*client.Client) error { return nil },
		func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), timeout)
		},
	)

	done := make(chan error, 1)
	go func() {
		_, err := daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
			return struct{}{}, errors.New("timed-out acquisition must not run the request")
		})
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, errDaemonUnavailable)
		assert.Equal(t, errDaemonUnavailable.Error(), err.Error())
	case <-time.After(250 * time.Millisecond):
		t.Fatal("daemon acquisition exceeded its lease-owned timeout")
	}
	assert.Equal(t, int32(1), ensures.Load())
}

func TestDaemonLeaseSharesAcquisitionFailure(t *testing.T) {
	ensureStarted := make(chan struct{})
	releaseEnsure := make(chan struct{})
	var ensures atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		ensures.Add(1)
		close(ensureStarted)
		<-releaseEnsure
		return nil, errors.New("synthetic acquisition failure")
	}, func(*client.Client) error { return nil })

	initiatorDone := make(chan error, 1)
	go func() {
		_, err := daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
			return struct{}{}, errors.New("failed acquisition must not run the request")
		})
		initiatorDone <- err
	}()
	<-ensureStarted
	lease.mu.Lock()
	acquiring := lease.acquiring
	lease.mu.Unlock()
	require.NotNil(t, acquiring)
	waiterDone := make(chan error, 1)
	go func() {
		_, err := waitForDaemonAcquisition(t.Context(), acquiring)
		waiterDone <- err
	}()
	close(releaseEnsure)

	for _, err := range []error{<-initiatorDone, <-waiterDone} {
		require.ErrorIs(t, err, errDaemonUnavailable)
		assert.Equal(t, errDaemonUnavailable.Error(), err.Error())
	}
	assert.Equal(t, int32(1), ensures.Load())
}

func TestDaemonReadRetriesAtMostOnce(t *testing.T) {
	first := disconnectedDaemonClient(t)
	second := disconnectedDaemonClient(t)
	recovered := client.New("http://unused.invalid", "recovered-key")
	clients := []*client.Client{first, second, recovered}
	var ensures atomic.Int32
	closed := newClosedClients()
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		index := int(ensures.Add(1) - 1)
		require.Less(t, index, len(clients))
		return clients[index], nil
	}, closed.close)

	calls := 0
	_, err := daemonRead(t.Context(), lease, func(c *client.Client) (string, error) {
		calls++
		return "", c.Health(t.Context())
	})
	require.ErrorIs(t, err, errDaemonRequestFailed)
	assert.Equal(t, errDaemonRequestFailed.Error(), err.Error())
	assert.Equal(t, 2, calls)
	assert.Equal(t, int32(2), ensures.Load())
	assert.Equal(t, 1, closed.count(first))
	assert.Equal(t, 1, closed.count(second))

	got, err := daemonRead(t.Context(), lease, func(c *client.Client) (string, error) {
		assert.Same(t, recovered, c)
		return "recovered later", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "recovered later", got)
	assert.Equal(t, int32(3), ensures.Load())
}

func TestDaemonReadNeverRetriesAfterResponse(t *testing.T) {
	tests := []struct {
		name      string
		newClient func(*testing.T) *client.Client
		read      func(context.Context, *client.Client) error
		check     func(*testing.T, error)
	}{
		{
			name: "domain response",
			newClient: func(t *testing.T) *client.Client {
				t.Helper()
				return problemDaemonClient(t, "not_found", "synthetic private document data")
			},
			read: func(ctx context.Context, c *client.Client) error { return c.Health(ctx) },
			check: func(t *testing.T, err error) {
				t.Helper()
				facts, ok := daemonProblemFacts(err)
				assert.True(t, ok)
				assert.Equal(t, "not_found", facts.Code)
				assert.ErrorIs(t, err, store.ErrNotFound)
			},
		},
		{
			name: "partial successful response",
			newClient: func(t *testing.T) *client.Client {
				t.Helper()
				server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
					response.Header().Set("Content-Type", "application/json")
					_, _ = response.Write([]byte(`{"id":`))
				}))
				t.Cleanup(server.Close)
				return client.New(server.URL, "synthetic-key")
			},
			read: func(ctx context.Context, c *client.Client) error {
				_, err := c.Node(ctx, 1)
				return err
			},
			check: func(t *testing.T, err error) {
				t.Helper()
				assert.False(t, client.IsResponseDecodeError(err), "raw response errors must not cross MCP")
				assert.NoError(t, errors.Unwrap(err))
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			current := testCase.newClient(t)
			var ensures atomic.Int32
			closed := newClosedClients()
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				ensures.Add(1)
				return current, nil
			}, closed.close)
			calls := 0

			_, err := daemonRead(t.Context(), lease, func(c *client.Client) (struct{}, error) {
				calls++
				return struct{}{}, testCase.read(t.Context(), c)
			})
			require.ErrorIs(t, err, errDaemonRequestFailed)
			assert.Equal(t, errDaemonRequestFailed.Error(), err.Error())
			assert.NotContains(t, err.Error(), "synthetic private document data")
			testCase.check(t, err)
			assert.Equal(t, 1, calls)
			assert.Equal(t, int32(1), ensures.Load())
			assert.Zero(t, closed.count(current))
		})
	}
}

func TestDaemonBoundaryCopiesSafeProblemFactsWithoutRawCause(t *testing.T) {
	tests := []struct {
		name         string
		code         string
		observed     int
		mapped       error
		wantObserved int
	}{
		{name: "mapped domain error", code: "not_found", mapped: store.ErrNotFound},
		{name: "unknown problem", code: "synthetic_unknown"},
		{name: "scope overflow", code: "scope_too_large", observed: 4097, wantObserved: 4097},
		{name: "one-byte code", code: "a"},
		{name: "64-byte code", code: "a0_" + strings.Repeat("b", 61)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			current := problemDaemonClientWithObserved(t, testCase.code,
				"synthetic-secret /private/vault private-document", testCase.observed)
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				return current, nil
			}, func(*client.Client) error { return nil })

			_, err := daemonRead(t.Context(), lease, func(c *client.Client) (struct{}, error) {
				return struct{}{}, c.Health(t.Context())
			})
			require.ErrorIs(t, err, errDaemonRequestFailed)
			facts, ok := daemonProblemFacts(err)
			require.True(t, ok)
			assert.Equal(t, testCase.code, facts.Code)
			assert.Equal(t, testCase.wantObserved, facts.ObservedScopeCount)
			if testCase.mapped == nil {
				assert.NoError(t, errors.Unwrap(err))
				assert.NoError(t, facts.MappedError)
			} else {
				require.ErrorIs(t, err, testCase.mapped)
				assert.Same(t, testCase.mapped, errors.Unwrap(err))
				assert.Same(t, testCase.mapped, facts.MappedError)
				require.NoError(t, errors.Unwrap(errors.Unwrap(err)))
			}
			var rawOverflow *client.SourceFenceScopeTooLargeError
			assert.NotErrorAs(t, err, &rawOverflow, "raw detailed client errors must not cross MCP")
			_, rawProblemVisible := client.ProblemCode(err)
			assert.False(t, rawProblemVisible, "client problem wrappers must not cross MCP")
			assertSafeDaemonFormatting(t, err)
		})
	}
}

func TestDaemonBoundaryDropsUnsafeProblemCodes(t *testing.T) {
	tests := []struct {
		name   string
		code   string
		marker string
	}{
		{name: "slash path", code: "a/private/path", marker: "private/path"},
		{name: "spaces", code: "a secret detail", marker: "secret detail"},
		{name: "newline", code: "a\nnewline_secret", marker: "newline_secret"},
		{name: "control", code: "a\x01control_secret", marker: "control_secret"},
		{name: "uppercase", code: "Uppercase_secret", marker: "Uppercase_secret"},
		{name: "non ASCII", code: "aéunicode_secret", marker: "unicode_secret"},
		{name: "empty"},
		{name: "leading digit", code: "1leading_secret", marker: "leading_secret"},
		{name: "over 64 bytes", code: "a" + strings.Repeat("b", 58) + "over64", marker: "over64"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			current := problemDaemonClientWithObserved(t, testCase.code,
				"synthetic-secret /private/vault private-document", 4097)
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				return current, nil
			}, func(*client.Client) error { return nil })

			_, err := daemonRead(t.Context(), lease, func(c *client.Client) (struct{}, error) {
				return struct{}{}, c.Health(t.Context())
			})
			require.ErrorIs(t, err, errDaemonRequestFailed)
			facts, ok := daemonProblemFacts(err)
			assert.False(t, ok)
			assert.Equal(t, client.ProblemFacts{}, facts)
			require.NoError(t, errors.Unwrap(err))
			_, rawProblemVisible := client.ProblemCode(err)
			assert.False(t, rawProblemVisible)
			assertSafeDaemonFormatting(t, err)
			if testCase.marker != "" {
				formatted := fmt.Sprintf("%v | %+v | %#v | unwrap=%v | unwrap=%#v",
					err, err, err, errors.Unwrap(err), errors.Unwrap(err))
				assert.NotContains(t, formatted, testCase.marker)
			}
		})
	}
}

type sensitiveDaemonFailureError struct{ detail string }

func (failure *sensitiveDaemonFailureError) Error() string { return failure.detail }

func TestDaemonBoundaryDropsRawAcquisitionCause(t *testing.T) {
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		return nil, &sensitiveDaemonFailureError{detail: "synthetic-secret /private/vault private-document"}
	}, func(*client.Client) error { return nil })

	_, err := daemonRead(t.Context(), lease, func(*client.Client) (struct{}, error) {
		return struct{}{}, errors.New("failed acquisition must not run the request")
	})
	require.ErrorIs(t, err, errDaemonUnavailable)
	require.NoError(t, errors.Unwrap(err))
	var recovered *sensitiveDaemonFailureError
	assert.NotErrorAs(t, err, &recovered)
	_, ok := daemonProblemFacts(err)
	assert.False(t, ok)
	assertSafeDaemonFormatting(t, err)
}

func assertSafeDaemonFormatting(t *testing.T, err error) {
	t.Helper()
	formatted := fmt.Sprintf("%v | %+v | %#v | unwrap=%v | unwrap=%#v",
		err, err, err, errors.Unwrap(err), errors.Unwrap(err))
	assert.NotContains(t, formatted, "synthetic-secret")
	assert.NotContains(t, formatted, "/private/vault")
	assert.NotContains(t, formatted, "private-document")
}

func TestDaemonProcessingStartNeverReplaysAmbiguousOutcome(t *testing.T) {
	tests := []struct {
		name      string
		newClient func(*testing.T) *client.Client
	}{
		{name: "failure before response", newClient: disconnectedDaemonClient},
		{name: "truncated response", newClient: func(t *testing.T) *client.Client {
			t.Helper()
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = response.Write([]byte(`{"sequence":1`))
			}))
			t.Cleanup(server.Close)
			return client.New(server.URL, "synthetic-key")
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			current := testCase.newClient(t)
			var ensures atomic.Int32
			closed := newClosedClients()
			lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
				ensures.Add(1)
				return current, nil
			}, closed.close)
			require.NoError(t, lease.bindAPIKeyExclusion(
				client.NewAPIKeyExclusionPolicy("independent-mcp-bearer")))
			calls := 0

			_, err := daemonProcessingStart(t.Context(), lease, func(c *client.Client) (api.ProcessingJob, error) {
				calls++
				return c.StartProcessing(t.Context(), api.StartProcessingRequest{})
			})
			require.ErrorIs(t, err, errProcessingOutcomeUnknown)
			assert.Equal(t, errProcessingOutcomeUnknown.Error(), err.Error())
			assert.Equal(t, 1, calls)
			assert.Equal(t, int32(1), ensures.Load())
			assert.Equal(t, 1, closed.count(current))
		})
	}
}

func TestDaemonProcessingStartPreservesDefiniteDomainResponse(t *testing.T) {
	current := problemDaemonClient(t, "processing_consent_required", "synthetic consent detail")
	var ensures atomic.Int32
	closed := newClosedClients()
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		ensures.Add(1)
		return current, nil
	}, closed.close)
	calls := 0

	_, err := daemonProcessingStart(t.Context(), lease, func(c *client.Client) (api.ProcessingJob, error) {
		calls++
		return c.StartProcessing(t.Context(), api.StartProcessingRequest{})
	})
	require.ErrorIs(t, err, errDaemonRequestFailed)
	require.NotErrorIs(t, err, errProcessingOutcomeUnknown)
	facts, ok := daemonProblemFacts(err)
	assert.True(t, ok)
	assert.Equal(t, "processing_consent_required", facts.Code)
	assert.Equal(t, 1, calls)
	assert.Equal(t, int32(1), ensures.Load())
	assert.Zero(t, closed.count(current))
}

func disconnectedDaemonClient(t *testing.T) *client.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			t.Error("synthetic server cannot hijack connection")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijacking synthetic connection: %v", err)
			return
		}
		_ = connection.Close()
	}))
	t.Cleanup(server.Close)
	return client.New(server.URL, "synthetic-key")
}

func problemDaemonClient(t *testing.T, code, detail string) *client.Client {
	t.Helper()
	return problemDaemonClientWithObserved(t, code, detail, 0)
}

func problemDaemonClientWithObserved(t *testing.T, code, detail string, observed int) *client.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/problem+json")
		response.WriteHeader(http.StatusUnprocessableEntity)
		err := json.MarshalWrite(response, api.Error{
			Title: "Unprocessable Entity", Status: http.StatusUnprocessableEntity,
			Code: code, Detail: detail, ObservedScopeCount: observed,
		})
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return client.New(server.URL, "synthetic-key")
}

type closedClients struct {
	mu     sync.Mutex
	counts map[*client.Client]int
}

func newClosedClients() *closedClients {
	return &closedClients{counts: make(map[*client.Client]int)}
}

func (closed *closedClients) close(c *client.Client) error {
	closed.mu.Lock()
	defer closed.mu.Unlock()
	closed.counts[c]++
	return nil
}

func (closed *closedClients) count(c *client.Client) int {
	closed.mu.Lock()
	defer closed.mu.Unlock()
	return closed.counts[c]
}
