package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

const (
	snapshotIDBytes             = 16
	querySnapshotMaxBuilders    = 2
	querySnapshotMaxCachedRows  = int64(1_000_000)
	querySnapshotMaxHandles     = 4096
	querySnapshotMaxOwnerHandle = 8
	querySnapshotIdleTTL        = 15 * time.Minute
	querySnapshotAbsoluteTTL    = 30 * time.Minute
	querySnapshotGCInterval     = time.Minute
	querySnapshotHandleOverhead = int64(2*maxSnapshotCursorBytes + 1024)
)

var (
	ErrSnapshotGone       = errors.New("query snapshot is gone")
	ErrSnapshotAdmission  = errors.New("query snapshot cache capacity is unavailable")
	ErrSnapshotBusy       = errors.New("query snapshot builders are busy")
	ErrSnapshotMemberHash = errors.New("query snapshot member hash does not match")
)

// SnapshotPage carries complete frozen metadata and one caller-owned row page.
type SnapshotPage struct {
	SnapshotProjection

	Snapshot   bool      `json:"snapshot"`
	SnapshotID string    `json:"snapshot_id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	PrevCursor string    `json:"prev_cursor"`
	NextCursor string    `json:"next_cursor"`
}

type snapshotCacheLimits struct {
	MaxRows         int64
	MaxBytes        int64
	MaxHandles      int
	MaxOwnerHandles int
	MaxBuilders     int
	IdleTTL         time.Duration
	AbsoluteTTL     time.Duration
	GCInterval      time.Duration
}

func (limits snapshotCacheLimits) withDefaults() snapshotCacheLimits {
	if limits.MaxRows == 0 {
		limits.MaxRows = querySnapshotMaxCachedRows
	}
	if limits.MaxBytes == 0 {
		limits.MaxBytes = querySnapshotMaxBytes
	}
	if limits.MaxHandles == 0 {
		limits.MaxHandles = querySnapshotMaxHandles
	}
	if limits.MaxOwnerHandles == 0 {
		limits.MaxOwnerHandles = querySnapshotMaxOwnerHandle
	}
	if limits.MaxBuilders == 0 {
		limits.MaxBuilders = querySnapshotMaxBuilders
	}
	if limits.IdleTTL == 0 {
		limits.IdleTTL = querySnapshotIdleTTL
	}
	if limits.AbsoluteTTL == 0 {
		limits.AbsoluteTTL = querySnapshotAbsoluteTTL
	}
	if limits.GCInterval == 0 {
		limits.GCInterval = querySnapshotGCInterval
	}
	return limits
}

type querySnapshotServiceOptions struct {
	Now                      func() time.Time
	Rand                     io.Reader
	HMACKey                  []byte
	Limits                   snapshotCacheLimits
	ChargeHook               func(context.Context, string, int64, int64) error
	SavedRunBeforeCommitHook func(context.Context) error
	SavedRunCommittedHook    func()
}

type cachedQuerySnapshot struct {
	id              string
	owner           string
	metadata        []byte
	rows            [][]byte
	created         time.Time
	lastUsed        time.Time
	used            uint64
	bytes           int64
	serializedBytes int64
}

type querySnapshotBuild struct {
	id            uint64
	snapshotID    string
	owner         string
	cancel        context.CancelFunc
	reservedRows  int64
	reservedBytes int64
	invalidated   bool
	finalizing    bool
}

type preparedQuerySnapshot struct {
	ctx        context.Context
	cancel     context.CancelFunc
	build      *querySnapshotBuild
	projection SnapshotProjection
	cached     *cachedQuerySnapshot
	page       SnapshotPage
}

type snapshotCacheStats struct {
	CachedHandles   int64
	ReservedHandles int64
	CachedRows      int64
	ReservedRows    int64
	CachedBytes     int64
	ReservedBytes   int64
	ActiveBuilders  int64
}

// QuerySnapshotService owns bounded, session-scoped frozen query results.
type QuerySnapshotService struct {
	store *Store

	mu            sync.Mutex
	randMu        sync.Mutex
	snapshots     map[string]*cachedQuerySnapshot
	builds        map[uint64]*querySnapshotBuild
	ownerHandles  map[string]int
	serial        uint64
	nextBuildID   uint64
	cachedRows    int64
	reservedRows  int64
	cachedBytes   int64
	reservedBytes int64
	closed        bool

	now                      func() time.Time
	random                   io.Reader
	hmacKey                  []byte
	limits                   snapshotCacheLimits
	chargeHook               func(context.Context, string, int64, int64) error
	savedRunBeforeCommitHook func(context.Context) error
	savedRunCommittedHook    func()

	stopCancel context.CancelFunc
	stopOnce   sync.Once
	doneOnce   sync.Once
	done       chan struct{}
	wg         sync.WaitGroup
}

func NewQuerySnapshotService(store *Store) *QuerySnapshotService {
	return newQuerySnapshotService(store, querySnapshotServiceOptions{})
}

func newQuerySnapshotService(store *Store, options querySnapshotServiceOptions) *QuerySnapshotService {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Rand == nil {
		options.Rand = rand.Reader
	}
	if len(options.HMACKey) == 0 {
		options.HMACKey = make([]byte, 32)
		if _, err := io.ReadFull(options.Rand, options.HMACKey); err != nil {
			panic(fmt.Sprintf("generating query snapshot cursor key: %v", err))
		}
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	service := &QuerySnapshotService{
		store: store, snapshots: make(map[string]*cachedQuerySnapshot),
		builds: make(map[uint64]*querySnapshotBuild), ownerHandles: make(map[string]int),
		now: options.Now, random: options.Rand, hmacKey: slices.Clone(options.HMACKey),
		limits: options.Limits.withDefaults(), chargeHook: options.ChargeHook,
		savedRunBeforeCommitHook: options.SavedRunBeforeCommitHook,
		savedRunCommittedHook:    options.SavedRunCommittedHook,
		stopCancel:               stopCancel, done: make(chan struct{}),
	}
	service.wg.Add(1)
	go service.gc(stopCtx)
	return service
}

func (s *QuerySnapshotService) Create(
	ctx context.Context, owner string, request SnapshotRequest,
) (SnapshotPage, error) {
	prepared, err := s.prepareSnapshot(ctx, owner, request, nil)
	if err != nil {
		return SnapshotPage{}, err
	}
	published := false
	defer func() {
		prepared.cancel()
		s.finishBuild(prepared.build, published)
	}()
	if err := s.publishBuild(prepared.build, prepared.cached); err != nil {
		return SnapshotPage{}, err
	}
	published = true
	return prepared.page, nil
}

// RunSaved materializes the inspected saved definition and durably records its
// comparison receipt before making the already-reserved snapshot visible.
func (s *QuerySnapshotService) RunSaved(
	ctx context.Context, owner, id string, expectedRevision int64, request SnapshotRequest,
) (SavedQueryRun, SnapshotPage, error) {
	if err := validateUUIDv4(id); err != nil || expectedRevision < 1 {
		return SavedQueryRun{}, SnapshotPage{}, fmt.Errorf("%w: saved query identity or revision", ErrInvalidSavedQueryRun)
	}
	prepared, err := s.prepareSnapshot(ctx, owner, request, &savedQuerySnapshotInput{
		ID: id, ExpectedRevision: expectedRevision,
	})
	if err != nil {
		return SavedQueryRun{}, SnapshotPage{}, err
	}
	published := false
	defer func() {
		prepared.cancel()
		s.finishBuild(prepared.build, published)
	}()
	if err := s.beginSavedRunFinalization(prepared); err != nil {
		return SavedQueryRun{}, SnapshotPage{}, err
	}
	if s.savedRunBeforeCommitHook != nil {
		if err := s.savedRunBeforeCommitHook(prepared.ctx); err != nil {
			return SavedQueryRun{}, SnapshotPage{}, err
		}
	}
	var run SavedQueryRun
	err = s.store.withLogicalTx(prepared.ctx, func(tx *sql.Tx) error {
		var insertErr error
		run, insertErr = insertSavedQueryRunTx(
			prepared.ctx, tx, id, expectedRevision, prepared.projection, prepared.page,
		)
		return insertErr
	})
	if err != nil {
		return SavedQueryRun{}, SnapshotPage{}, err
	}
	if s.savedRunCommittedHook != nil {
		s.savedRunCommittedHook()
	}
	published = s.finalizeSavedRun(prepared.build, prepared.cached)
	return run, prepared.page, nil
}

func (s *QuerySnapshotService) prepareSnapshot(
	ctx context.Context, owner string, request SnapshotRequest, saved *savedQuerySnapshotInput,
) (preparedQuerySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return preparedQuerySnapshot{}, err
	}
	if s.store == nil {
		return preparedQuerySnapshot{}, errors.New("query snapshot store is unavailable")
	}
	if owner == "" {
		return preparedQuerySnapshot{}, errors.New("query snapshot owner is required")
	}
	buildCtx, cancel := context.WithCancel(ctx)
	build, err := s.admitNewBuild(owner, cancel)
	if err != nil {
		cancel()
		return preparedQuerySnapshot{}, err
	}
	snapshotID := build.snapshotID
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
			s.finishBuild(build, false)
		}
	}()

	options := defaultSnapshotMaterializeOptions()
	options.Now = s.now
	options.SavedQuery = saved
	options.Charge = func(rows, bytes int64) error {
		if err := buildCtx.Err(); err != nil {
			return err
		}
		if err := s.chargeBuild(build, rows, bytes); err != nil {
			return err
		}
		if s.chargeHook != nil {
			return s.chargeHook(buildCtx, owner, rows, bytes)
		}
		return nil
	}
	projection, err := s.store.materializeQuerySnapshot(buildCtx, request, options)
	if err != nil {
		return preparedQuerySnapshot{}, err
	}
	if err := buildCtx.Err(); err != nil {
		return preparedQuerySnapshot{}, err
	}

	encodedRows := make([][]byte, len(projection.Rows))
	var encodedRowBytes int64
	for i := range projection.Rows {
		if err := buildCtx.Err(); err != nil {
			return preparedQuerySnapshot{}, err
		}
		encodedRows[i], err = json.Marshal(projection.Rows[i])
		if err != nil {
			return preparedQuerySnapshot{}, fmt.Errorf("encoding cached query snapshot row: %w", err)
		}
		encodedRowBytes += int64(len(encodedRows[i]))
		projection.Rows[i] = SnapshotRow{}
	}
	projection.Rows = nil
	metadata, err := json.Marshal(projection)
	if err != nil {
		return preparedQuerySnapshot{}, fmt.Errorf("encoding cached query snapshot metadata: %w", err)
	}
	actualBytes := int64(len(metadata)) + encodedRowBytes
	if actualBytes > build.reservedBytes {
		if err := s.chargeBuild(build, 0, actualBytes-build.reservedBytes); err != nil {
			return preparedQuerySnapshot{}, err
		}
	}
	if err := s.chargeBuild(build, 0, querySnapshotHandleOverhead+int64(len(owner)+len(snapshotID))); err != nil {
		return preparedQuerySnapshot{}, err
	}
	if err := buildCtx.Err(); err != nil {
		return preparedQuerySnapshot{}, err
	}
	now := s.now().UTC()
	cached := &cachedQuerySnapshot{
		id: snapshotID, owner: owner, metadata: metadata, rows: encodedRows,
		created: now, lastUsed: now, bytes: build.reservedBytes,
		serializedBytes: projection.SerializedBytes,
	}
	page, err := s.renderPage(cached, 0)
	if err != nil {
		return preparedQuerySnapshot{}, err
	}
	handedOff = true
	return preparedQuerySnapshot{
		ctx: buildCtx, cancel: cancel, build: build, projection: projection,
		cached: cached, page: page,
	}, nil
}

func (s *QuerySnapshotService) beginSavedRunFinalization(prepared preparedQuerySnapshot) error {
	if err := prepared.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active, ok := s.builds[prepared.build.id]
	if s.closed || !ok || active.invalidated {
		return context.Canceled
	}
	active.finalizing = true
	return nil
}

// finalizeSavedRun cannot fail: all encoding and capacity admission happened
// before the durable receipt transaction. Concurrent revocation discards the
// handle here while leaving the committed receipt as explicit Gone evidence.
func (s *QuerySnapshotService) finalizeSavedRun(
	build *querySnapshotBuild, cached *cachedQuerySnapshot,
) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	active, ok := s.builds[build.id]
	if !ok || s.closed || active.invalidated {
		return false
	}
	s.publishBuildLocked(build, cached)
	return true
}

func (s *QuerySnapshotService) Page(
	ctx context.Context, owner, snapshotID, cursor string,
) (SnapshotPage, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotPage{}, err
	}
	cached, err := s.lookup(owner, snapshotID)
	if err != nil {
		return SnapshotPage{}, err
	}
	payload, err := s.decodeCursor(owner, cursor)
	if err != nil {
		return SnapshotPage{}, err
	}
	var metadata SnapshotProjection
	if err := json.Unmarshal(cached.metadata, &metadata); err != nil {
		return SnapshotPage{}, fmt.Errorf("decoding cached query snapshot metadata: %w", err)
	}
	if payload.SnapshotID != snapshotID || payload.QueryFingerprint != metadata.QueryFingerprint ||
		payload.SortField != metadata.Query.Sort.Field || payload.SortDirection != metadata.Query.Sort.Direction ||
		payload.PageSize != metadata.PageSize || payload.Offset < 0 || payload.Offset >= int64(len(cached.rows)) ||
		payload.Offset%int64(metadata.PageSize) != 0 {
		return SnapshotPage{}, ErrSnapshotCursor
	}
	return s.renderPage(&cached, payload.Offset)
}

func (s *QuerySnapshotService) CopyMembers(
	ctx context.Context, owner, snapshotID, memberHash string,
) ([]SnapshotMember, error) {
	return s.CopyMembersBounded(ctx, owner, snapshotID, memberHash, int(querySnapshotMaxRows))
}

// CopyMembersBounded rejects oversized membership before allocating its copy.
func (s *QuerySnapshotService) CopyMembersBounded(ctx context.Context, owner, snapshotID, memberHash string, limit int) ([]SnapshotMember, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cached, err := s.lookup(owner, snapshotID)
	if err != nil {
		return nil, err
	}
	if limit < 1 || len(cached.rows) > limit {
		return nil, ErrQuerySnapshotTooLarge
	}
	var metadata SnapshotProjection
	if err := json.Unmarshal(cached.metadata, &metadata); err != nil {
		return nil, fmt.Errorf("decoding cached query snapshot metadata: %w", err)
	}
	if memberHash != metadata.MemberHash {
		return nil, ErrSnapshotMemberHash
	}
	members := make([]SnapshotMember, len(cached.rows))
	for i := range cached.rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var row SnapshotRow
		if err := json.Unmarshal(cached.rows[i], &row); err != nil {
			return nil, fmt.Errorf("decoding cached query snapshot member: %w", err)
		}
		members[i] = row.SnapshotMember
	}
	if int64(len(members)) != metadata.Total || snapshotMemberHash(members) != metadata.MemberHash {
		return nil, errors.New("cached query snapshot membership failed validation")
	}
	return members, nil
}

func (s *QuerySnapshotService) Revoke(owner string) {
	var cancels []context.CancelFunc
	s.mu.Lock()
	for id, cached := range s.snapshots {
		if cached.owner == owner {
			s.removeSnapshotLocked(id)
		}
	}
	for _, build := range s.builds {
		if build.owner == owner {
			build.invalidated = true
			cancels = append(cancels, build.cancel)
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *QuerySnapshotService) Close() error {
	s.startShutdown()
	<-s.done
	return nil
}

func (s *QuerySnapshotService) Shutdown(ctx context.Context) error {
	s.startShutdown()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *QuerySnapshotService) startShutdown() {
	s.stopOnce.Do(func() {
		var cancels []context.CancelFunc
		s.mu.Lock()
		s.closed = true
		for id := range s.snapshots {
			s.removeSnapshotLocked(id)
		}
		for _, build := range s.builds {
			cancels = append(cancels, build.cancel)
		}
		s.mu.Unlock()
		s.stopCancel()
		for _, cancel := range cancels {
			cancel()
		}
		go func() {
			s.wg.Wait()
			s.doneOnce.Do(func() { close(s.done) })
		}()
	})
}

func (s *QuerySnapshotService) newSnapshotID() (string, error) {
	raw := make([]byte, snapshotIDBytes)
	s.randMu.Lock()
	_, err := io.ReadFull(s.random, raw)
	s.randMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("generating query snapshot id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func (s *QuerySnapshotService) admitNewBuild(owner string, cancel context.CancelFunc) (*querySnapshotBuild, error) {
	for range 8 {
		snapshotID, err := s.newSnapshotID()
		if err != nil {
			return nil, err
		}
		build, collision, err := s.admitBuild(snapshotID, owner, cancel)
		if err != nil {
			return nil, err
		}
		if !collision {
			return build, nil
		}
	}
	return nil, errors.New("generating unique query snapshot id")
}

func (s *QuerySnapshotService) admitBuild(
	snapshotID, owner string, cancel context.CancelFunc,
) (*querySnapshotBuild, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrSnapshotGone
	}
	s.purgeExpiredLocked(s.now().UTC())
	if len(s.builds) >= s.limits.MaxBuilders {
		return nil, false, ErrSnapshotBusy
	}
	if _, exists := s.snapshots[snapshotID]; exists {
		return nil, true, nil
	}
	for _, build := range s.builds {
		if build.snapshotID == snapshotID {
			return nil, true, nil
		}
	}
	for len(s.snapshots)+len(s.builds) >= s.limits.MaxHandles ||
		s.ownerHandles[owner] >= s.limits.MaxOwnerHandles {
		ownerOnly := s.ownerHandles[owner] >= s.limits.MaxOwnerHandles
		if !s.evictLRULocked(owner, ownerOnly) {
			return nil, false, ErrSnapshotAdmission
		}
	}
	s.nextBuildID++
	build := &querySnapshotBuild{id: s.nextBuildID, snapshotID: snapshotID, owner: owner, cancel: cancel}
	s.builds[build.id] = build
	s.ownerHandles[owner]++
	s.wg.Add(1)
	return build, false, nil
}

func (s *QuerySnapshotService) chargeBuild(build *querySnapshotBuild, rows, bytes int64) error {
	if rows < 0 || bytes < 0 {
		return ErrSnapshotAdmission
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return context.Canceled
	}
	active, ok := s.builds[build.id]
	if !ok || active.invalidated {
		return context.Canceled
	}
	s.purgeExpiredLocked(s.now().UTC())
	for exceedsBound(s.cachedRows+s.reservedRows, rows, s.limits.MaxRows) ||
		exceedsBound(s.cachedBytes+s.reservedBytes, bytes, s.limits.MaxBytes) {
		if !s.evictLRULocked("", false) {
			return ErrSnapshotAdmission
		}
	}
	build.reservedRows += rows
	build.reservedBytes += bytes
	s.reservedRows += rows
	s.reservedBytes += bytes
	return nil
}

func exceedsBound(used, addition, maximum int64) bool {
	return addition > maximum || used > maximum-addition
}

func (s *QuerySnapshotService) publishBuild(build *querySnapshotBuild, cached *cachedQuerySnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return context.Canceled
	}
	active, ok := s.builds[build.id]
	if !ok || active.invalidated {
		return context.Canceled
	}
	s.publishBuildLocked(build, cached)
	return nil
}

func (s *QuerySnapshotService) publishBuildLocked(build *querySnapshotBuild, cached *cachedQuerySnapshot) {
	delete(s.builds, build.id)
	s.reservedRows -= build.reservedRows
	s.reservedBytes -= build.reservedBytes
	s.cachedRows += build.reservedRows
	s.cachedBytes += build.reservedBytes
	s.serial++
	cached.used = s.serial
	s.snapshots[cached.id] = cached
}

func (s *QuerySnapshotService) finishBuild(build *querySnapshotBuild, published bool) {
	s.mu.Lock()
	if !published {
		if _, ok := s.builds[build.id]; ok {
			delete(s.builds, build.id)
			s.reservedRows -= build.reservedRows
			s.reservedBytes -= build.reservedBytes
			s.releaseOwnerHandleLocked(build.owner)
		}
	}
	s.mu.Unlock()
	s.wg.Done()
}

func (s *QuerySnapshotService) lookup(owner, snapshotID string) (cachedQuerySnapshot, error) {
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	cached, ok := s.snapshots[snapshotID]
	if !ok || cached.owner != owner {
		return cachedQuerySnapshot{}, ErrSnapshotGone
	}
	s.serial++
	cached.used = s.serial
	cached.lastUsed = now
	return *cached, nil
}

func (s *QuerySnapshotService) renderPage(cached *cachedQuerySnapshot, start int64) (SnapshotPage, error) {
	var projection SnapshotProjection
	if err := json.Unmarshal(cached.metadata, &projection); err != nil {
		return SnapshotPage{}, fmt.Errorf("decoding cached query snapshot metadata: %w", err)
	}
	projection.SerializedBytes = cached.serializedBytes
	end := min(start+int64(projection.PageSize), int64(len(cached.rows)))
	projection.Rows = make([]SnapshotRow, end-start)
	for i := start; i < end; i++ {
		if err := json.Unmarshal(cached.rows[i], &projection.Rows[i-start]); err != nil {
			return SnapshotPage{}, fmt.Errorf("decoding cached query snapshot row: %w", err)
		}
	}
	page := SnapshotPage{
		SnapshotProjection: projection, Snapshot: true, SnapshotID: cached.id,
		CreatedAt: cached.created, ExpiresAt: s.expiry(cached),
	}
	var err error
	if start > 0 {
		previous := max(start-int64(projection.PageSize), 0)
		page.PrevCursor, err = s.encodeCursor(cached.owner, s.cursorPayload(cached, projection, "prev", previous))
		if err != nil {
			return SnapshotPage{}, err
		}
	}
	if end < int64(len(cached.rows)) {
		page.NextCursor, err = s.encodeCursor(cached.owner, s.cursorPayload(cached, projection, "next", end))
		if err != nil {
			return SnapshotPage{}, err
		}
	}
	return page, nil
}

func (s *QuerySnapshotService) cursorPayload(
	cached *cachedQuerySnapshot, projection SnapshotProjection, direction string, offset int64,
) snapshotCursorPayload {
	return snapshotCursorPayload{
		SnapshotID: cached.id, QueryFingerprint: projection.QueryFingerprint,
		SortField: projection.Query.Sort.Field, SortDirection: projection.Query.Sort.Direction,
		PageDirection: direction, PageSize: projection.PageSize, Offset: offset,
	}
}

func (s *QuerySnapshotService) expiry(cached *cachedQuerySnapshot) time.Time {
	idle := cached.lastUsed.Add(s.limits.IdleTTL)
	absolute := cached.created.Add(s.limits.AbsoluteTTL)
	if idle.Before(absolute) {
		return idle
	}
	return absolute
}

func (s *QuerySnapshotService) purgeExpiredLocked(now time.Time) {
	for id, cached := range s.snapshots {
		if !now.Before(s.expiry(cached)) {
			s.removeSnapshotLocked(id)
		}
	}
}

func (s *QuerySnapshotService) evictLRULocked(owner string, ownerOnly bool) bool {
	var oldest *cachedQuerySnapshot
	for _, candidate := range s.snapshots {
		if ownerOnly && candidate.owner != owner {
			continue
		}
		if oldest == nil || candidate.used < oldest.used {
			oldest = candidate
		}
	}
	if oldest == nil {
		return false
	}
	s.removeSnapshotLocked(oldest.id)
	return true
}

func (s *QuerySnapshotService) removeSnapshotLocked(id string) {
	cached, ok := s.snapshots[id]
	if !ok {
		return
	}
	delete(s.snapshots, id)
	s.cachedRows -= int64(len(cached.rows))
	s.cachedBytes -= cached.bytes
	s.releaseOwnerHandleLocked(cached.owner)
}

func (s *QuerySnapshotService) releaseOwnerHandleLocked(owner string) {
	s.ownerHandles[owner]--
	if s.ownerHandles[owner] == 0 {
		delete(s.ownerHandles, owner)
	}
}

func (s *QuerySnapshotService) gc(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.limits.GCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.purgeExpiredLocked(s.now().UTC())
			s.mu.Unlock()
		}
	}
}

func (s *QuerySnapshotService) stats() snapshotCacheStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshotCacheStats{
		CachedHandles: int64(len(s.snapshots)), ReservedHandles: int64(len(s.builds)),
		CachedRows: s.cachedRows, ReservedRows: s.reservedRows,
		CachedBytes: s.cachedBytes, ReservedBytes: s.reservedBytes,
		ActiveBuilders: int64(len(s.builds)),
	}
}
