package openssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateProbeFailed  = "probe_failed"
	StateStopping     = "stopping"
	StateDisconnected = "disconnected"
	StateError        = "error"
)

// Generation identifies one reserved connection lifecycle operation.
type Generation uint64

// Event is a lifecycle notification for one route identity. StateError can
// report a failed operation while the authoritative connection state remains
// available for recovery.
type Event struct {
	Identity   string
	Generation Generation
	State      string
	Message    string
}

// RunSSH executes an ssh argv without the executable name.
type RunSSH func(context.Context, []string) (exitCode int, err error)

// PersistentConfig controls daemon-owned, noninteractive ControlMasters.
type PersistentConfig struct {
	IdleTimeout             time.Duration
	RunSSH                  RunSSH
	OnEvent                 func(Event)
	ConnectionOptions       *ConnectionOptions
	EstablishTimeout        time.Duration
	EstablishPollInterval   time.Duration
	CleanupTimeout          time.Duration
	IdleCheckInterval       time.Duration
	MaximumControlPathBytes int
}

type hostEntry struct {
	mu               sync.Mutex
	state            string
	target           Target
	runSSH           RunSSH
	message          string
	lastActive       time.Time
	generation       Generation
	operationDone    chan struct{}
	events           []Event
	eventDispatching bool
}

type connectionSnapshot struct {
	state      string
	target     Target
	runSSH     RunSSH
	generation Generation
}

// masterDrainError means OpenSSH acknowledged the exit command but the
// manager could not yet prove that the control socket was released.
type masterDrainError struct {
	err error
}

func (e *masterDrainError) Error() string { return e.err.Error() }
func (e *masterDrainError) Unwrap() error { return e.err }

// PersistentManager owns adoptable noninteractive ControlMasters. It is
// intended for daemon lifecycles; app-session supervision is deliberately a
// separate concern.
type PersistentManager struct {
	socketDir string
	config    PersistentConfig

	mu    sync.Mutex
	hosts map[string]*hostEntry
}

// NewPersistentManager constructs a manager without touching the filesystem.
// Relative socket directories are anchored to the constructor's working
// directory so every injected SSH runner receives the same absolute path.
func NewPersistentManager(
	socketDir string,
	config PersistentConfig,
) (*PersistentManager, error) {
	if socketDir == "" {
		return nil, &PathError{Reason: "empty control directory"}
	}
	absoluteSocketDir, err := filepath.Abs(socketDir)
	if err != nil {
		return nil, &PathError{Path: socketDir, Reason: "resolve absolute directory: " + err.Error()}
	}
	if _, err := literalControlPath(absoluteSocketDir); err != nil {
		return nil, err
	}
	if config.RunSSH == nil {
		config.RunSSH = runSSH
	}
	if config.ConnectionOptions == nil {
		defaults := DefaultConnectionOptions()
		config.ConnectionOptions = &defaults
	} else {
		options := *config.ConnectionOptions
		config.ConnectionOptions = &options
	}
	if err := validateConnectionOptions(*config.ConnectionOptions, ""); err != nil {
		return nil, err
	}
	if config.EstablishTimeout <= 0 {
		config.EstablishTimeout = 10 * time.Second
	}
	if config.EstablishPollInterval <= 0 {
		config.EstablishPollInterval = 250 * time.Millisecond
	}
	if config.CleanupTimeout <= 0 {
		config.CleanupTimeout = 2 * time.Second
	}
	if config.IdleCheckInterval <= 0 {
		config.IdleCheckInterval = 30 * time.Second
	}
	if config.MaximumControlPathBytes <= 0 {
		config.MaximumControlPathBytes = 103
	}
	return &PersistentManager{
		socketDir: absoluteSocketDir,
		config:    config,
		hosts:     make(map[string]*hostEntry),
	}, nil
}

func (m *PersistentManager) host(identity string, create bool) *hostEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.hosts[identity]
	if entry == nil && create {
		entry = &hostEntry{state: StateDisconnected}
		m.hosts[identity] = entry
	}
	return entry
}

