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

	"go.kenn.io/docbank/internal/query"
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
	ErrSnapshotGone                    = errors.New("query snapshot is gone")
	ErrSnapshotAdmission               = errors.New("query snapshot cache capacity is unavailable")
	ErrSnapshotBusy                    = errors.New("query snapshot builders are busy")
	ErrSnapshotMemberHash              = errors.New("query snapshot member hash does not match")
	ErrPhotoSnapshotMemberNotInLibrary = errors.New("photo snapshot member is no longer in Library")
	ErrPhotoSnapshotExportUnsupported  = errors.New("photos snapshots cannot be exported here; use Photos to export complete photo assets")
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
	facetsOnly          bool
	id                  uint64
	snapshotID          string
	owner               string
	protectedSnapshotID string
	cancel              context.CancelFunc
	reservedRows        int64
	reservedBytes       int64
	invalidated         bool
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
	if request.FacetsOnly {
		return SnapshotPage{}, errors.New("counts requests do not create snapshots")
	}
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

// CreateFacets materializes a counts-only projection for a facets_only
// request. It never creates a snapshot handle or caches rows; its memory is
// reserved only while the build runs.
func (s *QuerySnapshotService) CreateFacets(
	ctx context.Context, owner string, request SnapshotRequest,
) (SnapshotProjection, error) {
	if !request.FacetsOnly {
		return SnapshotProjection{}, errors.New("counts requests require facets_only")
	}
	prepared, err := s.prepareSnapshot(ctx, owner, request, nil)
	if err != nil {
		return SnapshotProjection{}, err
	}
	defer func() {
		prepared.cancel()
		s.finishBuild(prepared.build, false)
	}()
	return prepared.projection, nil
}

