package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// IndexCurrent reports whether a projection covers the current source
// watermark. A projection ahead of the observed source is still current.
func IndexCurrent(sourceWatermark, indexWatermark uint64) bool {
	return indexWatermark >= sourceWatermark
}

type IndexProjectionKind string

const (
	IndexLexical  IndexProjectionKind = "lexical"
	IndexMetadata IndexProjectionKind = "metadata"
	IndexTag      IndexProjectionKind = "tag"
	IndexMap      IndexProjectionKind = "map"
	IndexVector   IndexProjectionKind = "vector"
)

type IndexState string

const (
	IndexStateQueryable IndexState = "queryable"
	IndexStateBuilding  IndexState = "building"
	IndexStateStale     IndexState = "stale"
	IndexStateFailed    IndexState = "failed"
	IndexStateDisabled  IndexState = "disabled"
)

type IndexFailureReason string

const (
	IndexFailureCorruptManifest   IndexFailureReason = "corrupt_manifest"
	IndexFailureBuildFailed       IndexFailureReason = "build_failed"
	IndexFailureStorageFull       IndexFailureReason = "storage_full"
	IndexFailureSourceChanged     IndexFailureReason = "source_changed"
	IndexFailurePermissionChanged IndexFailureReason = "permission_changed"
	IndexFailureCanceled          IndexFailureReason = "canceled"
)

var (
	ErrIndexFreshnessTimeout        = errors.New("index freshness wait timed out")
	ErrIndexManifestCorrupt         = errors.New("index manifest is corrupt")
	ErrIndexBuildFailed             = errors.New("index build failed")
	ErrIndexStorageFull             = errors.New("index storage is full")
	ErrIndexSourceChanged           = errors.New("index source changed during build")
	ErrIndexPermissionChanged       = errors.New("index permission changed during build")
	ErrIndexPlanChanged             = errors.New("index repair plan changed")
	ErrIndexProviderConsentRequired = errors.New("index repair provider consent is required")
	ErrIndexRepairInProgress        = errors.New("index repair is already in progress")
	ErrIndexDisabled                = errors.New("index projection is disabled")
	ErrIndexNotAuthorized           = errors.New("index projection is not authorized")
	ErrInvalidIndexRequest          = errors.New("index request is invalid")
)

type IndexTarget struct {
	Kind IndexProjectionKind `json:"kind" enum:"lexical,metadata,tag,map,vector"`
	Key  string              `json:"key,omitempty" maxLength:"256"`
}

type IndexCoverage struct {
	Expected    uint64 `json:"expected"`
	Indexed     uint64 `json:"indexed"`
	Unavailable uint64 `json:"unavailable"`
}

// IndexProjectionSnapshot is the backend's authorization-filtered view of one
// serving projection. Commit implementations must compare both captured
// watermarks in the same atomic operation that publishes a candidate.
type IndexProjectionSnapshot struct {
	Target                 IndexTarget        `json:"target"`
	Enabled                bool               `json:"enabled"`
	Authorized             bool               `json:"authorized"`
	Generation             string             `json:"generation,omitempty"`
	SourceWatermark        uint64             `json:"source_watermark"`
	IndexWatermark         uint64             `json:"index_watermark"`
	AuthorizationWatermark uint64             `json:"authorization_watermark"`
	Coverage               IndexCoverage      `json:"coverage"`
	FailureReason          IndexFailureReason `json:"failure_reason,omitempty"`
}

type IndexProjectionStatus struct {
	Target          IndexTarget        `json:"target"`
	State           IndexState         `json:"state" enum:"queryable,building,stale,failed,disabled"`
	Serving         bool               `json:"serving"`
	Generation      string             `json:"generation,omitempty"`
	SourceWatermark uint64             `json:"source_watermark"`
	IndexWatermark  uint64             `json:"index_watermark"`
	Coverage        IndexCoverage      `json:"coverage"`
	FailureReason   IndexFailureReason `json:"failure_reason,omitempty"`
}

type IndexStatusRequest struct {
	RequireFresh bool
	Wait         time.Duration
}