func beginOperation(entry *hostEntry) (Generation, chan struct{}) {
	entry.generation++
	done := make(chan struct{})
	entry.operationDone = done
	return entry.generation, done
}

func snapshotEntry(entry *hostEntry) connectionSnapshot {
	return connectionSnapshot{
		state:      entry.state,
		target:     entry.target,
		runSSH:     entry.runSSH,
		generation: entry.generation,
	}
}

func entryMatches(entry *hostEntry, snapshot connectionSnapshot) bool {
	return entry.operationDone == nil &&
		entry.state == snapshot.state &&
		entry.target == snapshot.target &&
		entry.generation == snapshot.generation
}

func activeState(state string) bool {
	return state == StateConnected || state == StateProbeFailed
}

// SocketPath returns the deterministic path for a route identity and target.
// Connect reports a PathError before use when the configured directory is too
// long.
func (m *PersistentManager) SocketPath(identity string, target Target) string {
	path, _ := ControlPath(m.socketDir, controlNameForTarget(identity, target), 0)
	return path
}

func (m *PersistentManager) checkedSocketPath(
	identity string,
	target Target,
) (string, error) {
	return ControlPath(
		m.socketDir,
		controlNameForTarget(identity, target),
		m.config.MaximumControlPathBytes,
	)
}