// RunSaved materializes the inspected saved definition and durably records its
// comparison receipt before making the already-reserved snapshot visible.
// mutate, when supplied, coordinates only the receipt write with maintenance.
func (s *QuerySnapshotService) RunSaved(
	ctx context.Context, owner, id string, expectedRevision int64, request SnapshotRequest,
	mutate func(func() error) error,
) (SavedQueryRun, SnapshotPage, error) {
	if request.FacetsOnly {
		return SavedQueryRun{}, SnapshotPage{}, ErrInvalidSavedQueryRun
	}
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
	// Photo receipts vouch for every frozen member, not just the first page.
	var frozen []SnapshotMember
	if prepared.projection.Photos {
		frozen, err = cachedSnapshotMembers(prepared.ctx, prepared.cached.rows)
		if err != nil {
			return SavedQueryRun{}, SnapshotPage{}, err
		}
	}
	var run SavedQueryRun
	write := func() error {
		return s.store.withLogicalTx(prepared.ctx, func(tx *sql.Tx) error {
			if err := validatePhotoSnapshotMembersTx(prepared.ctx, tx, frozen); err != nil {
				return err
			}
			var insertErr error
			run, insertErr = insertSavedQueryRunTx(
				prepared.ctx, tx, id, expectedRevision, prepared.projection, prepared.page,
			)
			return insertErr
		})
	}
	if mutate == nil {
		err = write()
	} else {
		err = mutate(write)
	}
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
	normalized, err := normalizeQuerySnapshotRequest(request)
	if err != nil {
		return preparedQuerySnapshot{}, err
	}
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
	protected := request.ReplacesSnapshotID
	build, err := s.admitNewBuild(owner, cancel, request.FacetsOnly, protected)
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
	var chargedRows, chargedBytes int64
	options.Charge = func(rows, bytes int64) error {
		if err := buildCtx.Err(); err != nil {
			return err
		}
		chargedRows += rows
		chargedBytes += bytes
		if chargedRows > build.reservedRows || chargedBytes > build.reservedBytes {
			if err := s.chargeBuild(build, max(0, chargedRows-build.reservedRows), max(0, chargedBytes-build.reservedBytes)); err != nil {
				return err
			}
		}
		if s.chargeHook != nil {
			return s.chargeHook(buildCtx, owner, rows, bytes)
		}
		return nil
	}
	projection, err := s.store.materializeNormalizedQuerySnapshot(buildCtx, normalized, options)
	if err != nil {
		return preparedQuerySnapshot{}, err
	}
	if err := buildCtx.Err(); err != nil {
		return preparedQuerySnapshot{}, err
	}
	if request.FacetsOnly {
		handedOff = true
		return preparedQuerySnapshot{ctx: buildCtx, cancel: cancel, build: build, projection: projection}, nil
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
	actualBytes := int64(len(metadata)) + encodedRowBytes + querySnapshotHandleOverhead + int64(len(owner)+len(snapshotID))
	if actualBytes > build.reservedBytes {
		if err := s.chargeBuild(build, 0, actualBytes-build.reservedBytes); err != nil {
			return preparedQuerySnapshot{}, err
		}
	}
	if err := buildCtx.Err(); err != nil {
		return preparedQuerySnapshot{}, err
	}
	now := s.now().UTC()
	cached := &cachedQuerySnapshot{
		id: snapshotID, owner: owner, metadata: metadata, rows: encodedRows,
		created: now, lastUsed: now, bytes: actualBytes,
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
		(!metadata.GroupPhotoStacks && payload.Offset%int64(metadata.PageSize) != 0) {
		return SnapshotPage{}, ErrSnapshotCursor
	}
	page, err := s.renderPage(&cached, payload.Offset)
	if err != nil {
		return SnapshotPage{}, err
	}
	if err := s.validatePhotoSnapshotPage(ctx, page); err != nil {
		return SnapshotPage{}, err
	}
	return page, nil
}

func (s *QuerySnapshotService) validatePhotoSnapshotPage(ctx context.Context, page SnapshotPage) error {
	if !page.Photos {
		return nil
	}
	members := make([]SnapshotMember, len(page.Rows))
	for i := range page.Rows {
		members[i] = page.Rows[i].SnapshotMember
	}
	return s.store.photoReadTx(ctx, func(tx *sql.Tx) error {
		return validatePhotoSnapshotMembersTx(ctx, tx, members)
	})
}

func validatePhotoSnapshotMembersTx(ctx context.Context, tx rowQuerier, members []SnapshotMember) error {
	if len(members) == 0 {
		return nil
	}
	assetIDs := make([]string, len(members))
	nodeIDs := make([]int64, len(members))
	for i, member := range members {
		assetIDs[i] = member.AssetID
		nodeIDs[i] = member.NodeID
	}
	assetIDsJSON, err := json.Marshal(assetIDs)
	if err != nil {
		return fmt.Errorf("encoding photo snapshot asset IDs: %w", err)
	}
	if err := photoSnapshotLibraryMembersTx(ctx, tx, assetIDsJSON, len(assetIDs)); err != nil {
		return err
	}
	nodeIDsJSON, err := json.Marshal(nodeIDs)
	if err != nil {
		return fmt.Errorf("encoding photo snapshot node IDs: %w", err)
	}
	var eligible int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM json_each(?) frozen
		JOIN nodes n ON n.id=frozen.value JOIN content_versions cv ON cv.version_id=n.current_version_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
		AND (NOT EXISTS (SELECT 1 FROM photo_files owner WHERE owner.node_id=n.id AND owner.asset_id IS NOT NULL)
		OR `+photoBrowseEligibleMemberPredicate(false)+`)`, string(nodeIDsJSON)).Scan(&eligible); err != nil {
		return err
	}
	if eligible != len(nodeIDs) {
		return ErrPhotoSnapshotMemberNotInLibrary
	}
	return nil
}

func (s *QuerySnapshotService) CopyMembers(
	ctx context.Context, owner, snapshotID, memberHash string,
) ([]SnapshotMember, error) {
	return s.CopyMembersBounded(ctx, owner, snapshotID, memberHash, int(querySnapshotMaxRows))
}

// CopyMembersBounded rejects oversized membership before allocating its copy.
func (s *QuerySnapshotService) CopyMembersBounded(ctx context.Context, owner, snapshotID, memberHash string, limit int) ([]SnapshotMember, error) {
	return s.copyMembersBounded(ctx, owner, snapshotID, memberHash, limit, false)
}

// CopyExportMembersBounded accepts Documents snapshots; Photos exports need complete assets.
func (s *QuerySnapshotService) CopyExportMembersBounded(ctx context.Context, owner, snapshotID, memberHash string, limit int) ([]SnapshotMember, error) {
	return s.copyMembersBounded(ctx, owner, snapshotID, memberHash, limit, true)
}

func (s *QuerySnapshotService) copyMembersBounded(ctx context.Context, owner, snapshotID, memberHash string, limit int, exporting bool) ([]SnapshotMember, error) {
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
	if exporting && metadata.Photos {
		return nil, ErrPhotoSnapshotExportUnsupported
	}
	members, err := cachedSnapshotMembers(ctx, cached.rows)
	if err != nil {
		return nil, err
	}
	if int64(len(members)) != metadata.Total || snapshotMemberHash(members) != metadata.MemberHash {
		return nil, errors.New("cached query snapshot membership failed validation")
	}
	return members, nil
}

func cachedSnapshotMembers(ctx context.Context, rows [][]byte) ([]SnapshotMember, error) {
	members := make([]SnapshotMember, len(rows))
	for i := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var row SnapshotRow
		if err := json.Unmarshal(rows[i], &row); err != nil {
			return nil, fmt.Errorf("decoding cached query snapshot member: %w", err)
		}
		members[i] = row.SnapshotMember
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
	s.Stop()
	<-s.done
	return nil
}

func (s *QuerySnapshotService) Shutdown(ctx context.Context) error {
	s.Stop()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop invalidates cached snapshots, rejects new builds, and cancels active
// builders without waiting for them to return. Repeated calls have no effect.
func (s *QuerySnapshotService) Stop() {
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

func (s *QuerySnapshotService) admitNewBuild(owner string, cancel context.CancelFunc, facetsOnly bool, protectedSnapshotID string) (*querySnapshotBuild, error) {
	if facetsOnly {
		build, _, err := s.admitBuild("", owner, cancel, true, "")
		return build, err
	}
	for range 8 {
		snapshotID, err := s.newSnapshotID()
		if err != nil {
			return nil, err
		}
		build, collision, err := s.admitBuild(snapshotID, owner, cancel, false, protectedSnapshotID)
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
	snapshotID, owner string, cancel context.CancelFunc, facetsOnly bool, protectedSnapshotID string,
) (*querySnapshotBuild, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrSnapshotGone
	}
	s.purgeExpiredLocked(s.now().UTC())
	// Counts and snapshots take separate builder slots so neither blocks the other.
	if s.activeBuildsLocked(facetsOnly) >= s.limits.MaxBuilders {
		return nil, false, ErrSnapshotBusy
	}
	if _, exists := s.snapshots[snapshotID]; exists {
		return nil, true, nil
	}
	for _, build := range s.builds {
		if !facetsOnly && build.snapshotID == snapshotID {
			return nil, true, nil
		}
	}
	if cached := s.snapshots[protectedSnapshotID]; cached == nil || cached.owner != owner {
		protectedSnapshotID = ""
	}
	for !facetsOnly && (len(s.snapshots)+s.activeBuildsLocked(false) >= s.limits.MaxHandles ||
		s.ownerHandles[owner] >= s.limits.MaxOwnerHandles) {
		ownerOnly := s.ownerHandles[owner] >= s.limits.MaxOwnerHandles
		if !s.evictLRULocked(owner, ownerOnly, protectedSnapshotID) {
			return nil, false, ErrSnapshotAdmission
		}
	}
	s.nextBuildID++
	build := &querySnapshotBuild{
		id: s.nextBuildID, snapshotID: snapshotID, owner: owner, cancel: cancel, facetsOnly: facetsOnly, protectedSnapshotID: protectedSnapshotID,
	}
	s.builds[build.id] = build
	if !facetsOnly {
		s.ownerHandles[owner]++
	}
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
	if exceedsBound(s.cachedRows+s.reservedRows, rows, s.limits.MaxRows) ||
		exceedsBound(s.cachedBytes+s.reservedBytes, bytes, s.limits.MaxBytes) {
		s.purgeExpiredLocked(s.now().UTC())
	}
	for exceedsBound(s.cachedRows+s.reservedRows, rows, s.limits.MaxRows) ||
		exceedsBound(s.cachedBytes+s.reservedBytes, bytes, s.limits.MaxBytes) {
		if !s.evictLRULocked("", false, "") {
			return ErrSnapshotAdmission
		}
	}
	// Reserve small batches before allocating rows, without exceeding either
	// global budget. Publication releases the unused part of the reservation.
	if rows > 0 {
		rows = min(max(rows, 256), s.limits.MaxRows-s.cachedRows-s.reservedRows)
	}
	if bytes > 0 {
		bytes = min(max(bytes, 256<<10), s.limits.MaxBytes-s.cachedBytes-s.reservedBytes)
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
	s.cachedRows += int64(len(cached.rows))
	s.cachedBytes += cached.bytes
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
			if !build.facetsOnly {
				s.releaseOwnerHandleLocked(build.owner)
			}
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
	end, err := photoSnapshotPageEnd(cached, projection, start)
	if err != nil {
		return SnapshotPage{}, err
	}
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
	if start > 0 {
		previous := max(start-int64(projection.PageSize), 0)
		if projection.GroupPhotoStacks {
			for position := int64(0); position < start; {
				previous = position
				position, err = photoSnapshotPageEnd(cached, projection, position)
				if err != nil {
					return SnapshotPage{}, err
				}
			}
		}
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

func (s *QuerySnapshotService) evictLRULocked(owner string, ownerOnly bool, protectedSnapshotID string) bool {
	var oldest *cachedQuerySnapshot
	for _, candidate := range s.snapshots {
		if candidate.id == protectedSnapshotID {
			continue
		}
		protected := false
		for _, build := range s.builds {
			if candidate.id == build.protectedSnapshotID {
				protected = true
				break
			}
		}
		if protected || ownerOnly && candidate.owner != owner {
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

// activeBuildsLocked counts in-flight builds of one kind: counts-only builds or
// snapshot builds, which also reserve a handle.
func (s *QuerySnapshotService) activeBuildsLocked(facetsOnly bool) int {
	count := 0
	for _, build := range s.builds {
		if build.facetsOnly == facetsOnly {
			count++
		}
	}
	return count
}

func (s *QuerySnapshotService) stats() snapshotCacheStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshotCacheStats{
		CachedHandles: int64(len(s.snapshots)), ReservedHandles: int64(s.activeBuildsLocked(false)),
		CachedRows: s.cachedRows, ReservedRows: s.reservedRows,
		CachedBytes: s.cachedBytes, ReservedBytes: s.reservedBytes,
		ActiveBuilders: int64(len(s.builds)),
	}
}

// FirstPage reuses an admitted owner-bound snapshot without running its query again.
func (s *QuerySnapshotService) FirstPage(ctx context.Context, owner, snapshotID string) (SnapshotPage, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotPage{}, err
	}
	cached, err := s.lookup(owner, snapshotID)
	if err != nil {
		return SnapshotPage{}, err
	}
	page, err := s.renderPage(&cached, 0)
	if err != nil {
		return SnapshotPage{}, err
	}
	if err := s.validatePhotoSnapshotPage(ctx, page); err != nil {
		return SnapshotPage{}, err
	}
	return page, nil
}

// PhotoPage binds continuation inputs to the admitted view, including its projection.
func (s *QuerySnapshotService) PhotoPage(ctx context.Context, owner, id, cursor string, request SnapshotRequest) (SnapshotPage, error) {
	cached, err := s.lookup(owner, id)
	if err != nil {
		return SnapshotPage{}, err
	}
	var first SnapshotProjection
	if err := json.Unmarshal(cached.metadata, &first); err != nil {
		return SnapshotPage{}, err
	}
	request, err = normalizeQuerySnapshotRequest(request)
	if err != nil {
		return SnapshotPage{}, err
	}
	fingerprint, err := query.Fingerprint(request.Query)
	if err != nil {
		return SnapshotPage{}, err
	}
	coverage, err := normalizeCoverageSelection(request.Coverage)
	if err != nil {
		return SnapshotPage{}, err
	}
	if fingerprint != first.QueryFingerprint || coverage != first.Coverage || request.PageSize != first.PageSize || request.GroupPhotoStacks != first.GroupPhotoStacks || request.PhotoProjection != first.PhotoProjection {
		return SnapshotPage{}, ErrSnapshotCursor
	}
	if cursor == "" {
		return s.FirstPage(ctx, owner, id)
	}
	return s.Page(ctx, owner, id, cursor)
}

func photoSnapshotPageEnd(cached *cachedQuerySnapshot, projection SnapshotProjection, start int64) (int64, error) {
	end := min(start+int64(projection.PageSize), int64(len(cached.rows)))
	if projection.GroupPhotoStacks && end < int64(len(cached.rows)) && end > start {
		var boundary SnapshotRow
		if err := json.Unmarshal(cached.rows[end-1], &boundary); err != nil {
			return 0, err
		}
		for _, stack := range projection.PhotoStacks {
			if !slices.Contains(stack.AssetIDs, boundary.AssetID) {
				continue
			}
			for end < int64(len(cached.rows)) {
				var next SnapshotRow
				if err := json.Unmarshal(cached.rows[end], &next); err != nil {
					return 0, err
				}
				if !slices.Contains(stack.AssetIDs, next.AssetID) {
					break
				}
				end++
			}
			break
		}
	}
	return end, nil
}

// PhotoStackRepresentatives reselects choices within accepted groups, preserving their membership.
func (s *QuerySnapshotService) PhotoStackRepresentatives(ctx context.Context, owner, id string) ([]PhotoStack, error) {
	cached, err := s.lookup(owner, id)
	if err != nil {
		return nil, err
	}
	var projection SnapshotProjection
	if err := json.Unmarshal(cached.metadata, &projection); err != nil {
		return nil, err
	}
	if !projection.Photos {
		return nil, ErrSnapshotCursor
	}
	return s.photoStackRepresentatives(ctx, projection.PhotoStacks)
}

func (s *QuerySnapshotService) photoStackRepresentatives(ctx context.Context, groups []PhotoStack) ([]PhotoStack, error) {
	var stacks []PhotoStack
	err := s.store.photoReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		stacks, err = photoStackRepresentativesTx(ctx, tx, groups)
		return err
	})
	return stacks, err
}

func photoStackRepresentativesTx(ctx context.Context, tx metadataQuerier, groups []PhotoStack) (stacks []PhotoStack, err error) {
	var ids []string
	for _, stack := range groups {
		ids = append(ids, stack.AssetIDs...)
	}
	inputs := map[string]photoStackFrame{}
	if err := queryInChunks(ctx, tx, `SELECT a.asset_id,`+photoStackChoiceColumns+` FROM `+photoBrowseDisplayFrom+` WHERE `+photoBrowseLiveDisplay+` AND `+photoVisibilityPredicate(false)+` AND a.asset_id IN (`, ids, func(rows *sql.Rows) error {
		var frame photoStackFrame
		err := rows.Scan(&frame.AssetID, &frame.Rank, &frame.Picked, &frame.Rejected)
		inputs[frame.AssetID] = frame
		return err
	}); err != nil {
		return nil, err
	}
	for _, stack := range groups {
		frames := make([]photoStackFrame, len(stack.AssetIDs))
		for i, id := range stack.AssetIDs {
			frames[i] = inputs[id]
		}
		if frames = slices.DeleteFunc(frames, func(frame photoStackFrame) bool { return frame.AssetID == "" }); len(frames) >= 2 {
			stacks = append(stacks, newPhotoStack(frames))
		}
	}
	return stacks, nil
}

func (s *Store) currentPhotoRowsTx(ctx context.Context, tx metadataQuerier, members []SnapshotMember, recipes map[string]string) ([]PhotoBrowseRow, error) {
	if err := validatePhotoSnapshotMembersTx(ctx, tx, members); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(members)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+photoBrowseSelect+`,'',0 FROM `+photoBrowseDisplayFrom+` JOIN json_each(?) frozen ON json_extract(frozen.value,'$.asset_id')=a.asset_id WHERE `+photoBrowseLiveDisplay+` ORDER BY CAST(frozen.key AS INTEGER)`, string(encoded))
	if err != nil {
		return nil, err
	}
	photos, err := scanPhotoBrowseRows(rows, PhotoBrowsePosition{})
	if err == nil && recipes != nil {
		err = s.hydratePhotoPreviews(ctx, tx, PhotoBrowseRequest{Recipes: recipes}, photos)
	}
	return photos, err
}

// CurrentPhotoPage reads display values and representatives while retaining the accepted order and groups.
func (s *QuerySnapshotService) CurrentPhotoPage(ctx context.Context, snapshot SnapshotPage, recipes map[string]string) (PhotoBrowsePage, error) {
	page := PhotoBrowsePage{Total: snapshot.Total, RelevanceAvailable: true, BurstGapSeconds: snapshot.PhotoBurstGapSeconds, GroupingIncomplete: snapshot.PhotoGroupingIncomplete}
	for _, facet := range snapshot.Facets {
		if facet.Dimension != "capture_day" {
			page.Facets = append(page.Facets, facet)
		}
	}
	err := s.store.photoReadTx(ctx, func(tx *sql.Tx) error {
		onPage := make(map[string]bool, len(snapshot.Rows))
		for _, row := range snapshot.Rows {
			onPage[row.AssetID] = true
		}
		var err error
		page.Items, err = s.store.currentPhotoRowsTx(ctx, tx, snapshotMembers(snapshot.Rows), recipes)
		if err != nil {
			return err
		}
		for i, row := range snapshot.Rows {
			page.Items[i].MatchedAISuggestion = row.SuggestionMatch
		}
		var groups []PhotoStack
		for _, stack := range snapshot.PhotoStacks {
			if slices.ContainsFunc(stack.AssetIDs, func(id string) bool { return !onPage[id] }) {
				continue
			}
			groups = append(groups, stack)
		}
		page.Stacks, err = photoStackRepresentativesTx(ctx, tx, groups)
		return err
	})
	return page, err
}