type IndexStatusReport struct {
	Fresh           bool                    `json:"fresh"`
	SourceWatermark uint64                  `json:"source_watermark"`
	IndexWatermark  uint64                  `json:"index_watermark"`
	Projections     []IndexProjectionStatus `json:"projections"`
}

type IndexProviderDisclosure struct {
	Processor   string   `json:"processor" maxLength:"256"`
	Endpoint    string   `json:"endpoint,omitempty" maxLength:"2048"`
	Model       string   `json:"model,omitempty" maxLength:"256"`
	DataClasses []string `json:"data_classes,omitempty" maxItems:"64"`
}

type IndexRepairWork struct {
	Documents           uint64                    `json:"documents"`
	Bytes               uint64                    `json:"bytes"`
	ProviderCalls       uint64                    `json:"provider_calls"`
	EstimatedCostMicros uint64                    `json:"estimated_cost_micros"`
	CostCurrency        string                    `json:"cost_currency,omitempty" pattern:"^[A-Z]{3}$"`
	Providers           []IndexProviderDisclosure `json:"providers,omitempty" maxItems:"16"`
}

type IndexRepairPlanRequest struct {
	Targets []IndexTarget `json:"targets,omitempty" maxItems:"64"`
}

type IndexRepairProjectionPlan struct {
	Target                 IndexTarget     `json:"target"`
	CurrentGeneration      string          `json:"current_generation,omitempty"`
	SourceWatermark        uint64          `json:"source_watermark"`
	IndexWatermark         uint64          `json:"index_watermark"`
	AuthorizationWatermark uint64          `json:"authorization_watermark"`
	Work                   IndexRepairWork `json:"work"`
}

type IndexRepairPlan struct {
	Fingerprint string                      `json:"fingerprint" pattern:"^[0-9a-f]{64}$"`
	Projections []IndexRepairProjectionPlan `json:"projections"`
}

type IndexRepairRequest struct {
	Targets           []IndexTarget `json:"targets" minItems:"1" maxItems:"1"`
	PlanFingerprint   string        `json:"plan_fingerprint" pattern:"^[0-9a-f]{64}$"`
	AllowProviderWork bool          `json:"allow_provider_work"`
}

type IndexRepairResult struct {
	Target             IndexTarget `json:"target"`
	PreviousGeneration string      `json:"previous_generation,omitempty"`
	Generation         string      `json:"generation"`
	SourceWatermark    uint64      `json:"source_watermark"`
}

type IndexRepairReport struct {
	PlanFingerprint string              `json:"plan_fingerprint"`
	Projections     []IndexRepairResult `json:"projections"`
}

type IndexBuildRequest struct {
	Target                 IndexTarget
	SourceWatermark        uint64
	AuthorizationWatermark uint64
}

type IndexCandidate struct {
	Target     IndexTarget
	Generation string
	Handle     any `json:"-"`
}

// IndexProjectionBackend adapts an existing immutable projection. Build must
// leave its candidate unreachable. Validate must inspect the complete candidate.
// Build returns no candidate when it returns an error. Commit must atomically
// recheck request watermarks, switch the serving head, and retain the replaced
// generation according to the backend's rollback policy.
type IndexProjectionBackend interface {
	Target() IndexTarget
	Inspect(ctx context.Context) (IndexProjectionSnapshot, error)
	Preview(ctx context.Context, snapshot IndexProjectionSnapshot) (IndexRepairWork, error)
	Build(ctx context.Context, request IndexBuildRequest) (IndexCandidate, error)
	Validate(ctx context.Context, candidate IndexCandidate) error
	Commit(ctx context.Context, candidate IndexCandidate, request IndexBuildRequest) (IndexProjectionSnapshot, error)
	Discard(ctx context.Context, candidate IndexCandidate) error
}

type IndexCoordinatorConfig struct {
	Backends       []IndexProjectionBackend
	Discover       func(context.Context) ([]IndexProjectionBackend, error)
	MaxFreshWait   time.Duration
	PollInterval   time.Duration
	CleanupTimeout time.Duration
}

