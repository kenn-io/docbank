package report

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	// DefaultBudgetBytes bounds retained evidence and artifacts for one report cache.
	DefaultBudgetBytes = 1 << 30
	// DefaultTimeout bounds one daemon report creation or revision operation.
	DefaultTimeout = 60 * time.Second
)

var (
	ErrBudgetExhausted = errors.New("report resource budget exhausted")
	ErrBudgetClosed    = errors.New("report resource budget closed")
)

// Budget charges retained allocations to one shared ceiling. Child scopes own
// their reservations so a canceled build can release them in one Close call.
type Budget interface {
	Reserve(ctx context.Context, bytes int64) (release func(), err error)
	Child() Budget
	Used() int64
	Close() error
}

type budgetState struct {
	mu     sync.Mutex
	limit  int64
	used   int64
	closed bool
	scopes map[*budgetScope]struct{}
}

type budgetScope struct {
	state        *budgetState
	root         bool
	closed       bool
	reservations map[*reservation]struct{}
}

type reservation struct {
	bytes  int64
	active bool
}

// NewBudget constructs the root of a shared byte budget.
func NewBudget(limit int64) Budget {
	if limit < 0 {
		limit = 0
	}
	state := &budgetState{limit: limit, scopes: make(map[*budgetScope]struct{})}
	root := &budgetScope{state: state, root: true, reservations: make(map[*reservation]struct{})}
	state.scopes[root] = struct{}{}
	return root
}

func (scope *budgetScope) Reserve(ctx context.Context, size int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, errors.New("negative report reservation")
	}
	state := scope.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.closed || scope.closed {
		return nil, ErrBudgetClosed
	}
	if size > state.limit-state.used {
		return nil, ErrBudgetExhausted
	}
	handle := &reservation{bytes: size, active: true}
	scope.reservations[handle] = struct{}{}
	state.used += size
	return func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		if !handle.active {
			return
		}
		handle.active = false
		state.used -= handle.bytes
		delete(scope.reservations, handle)
	}, nil
}

func (scope *budgetScope) Child() Budget {
	state := scope.state
	state.mu.Lock()
	defer state.mu.Unlock()
	child := &budgetScope{
		state: state, closed: state.closed || scope.closed,
		reservations: make(map[*reservation]struct{}),
	}
	if !child.closed {
		state.scopes[child] = struct{}{}
	}
	return child
}

func (scope *budgetScope) Used() int64 {
	state := scope.state
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.used
}

func (scope *budgetScope) Close() error {
	state := scope.state
	state.mu.Lock()
	defer state.mu.Unlock()
	if scope.closed {
		return nil
	}
	if scope.root {
		state.closed = true
		for child := range state.scopes {
			closeBudgetScopeLocked(child)
		}
		state.scopes = nil
		return nil
	}
	closeBudgetScopeLocked(scope)
	delete(state.scopes, scope)
	return nil
}

func closeBudgetScopeLocked(scope *budgetScope) {
	if scope.closed {
		return
	}
	scope.closed = true
	for handle := range scope.reservations {
		if handle.active {
			handle.active = false
			scope.state.used -= handle.bytes
		}
		delete(scope.reservations, handle)
	}
}