// ConnectionArguments returns explicit client multiplexing arguments for the
// expected connection generation.
func (m *PersistentManager) ConnectionArguments(
	identity string,
	generation Generation,
) ([]string, error) {
	entry := m.host(identity, false)
	if entry == nil {
		return nil, ErrConnectionChanged
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.generation != generation || entry.operationDone != nil {
		return nil, ErrConnectionChanged
	}
	if !activeState(entry.state) {
		return ClientArguments("")
	}
	socketPath, err := m.checkedSocketPath(identity, entry.target)
	if err != nil {
		return nil, err
	}
	return ClientArguments(socketPath)
}

// Connect establishes or adopts the master for identity.
func (m *PersistentManager) Connect(
	ctx context.Context,
	identity string,
	target Target,
) (Generation, error) {
	return m.connect(ctx, identity, target, m.config.RunSSH)
}

// ConnectWithRunner establishes or adopts the master using runSSH. The runner
// is retained with the resulting generation and is used for every subsequent
// probe and teardown of that master. If identity already has a live generation,
// its retained runner remains authoritative.
func (m *PersistentManager) ConnectWithRunner(
	ctx context.Context,
	identity string,
	target Target,
	runSSH RunSSH,
) (Generation, error) {
	if runSSH == nil {
		return 0, &ConfigError{Destination: target.String(), Reason: "nil SSH runner"}
	}
	return m.connect(ctx, identity, target, runSSH)
}

func (m *PersistentManager) connect(
	ctx context.Context,
	identity string,
	target Target,
	runSSH RunSSH,
) (Generation, error) {
	if err := persistentSupportError(); err != nil {
		return 0, err
	}
	if identity == "" {
		return 0, &ConfigError{Destination: target.String(), Reason: "empty connection identity"}
	}
	if err := ValidateTarget(target); err != nil {
		return 0, err
	}
	socketPath, err := m.checkedSocketPath(identity, target)
	if err != nil {
		return 0, err
	}
	if err := ensurePersistentDirectory(m.socketDir); err != nil {
		return 0, err
	}
	entry := m.host(identity, true)
	if entry == nil {
		return 0, fmt.Errorf("OpenSSH host state for %q was not created", identity)
	}

	for {
		entry.mu.Lock()
		if done := entry.operationDone; done != nil {
			entry.mu.Unlock()
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-done:
				continue
			}
		}
		snapshot := snapshotEntry(entry)
		if snapshot.state == StateStopping {
			generation, done := beginOperation(entry)
			stoppingTarget := snapshot.target
			entry.mu.Unlock()

			stoppingPath, pathErr := m.checkedSocketPath(identity, stoppingTarget)
			drainErr := pathErr
			if drainErr == nil {
				drainErr = m.drainStoppedMaster(ctx, stoppingPath)
			}
			m.finishTeardown(identity, entry, generation, done, drainErr)
			if drainErr != nil {
				return 0, drainErr
			}
			continue
		}
		if snapshot.target.Hostname != "" && snapshot.target != target {
			generation, done := beginOperation(entry)
			oldTarget := snapshot.target
			entry.mu.Unlock()

			oldSocketPath, pathErr := m.checkedSocketPath(identity, oldTarget)
			teardownErr := pathErr
			if teardownErr == nil {
				teardownErr = m.stopMaster(ctx, oldSocketPath, oldTarget, snapshot.runSSH)
			}
			m.finishTeardown(identity, entry, generation, done, teardownErr)
			if teardownErr != nil {
				return 0, teardownErr
			}
			continue
		}
		probeTarget := target
		probeRunner := runSSH
		if activeState(snapshot.state) && snapshot.target.Hostname != "" {
			probeTarget = snapshot.target
		}
		if snapshot.target.Hostname != "" && snapshot.runSSH != nil {
			probeRunner = snapshot.runSSH
		}
		entry.mu.Unlock()

		probeState, probeErr := m.probeControlMasterWithRunner(
			ctx, socketPath, probeTarget, probeRunner,
		)
		entry.mu.Lock()
		if !entryMatches(entry, snapshot) {
			entry.mu.Unlock()
			continue
		}
		if probeErr != nil {
			entry.mu.Unlock()
			return 0, probeErr
		}

		if probeState == probeAlive {
			if activeState(snapshot.state) && snapshot.target == target {
				recovered := snapshot.state == StateProbeFailed
				entry.state = StateConnected
				entry.message = ""
				entry.lastActive = time.Now()
				generation := entry.generation
				entry.mu.Unlock()
				if recovered {
					m.emit(identity, generation, StateConnected, "")
				}
				return generation, nil
			}
			generation, done := beginOperation(entry)
			entry.state = StateConnected
			entry.target = target
			entry.runSSH = probeRunner
			entry.message = ""
			entry.lastActive = time.Now()
			entry.operationDone = nil
			close(done)
			entry.mu.Unlock()
			m.emit(identity, generation, StateConnected, "")
			return generation, nil
		}

		generation, done := beginOperation(entry)
		entry.state = StateConnecting
		entry.target = target
		entry.runSSH = runSSH
		entry.message = ""
		entry.lastActive = time.Now()
		entry.mu.Unlock()
		m.emit(identity, generation, StateConnecting, "")

		if probeState == probeStale {
			if removeErr := removeControlSocket(socketPath); removeErr != nil {
				return 0, m.finishStart(identity, entry, generation, done, target, removeErr)
			}
		}
		if startErr := m.startMaster(ctx, socketPath, target, runSSH); startErr != nil {
			return 0, m.finishStart(identity, entry, generation, done, target, startErr)
		}
		if finishErr := m.finishStart(identity, entry, generation, done, target, nil); finishErr != nil {
			return 0, finishErr
		}
		return generation, nil
	}
}