type indexRuntimeState struct {
	building bool
	failure  IndexFailureReason
}

type IndexCoordinator struct {
	backends       map[IndexTarget]IndexProjectionBackend
	order          []IndexTarget
	maxFreshWait   time.Duration
	pollInterval   time.Duration
	cleanupTimeout time.Duration
	discover       func(context.Context) ([]IndexProjectionBackend, error)

	mu      sync.Mutex
	runtime map[IndexTarget]indexRuntimeState
	changed chan struct{}
}

func NewIndexCoordinator(config IndexCoordinatorConfig) (*IndexCoordinator, error) {
	if len(config.Backends) == 0 || len(config.Backends) > 64 {
		return nil, fmt.Errorf("%w: one to 64 index backends are required", ErrInvalidIndexRequest)
	}
	if config.MaxFreshWait <= 0 {
		config.MaxFreshWait = 10 * time.Second
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 25 * time.Millisecond
	}
	if config.CleanupTimeout <= 0 {
		config.CleanupTimeout = 5 * time.Second
	}
	coordinator := &IndexCoordinator{
		backends:     make(map[IndexTarget]IndexProjectionBackend, len(config.Backends)),
		runtime:      make(map[IndexTarget]indexRuntimeState, len(config.Backends)),
		maxFreshWait: config.MaxFreshWait, pollInterval: config.PollInterval,
		cleanupTimeout: config.CleanupTimeout, changed: make(chan struct{}),
		discover: config.Discover,
	}
	for _, backend := range config.Backends {
		if interfaceNil(backend) {
			return nil, fmt.Errorf("%w: index backend is nil", ErrInvalidIndexRequest)
		}
		target := backend.Target()
		if err := validateIndexTarget(target); err != nil {
			return nil, err
		}
		if _, exists := coordinator.backends[target]; exists {
			return nil, fmt.Errorf("%w: duplicate index target %s", ErrInvalidIndexRequest, indexTargetName(target))
		}
		coordinator.backends[target] = backend
		coordinator.order = append(coordinator.order, target)
	}
	slices.SortFunc(coordinator.order, compareIndexTargets)
	return coordinator, nil
}