func (m *PersistentManager) startMaster(
	ctx context.Context,
	socketPath string,
	target Target,
	runSSH RunSSH,
) error {
	arguments, err := MasterArguments(
		socketPath,
		target,
		*m.config.ConnectionOptions,
	)
	if err != nil {
		return err
	}
	exitCode, runErr := runSSH(ctx, arguments)
	if runErr != nil || exitCode != 0 {
		primary := &CommandError{
			Operation:   "master start",
			Destination: target.String(),
			ExitCode:    exitCode,
			Err:         runErr,
		}
		if ctx.Err() != nil {
			primary.Err = errors.Join(ctx.Err(), runErr)
		}
		return m.cleanupFailedStart(socketPath, target, runSSH, primary)
	}

	establishCtx, cancel := context.WithTimeout(ctx, m.config.EstablishTimeout)
	defer cancel()
	ticker := time.NewTicker(m.config.EstablishPollInterval)
	defer ticker.Stop()
	for {
		probeState, probeErr := m.probeControlMasterWithRunner(
			establishCtx, socketPath, target, runSSH,
		)
		if probeErr != nil {
			primary := probeErr
			if establishCtx.Err() != nil {
				primary = establishmentContextError(ctx, probeErr)
			}
			return m.cleanupFailedStart(socketPath, target, runSSH, primary)
		}
		if probeState == probeAlive {
			return nil
		}
		select {
		case <-establishCtx.Done():
			return m.cleanupFailedStart(
				socketPath,
				target,
				runSSH,
				establishmentContextError(ctx, nil),
			)
		case <-ticker.C:
		}
	}
}

func establishmentContextError(ctx context.Context, probeErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, probeErr)
	}
	timeoutErr := fmt.Errorf("timeout waiting for control master: %w", context.DeadlineExceeded)
	return errors.Join(timeoutErr, probeErr)
}

func (m *PersistentManager) cleanupFailedStart(
	socketPath string,
	target Target,
	runSSH RunSSH,
	primary error,
) error {
	cleanupCtx, cancel := context.WithTimeout(
		context.Background(),
		m.config.CleanupTimeout,
	)
	defer cancel()
	cleanupErr := m.cleanupFailedStartMaster(cleanupCtx, socketPath, target, runSSH)
	if cleanupErr == nil {
		return primary
	}
	return errors.Join(primary, cleanupErr)
}

// cleanupFailedStartMaster observes an initially absent path for the full
// cleanup window because ssh -MNf can return before the detached master binds
// its socket. The caller retains the lifecycle reservation until this returns.
func (m *PersistentManager) cleanupFailedStartMaster(
	ctx context.Context,
	socketPath string,
	target Target,
	runSSH RunSSH,
) error {
	present, err := m.observeFailedStartSocket(ctx, socketPath)
	if err != nil || !present {
		return err
	}
	// Observation may spend the cleanup window, so give verified teardown its
	// own bounded window to stop and drain the master.
	return m.stopMaster(context.Background(), socketPath, target, runSSH)
}

// observeFailedStartSocket reports whether a control socket appears before ctx
// ends, ending with an ownership and type inspection once ctx expires.
func (m *PersistentManager) observeFailedStartSocket(ctx context.Context, socketPath string) (bool, error) {
	ticker := time.NewTicker(m.config.EstablishPollInterval)
	defer ticker.Stop()
	for {
		dialState, err := inspectControlSocket(ctx, socketPath)
		if err == nil && dialState != socketAbsent {
			return true, nil
		}
		// A dial cut short by the expiring window still gets the final
		// inspection, so a master that already bound is not left running.
		if err != nil && ctx.Err() == nil {
			return false, err
		}
		select {
		case <-ctx.Done():
			return validateControlSocket(socketPath)
		case <-ticker.C:
		}
	}
}

func (m *PersistentManager) finishStart(
	identity string,
	entry *hostEntry,
	generation Generation,
	done chan struct{},
	target Target,
	err error,
) error {
	stopping := false
	entry.mu.Lock()
	if entry.generation == generation && entry.operationDone == done {
		if err == nil {
			entry.state = StateConnected
			entry.message = ""
		} else {
			if _, ok := errors.AsType[*masterDrainError](err); ok {
				entry.state = StateStopping
				stopping = true
			} else {
				entry.state = StateError
			}
			entry.message = err.Error()
		}
		entry.target = target
		entry.lastActive = time.Now()
		entry.operationDone = nil
		close(done)
	}
	entry.mu.Unlock()
	if err == nil {
		m.emit(identity, generation, StateConnected, "")
		return nil
	}
	if stopping {
		m.emit(identity, generation, StateStopping, err.Error())
	} else {
		m.emit(identity, generation, StateError, err.Error())
	}
	return err
}

// Disconnect tears down one tracked master. Unknown identities are an
// idempotent, filesystem-free no-op.
func (m *PersistentManager) Disconnect(ctx context.Context, identity string) error {
	entry := m.host(identity, false)
	if entry == nil {
		return nil
	}
	for {
		entry.mu.Lock()
		if done := entry.operationDone; done != nil {
			entry.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				continue
			}
		}
		if entry.state == StateDisconnected {
			entry.mu.Unlock()
			return nil
		}
		target := entry.target
		runSSH := entry.runSSH
		stopping := entry.state == StateStopping
		generation, done := beginOperation(entry)
		entry.mu.Unlock()

		socketPath, pathErr := m.checkedSocketPath(identity, target)
		if pathErr == nil {
			pathErr = ensurePersistentDirectory(m.socketDir)
		}
		teardownErr := pathErr
		if teardownErr == nil {
			if stopping {
				teardownErr = m.drainStoppedMaster(ctx, socketPath)
			} else {
				teardownErr = m.stopMaster(ctx, socketPath, target, runSSH)
			}
		}
		m.finishTeardown(identity, entry, generation, done, teardownErr)
		return teardownErr
	}
}

func (m *PersistentManager) finishTeardown(
	identity string,
	entry *hostEntry,
	generation Generation,
	done chan struct{},
	err error,
) {
	stopping := false
	entry.mu.Lock()
	if entry.generation == generation && entry.operationDone == done {
		if err == nil {
			entry.state = StateDisconnected
			entry.target = Target{}
			entry.runSSH = nil
			entry.message = ""
			entry.lastActive = time.Now()
		} else {
			if _, ok := errors.AsType[*masterDrainError](err); ok {
				entry.state = StateStopping
				entry.message = err.Error()
				stopping = true
			}
		}
		entry.operationDone = nil
		close(done)
	}
	entry.mu.Unlock()
	if err == nil {
		m.emit(identity, generation, StateDisconnected, "")
	} else if stopping {
		m.emit(identity, generation, StateStopping, err.Error())
	} else {
		m.emitError(identity, generation, err.Error())
	}
}

func (m *PersistentManager) stopMaster(
	ctx context.Context,
	socketPath string,
	target Target,
	runSSH RunSSH,
) error {
	stopCtx, cancel := context.WithTimeout(ctx, m.config.CleanupTimeout)
	defer cancel()

	dialState, err := inspectControlSocket(stopCtx, socketPath)
	if err != nil {
		return err
	}
	switch dialState {
	case socketAbsent:
		return nil
	case socketStale:
		return removeControlSocket(socketPath)
	default:
	}
	arguments, err := ExitArguments(socketPath, target)
	if err != nil {
		return err
	}
	exitCode, runErr := runSSH(stopCtx, arguments)
	if runErr != nil || exitCode != 0 {
		if stopCtx.Err() != nil {
			runErr = errors.Join(stopCtx.Err(), runErr)
		}
		return &CommandError{
			Operation:   "master exit",
			Destination: target.String(),
			ExitCode:    exitCode,
			Err:         runErr,
		}
	}
	if err := m.waitForMasterExit(stopCtx, socketPath); err != nil {
		return &masterDrainError{err: err}
	}
	return nil
}

func (m *PersistentManager) drainStoppedMaster(
	ctx context.Context,
	socketPath string,
) error {
	drainCtx, cancel := context.WithTimeout(ctx, m.config.CleanupTimeout)
	defer cancel()
	if err := m.waitForMasterExit(drainCtx, socketPath); err != nil {
		return &masterDrainError{err: err}
	}
	return nil
}