func interfaceNil(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

func validateIndexTarget(target IndexTarget) error {
	switch target.Kind {
	case IndexLexical, IndexMetadata, IndexTag, IndexMap, IndexVector:
	default:
		return fmt.Errorf("%w: unknown index projection kind", ErrInvalidIndexRequest)
	}
	if !utf8.ValidString(target.Key) || len(target.Key) > 256 || strings.ContainsAny(target.Key, "\x00\r\n") {
		return fmt.Errorf("%w: invalid index target key", ErrInvalidIndexRequest)
	}
	return nil
}

func indexTargetName(target IndexTarget) string {
	return string(target.Kind) + "\x00" + target.Key
}

func compareIndexTargets(left, right IndexTarget) int {
	return strings.Compare(indexTargetName(left), indexTargetName(right))
}

func (coordinator *IndexCoordinator) Status(ctx context.Context, request IndexStatusRequest) (IndexStatusReport, error) {
	if coordinator == nil {
		return IndexStatusReport{}, fmt.Errorf("%w: index coordinator is nil", ErrInvalidIndexRequest)
	}
	if err := coordinator.refresh(ctx); err != nil {
		return IndexStatusReport{}, err
	}
	if !request.RequireFresh {
		return coordinator.statusOnce(ctx)
	}
	wait := request.Wait
	if wait <= 0 || wait > coordinator.maxFreshWait {
		wait = coordinator.maxFreshWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(coordinator.pollInterval)
	defer ticker.Stop()
	for {
		report, err := coordinator.statusOnce(ctx)
		if err != nil || report.Fresh {
			return report, err
		}
		coordinator.mu.Lock()
		changed := coordinator.changed
		coordinator.mu.Unlock()
		select {
		case <-ctx.Done():
			return IndexStatusReport{}, ctx.Err()
		case <-timer.C:
			return IndexStatusReport{}, fmt.Errorf("%w after %s", ErrIndexFreshnessTimeout, wait)
		case <-ticker.C:
		case <-changed:
		}
	}
}

// Reconcile reloads and validates backend state without starting provider or
// rebuild work. Verified generations therefore survive daemon restarts.
func (coordinator *IndexCoordinator) Reconcile(ctx context.Context) (IndexStatusReport, error) {
	return coordinator.Status(ctx, IndexStatusRequest{})
}

func (coordinator *IndexCoordinator) statusOnce(ctx context.Context) (IndexStatusReport, error) {
	coordinator.mu.Lock()
	order := slices.Clone(coordinator.order)
	backends := make(map[IndexTarget]IndexProjectionBackend, len(coordinator.backends))
	maps.Copy(backends, coordinator.backends)
	coordinator.mu.Unlock()
	report := IndexStatusReport{Fresh: true, Projections: make([]IndexProjectionStatus, 0, len(order))}
	firstActive := true
	for _, target := range order {
		if err := ctx.Err(); err != nil {
			return IndexStatusReport{}, err
		}
		status, err := coordinator.projectionStatus(ctx, target, backends[target])
		if err != nil {
			return IndexStatusReport{}, err
		}
		report.Projections = append(report.Projections, status)
		if status.State == IndexStateDisabled {
			continue
		}
		if status.SourceWatermark > report.SourceWatermark {
			report.SourceWatermark = status.SourceWatermark
		}
		if firstActive || status.IndexWatermark < report.IndexWatermark {
			report.IndexWatermark = status.IndexWatermark
			firstActive = false
		}
		if status.State != IndexStateQueryable {
			report.Fresh = false
		}
	}
	return report, nil
}

func (coordinator *IndexCoordinator) projectionStatus(
	ctx context.Context, target IndexTarget, backend IndexProjectionBackend,
) (IndexProjectionStatus, error) {
	snapshot, err := backend.Inspect(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return IndexProjectionStatus{}, ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return IndexProjectionStatus{}, err
		}
		return IndexProjectionStatus{Target: target, State: IndexStateFailed, FailureReason: safeIndexFailure(err)}, nil
	}
	if snapshot.Target != target || !indexSnapshotValid(snapshot) {
		return IndexProjectionStatus{Target: target, State: IndexStateFailed, FailureReason: IndexFailureCorruptManifest}, nil
	}
	if !snapshot.Enabled || !snapshot.Authorized {
		return IndexProjectionStatus{Target: target, State: IndexStateDisabled}, nil
	}
	status := IndexProjectionStatus{Target: target, Serving: snapshot.Generation != "", Generation: snapshot.Generation,
		SourceWatermark: snapshot.SourceWatermark, IndexWatermark: snapshot.IndexWatermark, Coverage: snapshot.Coverage}
	coordinator.mu.Lock()
	runtime := coordinator.runtime[target]
	coordinator.mu.Unlock()
	switch {
	case runtime.building:
		status.State = IndexStateBuilding
	case runtime.failure != "" && !IndexCurrent(snapshot.SourceWatermark, snapshot.IndexWatermark):
		status.State, status.FailureReason = IndexStateFailed, runtime.failure
	case snapshot.FailureReason != "":
		status.State, status.FailureReason = IndexStateFailed, snapshot.FailureReason
	case IndexCurrent(snapshot.SourceWatermark, snapshot.IndexWatermark):
		status.State = IndexStateQueryable
	default:
		status.State = IndexStateStale
	}
	return status, nil
}

func validateIndexSnapshot(snapshot IndexProjectionSnapshot) error {
	if err := validateIndexTarget(snapshot.Target); err != nil {
		return err
	}
	if snapshot.Coverage.Indexed > snapshot.Coverage.Expected ||
		snapshot.Coverage.Unavailable > snapshot.Coverage.Expected-snapshot.Coverage.Indexed {
		return ErrIndexManifestCorrupt
	}
	if !utf8.ValidString(snapshot.Generation) || len(snapshot.Generation) > 512 || strings.ContainsAny(snapshot.Generation, "\x00\r\n") {
		return ErrIndexManifestCorrupt
	}
	if snapshot.FailureReason != "" && !validIndexFailure(snapshot.FailureReason) {
		return ErrIndexManifestCorrupt
	}
	return nil
}

func indexSnapshotValid(snapshot IndexProjectionSnapshot) bool {
	return validateIndexSnapshot(snapshot) == nil
}

func validIndexFailure(reason IndexFailureReason) bool {
	switch reason {
	case IndexFailureCorruptManifest, IndexFailureBuildFailed, IndexFailureStorageFull,
		IndexFailureSourceChanged, IndexFailurePermissionChanged, IndexFailureCanceled:
		return true
	default:
		return false
	}
}

func safeIndexFailure(err error) IndexFailureReason {
	switch {
	case errors.Is(err, ErrIndexManifestCorrupt):
		return IndexFailureCorruptManifest
	case errors.Is(err, ErrIndexStorageFull):
		return IndexFailureStorageFull
	case errors.Is(err, ErrIndexSourceChanged):
		return IndexFailureSourceChanged
	case errors.Is(err, ErrIndexPermissionChanged), errors.Is(err, ErrIndexNotAuthorized):
		return IndexFailurePermissionChanged
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return IndexFailureCanceled
	default:
		return IndexFailureBuildFailed
	}
}

func (coordinator *IndexCoordinator) PlanRepair(ctx context.Context, request IndexRepairPlanRequest) (IndexRepairPlan, error) {
	if coordinator == nil {
		return IndexRepairPlan{}, fmt.Errorf("%w: index coordinator is nil", ErrInvalidIndexRequest)
	}
	if err := coordinator.refresh(ctx); err != nil {
		return IndexRepairPlan{}, err
	}
	targets, err := coordinator.normalizeTargets(request.Targets)
	if err != nil {
		return IndexRepairPlan{}, err
	}
	plan := IndexRepairPlan{Projections: make([]IndexRepairProjectionPlan, 0, len(targets))}
	for _, target := range targets {
		coordinator.mu.Lock()
		backend := coordinator.backends[target]
		coordinator.mu.Unlock()
		snapshot, err := backend.Inspect(ctx)
		if err != nil {
			return IndexRepairPlan{}, err
		}
		if snapshot.Target != target || validateIndexSnapshot(snapshot) != nil {
			return IndexRepairPlan{}, ErrIndexManifestCorrupt
		}
		if !snapshot.Enabled {
			return IndexRepairPlan{}, ErrIndexDisabled
		}
		if !snapshot.Authorized {
			return IndexRepairPlan{}, ErrIndexNotAuthorized
		}
		work, err := backend.Preview(ctx, snapshot)
		if err != nil {
			return IndexRepairPlan{}, err
		}
		if err := normalizeIndexRepairWork(&work); err != nil {
			return IndexRepairPlan{}, err
		}
		plan.Projections = append(plan.Projections, IndexRepairProjectionPlan{Target: target,
			CurrentGeneration: snapshot.Generation, SourceWatermark: snapshot.SourceWatermark,
			IndexWatermark: snapshot.IndexWatermark, AuthorizationWatermark: snapshot.AuthorizationWatermark,
			Work: work})
	}
	raw, err := json.Marshal(plan.Projections)
	if err != nil {
		return IndexRepairPlan{}, fmt.Errorf("encoding index repair plan: %w", err)
	}
	digest := sha256.Sum256(append([]byte("index-repair-plan/v1\x00"), raw...))
	plan.Fingerprint = hex.EncodeToString(digest[:])
	return plan, nil
}

func normalizeIndexRepairWork(work *IndexRepairWork) error {
	if len(work.Providers) > 16 {
		return fmt.Errorf("%w: too many index providers", ErrInvalidIndexRequest)
	}
	if (work.EstimatedCostMicros == 0) != (work.CostCurrency == "") ||
		work.CostCurrency != "" && !validCostCurrency(work.CostCurrency) {
		return fmt.Errorf("%w: index cost requires an ISO currency", ErrInvalidIndexRequest)
	}
	for index := range work.Providers {
		provider := &work.Providers[index]
		if provider.Processor == "" || !validIndexDisclosureText(provider.Processor, 256) ||
			!validIndexDisclosureText(provider.Endpoint, 2048) || !validIndexDisclosureText(provider.Model, 256) ||
			len(provider.DataClasses) > 64 {
			return fmt.Errorf("%w: invalid index provider disclosure", ErrInvalidIndexRequest)
		}
		provider.DataClasses = slices.Clone(provider.DataClasses)
		for _, dataClass := range provider.DataClasses {
			if dataClass == "" || !validIndexDisclosureText(dataClass, 128) {
				return fmt.Errorf("%w: invalid index provider data class", ErrInvalidIndexRequest)
			}
		}
		slices.Sort(provider.DataClasses)
	}
	slices.SortFunc(work.Providers, func(left, right IndexProviderDisclosure) int {
		return strings.Compare(left.Processor+"\x00"+left.Endpoint+"\x00"+left.Model,
			right.Processor+"\x00"+right.Endpoint+"\x00"+right.Model)
	})
	return nil
}

func validCostCurrency(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, character := range value {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func validIndexDisclosureText(value string, limit int) bool {
	return utf8.ValidString(value) && len(value) <= limit && !strings.ContainsAny(value, "\x00\r\n")
}

func (coordinator *IndexCoordinator) normalizeTargets(requested []IndexTarget) ([]IndexTarget, error) {
	coordinator.mu.Lock()
	order := slices.Clone(coordinator.order)
	known := make(map[IndexTarget]struct{}, len(coordinator.backends))
	for target := range coordinator.backends {
		known[target] = struct{}{}
	}
	coordinator.mu.Unlock()
	if len(requested) == 0 {
		return order, nil
	}
	if len(requested) > 64 {
		return nil, fmt.Errorf("%w: too many index targets", ErrInvalidIndexRequest)
	}
	targets := slices.Clone(requested)
	slices.SortFunc(targets, compareIndexTargets)
	for index, target := range targets {
		if err := validateIndexTarget(target); err != nil {
			return nil, err
		}
		if _, exists := known[target]; !exists {
			return nil, fmt.Errorf("%w: unknown index target %s", ErrInvalidIndexRequest, indexTargetName(target))
		}
		if index > 0 && target == targets[index-1] {
			return nil, fmt.Errorf("%w: duplicate index target %s", ErrInvalidIndexRequest, indexTargetName(target))
		}
	}
	return targets, nil
}

func (coordinator *IndexCoordinator) refresh(ctx context.Context) error {
	if coordinator.discover == nil {
		return nil
	}
	backends, err := coordinator.discover(ctx)
	if err != nil {
		return err
	}
	if len(backends) > 64 {
		return fmt.Errorf("%w: too many discovered index backends", ErrInvalidIndexRequest)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	for _, backend := range backends {
		if interfaceNil(backend) {
			return fmt.Errorf("%w: discovered index backend is nil", ErrInvalidIndexRequest)
		}
		target := backend.Target()
		if err := validateIndexTarget(target); err != nil {
			return err
		}
		if _, exists := coordinator.backends[target]; exists {
			continue
		}
		if len(coordinator.backends) == 64 {
			return fmt.Errorf("%w: too many index backends", ErrInvalidIndexRequest)
		}
		coordinator.backends[target] = backend
		coordinator.order = append(coordinator.order, target)
	}
	slices.SortFunc(coordinator.order, compareIndexTargets)
	return nil
}

func (coordinator *IndexCoordinator) Repair(ctx context.Context, request IndexRepairRequest) (IndexRepairReport, error) {
	if len(request.Targets) != 1 || len(request.PlanFingerprint) != sha256.Size*2 {
		return IndexRepairReport{}, ErrInvalidIndexRequest
	}
	plan, err := coordinator.PlanRepair(ctx, IndexRepairPlanRequest{Targets: request.Targets})
	if err != nil {
		return IndexRepairReport{}, err
	}
	if plan.Fingerprint != request.PlanFingerprint {
		return IndexRepairReport{}, ErrIndexPlanChanged
	}
	for _, projection := range plan.Projections {
		if (projection.Work.ProviderCalls != 0 || projection.Work.EstimatedCostMicros != 0 || len(projection.Work.Providers) != 0) && !request.AllowProviderWork {
			return IndexRepairReport{}, ErrIndexProviderConsentRequired
		}
	}
	report := IndexRepairReport{PlanFingerprint: plan.Fingerprint,
		Projections: make([]IndexRepairResult, 0, len(plan.Projections))}
	for _, projection := range plan.Projections {
		result, err := coordinator.repairProjection(ctx, projection)
		if err != nil {
			return IndexRepairReport{}, err
		}
		report.Projections = append(report.Projections, result)
	}
	return report, nil
}

func (coordinator *IndexCoordinator) repairProjection(ctx context.Context, plan IndexRepairProjectionPlan) (_ IndexRepairResult, retErr error) {
	target := plan.Target
	coordinator.mu.Lock()
	state := coordinator.runtime[target]
	if state.building {
		coordinator.mu.Unlock()
		return IndexRepairResult{}, ErrIndexRepairInProgress
	}
	state.building, state.failure = true, ""
	coordinator.runtime[target] = state
	coordinator.signalLocked()
	coordinator.mu.Unlock()

	defer func() {
		coordinator.mu.Lock()
		state := coordinator.runtime[target]
		state.building = false
		if retErr != nil {
			state.failure = safeIndexFailure(retErr)
		} else {
			state.failure = ""
		}
		coordinator.runtime[target] = state
		coordinator.signalLocked()
		coordinator.mu.Unlock()
	}()

	coordinator.mu.Lock()
	backend := coordinator.backends[target]
	coordinator.mu.Unlock()
	request := IndexBuildRequest{Target: target, SourceWatermark: plan.SourceWatermark,
		AuthorizationWatermark: plan.AuthorizationWatermark}
	candidate, err := backend.Build(ctx, request)
	if err != nil {
		return IndexRepairResult{}, err
	}
	if candidate.Target != target || candidate.Generation == "" || !utf8.ValidString(candidate.Generation) ||
		len(candidate.Generation) > 512 || strings.ContainsAny(candidate.Generation, "\x00\r\n") {
		return IndexRepairResult{}, ErrIndexManifestCorrupt
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinator.cleanupTimeout)
		defer cancel()
		if err := backend.Discard(cleanupCtx, candidate); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("discarding index candidate: %w", err))
		}
	}()
	if err := backend.Validate(ctx, candidate); err != nil {
		return IndexRepairResult{}, err
	}
	current, err := backend.Inspect(ctx)
	if err != nil {
		return IndexRepairResult{}, err
	}
	if current.Target != target || validateIndexSnapshot(current) != nil {
		return IndexRepairResult{}, ErrIndexManifestCorrupt
	}
	if !current.Enabled || !current.Authorized {
		return IndexRepairResult{}, ErrIndexPermissionChanged
	}
	if current.AuthorizationWatermark != request.AuthorizationWatermark {
		return IndexRepairResult{}, ErrIndexPermissionChanged
	}
	if current.SourceWatermark != request.SourceWatermark {
		return IndexRepairResult{}, ErrIndexSourceChanged
	}
	if err := ctx.Err(); err != nil {
		return IndexRepairResult{}, err
	}
	published, err := backend.Commit(ctx, candidate, request)
	if err != nil {
		return IndexRepairResult{}, err
	}
	committed = true
	if published.Target != target || published.Generation != candidate.Generation || validateIndexSnapshot(published) != nil || !published.Authorized || !IndexCurrent(published.SourceWatermark, published.IndexWatermark) {
		return IndexRepairResult{}, ErrIndexManifestCorrupt
	}
	return IndexRepairResult{Target: target, PreviousGeneration: plan.CurrentGeneration,
		Generation: published.Generation, SourceWatermark: published.SourceWatermark}, nil
}

func (coordinator *IndexCoordinator) signalLocked() {
	close(coordinator.changed)
	coordinator.changed = make(chan struct{})
}