func (m *PersistentManager) waitForMasterExit(
	ctx context.Context,
	socketPath string,
) error {
	ticker := time.NewTicker(m.config.EstablishPollInterval)
	defer ticker.Stop()
	for {
		dialState, err := inspectControlSocket(ctx, socketPath)
		if err != nil {
			return err
		}
		switch dialState {
		case socketAbsent:
			return nil
		case socketStale:
			return removeControlSocket(socketPath)
		default:
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for OpenSSH master exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func removeControlSocket(socketPath string) error {
	exists, validateErr := validateControlSocket(socketPath)
	if validateErr != nil {
		return validateErr
	}
	if !exists {
		return nil
	}
	err := os.Remove(socketPath)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove OpenSSH control socket: %w", err)
}

// IsAlive reports whether the expected connection generation answers ssh -O
// check. The probe runs without holding the lifecycle lock.
func (m *PersistentManager) IsAlive(
	ctx context.Context,
	identity string,
	generation Generation,
) (bool, error) {
	if err := ensurePersistentDirectory(m.socketDir); err != nil {
		return false, err
	}
	entry := m.host(identity, false)
	if entry == nil {
		return false, nil
	}
	entry.mu.Lock()
	if entry.generation != generation || entry.operationDone != nil ||
		!activeState(entry.state) {
		entry.mu.Unlock()
		return false, ErrConnectionChanged
	}
	target := entry.target
	runSSH := entry.runSSH
	entry.mu.Unlock()
	probeState, probeErr := m.probeControlMasterWithRunner(
		ctx, m.SocketPath(identity, target), target, runSSH,
	)

	entry.mu.Lock()
	changed := entry.generation != generation || entry.operationDone != nil ||
		!activeState(entry.state)
	entry.mu.Unlock()
	if changed {
		if probeErr != nil {
			return false, errors.Join(ErrConnectionChanged, probeErr)
		}
		return false, ErrConnectionChanged
	}
	if probeErr != nil {
		return false, probeErr
	}
	return probeState == probeAlive, nil
}

func (m *PersistentManager) State(identity string) string {
	entry := m.host(identity, false)
	if entry == nil {
		return StateDisconnected
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.state
}

func (m *PersistentManager) Destination(identity string) string {
	entry := m.host(identity, false)
	if entry == nil {
		return ""
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.target.String()
}

// TouchActivity records touch-before-use activity for the expected generation.
// It may return false after idle teardown has already reserved the lifecycle.
func (m *PersistentManager) TouchActivity(identity string, generation Generation) bool {
	entry := m.host(identity, false)
	if entry == nil {
		return false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.generation != generation || entry.operationDone != nil ||
		!activeState(entry.state) {
		return false
	}
	entry.lastActive = time.Now()
	return true
}

// SetProbeFailed publishes a probe failure only for the expected generation.
func (m *PersistentManager) SetProbeFailed(
	identity string,
	generation Generation,
	message string,
) bool {
	entry := m.host(identity, false)
	if entry == nil {
		return false
	}
	entry.mu.Lock()
	if entry.generation != generation || entry.operationDone != nil ||
		!activeState(entry.state) {
		entry.mu.Unlock()
		return false
	}
	entry.state = StateProbeFailed
	entry.message = message
	entry.mu.Unlock()
	m.emit(identity, generation, StateProbeFailed, message)
	return true
}

// StartIdleMonitor disconnects idle masters until ctx ends.
func (m *PersistentManager) StartIdleMonitor(ctx context.Context) {
	if m.config.IdleTimeout <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(m.config.IdleCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.disconnectIdle(ctx)
			}
		}
	}()
}

func (m *PersistentManager) disconnectIdle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	idleBefore := time.Now().Add(-m.config.IdleTimeout)
	m.mu.Lock()
	identities := make([]string, 0, len(m.hosts))
	for identity := range m.hosts {
		identities = append(identities, identity)
	}
	m.mu.Unlock()
	for _, identity := range identities {
		if ctx.Err() != nil {
			return
		}
		_ = m.disconnectIdleCandidate(ctx, identity, idleBefore)
	}
}

func (m *PersistentManager) disconnectIdleCandidate(
	ctx context.Context,
	identity string,
	idleBefore time.Time,
) error {
	entry := m.host(identity, false)
	if entry == nil {
		return nil
	}
	entry.mu.Lock()
	if entry.operationDone != nil || !activeState(entry.state) ||
		!entry.lastActive.Before(idleBefore) {
		entry.mu.Unlock()
		return nil
	}
	if err := ctx.Err(); err != nil {
		entry.mu.Unlock()
		return err
	}
	target := entry.target
	runSSH := entry.runSSH
	generation, done := beginOperation(entry)
	entry.mu.Unlock()

	socketPath, err := m.checkedSocketPath(identity, target)
	if err == nil {
		err = ensurePersistentDirectory(m.socketDir)
	}
	if err == nil {
		err = m.stopMaster(ctx, socketPath, target, runSSH)
	}
	m.finishTeardown(identity, entry, generation, done, err)
	return err
}

func (m *PersistentManager) emit(
	identity string,
	generation Generation,
	state, message string,
) {
	if m.config.OnEvent == nil {
		return
	}
	entry := m.host(identity, false)
	if entry == nil {
		return
	}
	entry.mu.Lock()
	current := entry.generation == generation && entry.state == state && entry.message == message
	if !current {
		entry.mu.Unlock()
		return
	}
	startDispatch := enqueueEventLocked(entry, Event{
		Identity: identity, Generation: generation, State: state, Message: message,
	})
	entry.mu.Unlock()
	if startDispatch {
		go m.dispatchEvents(entry)
	}
}

// emitError reports a failed lifecycle operation while leaving the recorded
// connection state and target authoritative for recovery and retry.
func (m *PersistentManager) emitError(
	identity string,
	generation Generation,
	message string,
) {
	if m.config.OnEvent == nil {
		return
	}
	entry := m.host(identity, false)
	if entry == nil {
		return
	}
	entry.mu.Lock()
	if entry.generation != generation || entry.operationDone != nil {
		entry.mu.Unlock()
		return
	}
	startDispatch := enqueueEventLocked(entry, Event{
		Identity: identity, Generation: generation, State: StateError, Message: message,
	})
	entry.mu.Unlock()
	if startDispatch {
		go m.dispatchEvents(entry)
	}
}

// enqueueEventLocked retains only the latest occurrence of each state for the
// current generation. Removing an older occurrence before appending its
// replacement preserves the order of the latest pending transitions and
// bounds the queue by the package's finite set of lifecycle states.
func enqueueEventLocked(entry *hostEntry, event Event) bool {
	kept := 0
	for _, pending := range entry.events {
		if pending.Generation != event.Generation || pending.State == event.State {
			continue
		}
		entry.events[kept] = pending
		kept++
	}
	clear(entry.events[kept:])
	entry.events = append(entry.events[:kept], event)
	if entry.eventDispatching {
		return false
	}
	entry.eventDispatching = true
	return true
}

func (m *PersistentManager) dispatchEvents(entry *hostEntry) {
	for {
		entry.mu.Lock()
		if len(entry.events) == 0 {
			entry.eventDispatching = false
			entry.mu.Unlock()
			return
		}
		event := entry.events[0]
		entry.events[0] = Event{}
		entry.events = entry.events[1:]
		current := entry.generation == event.Generation
		entry.mu.Unlock()
		if current {
			m.config.OnEvent(event)
		}
	}
}

func runSSH(ctx context.Context, arguments []string) (int, error) {
	return runSSHCommand(ctx, "ssh", arguments)
}

func runSSHCommand(
	ctx context.Context,
	executable string,
	arguments []string,
) (int, error) {
	cmd := exec.CommandContext(ctx, executable, arguments...)
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return -1, errors.Join(contextErr, err)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), err
	}
	return -1, err
}
