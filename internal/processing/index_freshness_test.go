package processing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestIndexCurrent(t *testing.T) {
	if IndexCurrent(3, 2) {
		t.Fatal("stale index called current")
	}
	if !IndexCurrent(3, 3) {
		t.Fatal("matching watermark stale")
	}
}

func TestIndexStatusStatesAndSafeFailures(t *testing.T) {
	tests := []struct {
		name     string
		snapshot IndexProjectionSnapshot
		inspect  error
		want     IndexState
		serving  bool
		reason   IndexFailureReason
	}{
		{name: "queryable", snapshot: indexSnapshot(IndexLexical, 4, 4), want: IndexStateQueryable, serving: true},
		{name: "stale", snapshot: indexSnapshot(IndexMetadata, 4, 3), want: IndexStateStale, serving: true},
		{name: "disabled", snapshot: IndexProjectionSnapshot{Target: IndexTarget{Kind: IndexMap}, Enabled: false}, want: IndexStateDisabled},
		{name: "corrupt", snapshot: indexSnapshot(IndexTag, 4, 4), inspect: ErrIndexManifestCorrupt, want: IndexStateFailed, reason: IndexFailureCorruptManifest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeIndexBackend{target: test.snapshot.Target, snapshot: test.snapshot, inspectErr: test.inspect}
			coordinator := newTestIndexCoordinator(t, backend)
			report, err := coordinator.Status(t.Context(), IndexStatusRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Projections) != 1 {
				t.Fatalf("got %d projections", len(report.Projections))
			}
			status := report.Projections[0]
			if status.State != test.want || status.Serving != test.serving || status.FailureReason != test.reason {
				t.Fatalf("status = %#v", status)
			}
			if test.inspect != nil && status.Generation != "" {
				t.Fatalf("corrupt generation disclosed as %q", status.Generation)
			}
		})
	}
}

func TestIndexStatusRequiresFreshWithinBoundedWait(t *testing.T) {
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, snapshot: indexSnapshot(IndexLexical, 8, 7)}
	coordinator := newTestIndexCoordinator(t, backend)

	started := time.Now()
	_, err := coordinator.Status(t.Context(), IndexStatusRequest{RequireFresh: true, Wait: 25 * time.Millisecond})
	if !errors.Is(err, ErrIndexFreshnessTimeout) {
		t.Fatalf("got %v, want freshness timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("fresh wait exceeded bound: %s", elapsed)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		backend.setSnapshot(indexSnapshot(IndexLexical, 8, 8))
	}()
	report, err := coordinator.Status(t.Context(), IndexStatusRequest{RequireFresh: true, Wait: time.Second})
	if err != nil || !report.Fresh {
		t.Fatalf("fresh report = %#v, %v", report, err)
	}

	backend.setSnapshot(indexSnapshot(IndexLexical, 9, 8))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = coordinator.Status(ctx, IndexStatusRequest{RequireFresh: true, Wait: time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

func TestIndexStatusPropagatesBackendCancellation(t *testing.T) {
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, inspectErr: context.Canceled}
	coordinator := newTestIndexCoordinator(t, backend)
	_, err := coordinator.Status(t.Context(), IndexStatusRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
}

func TestIndexRepairPlanIsTargetedDeterministicAndConsentBound(t *testing.T) {
	lexical := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, snapshot: indexSnapshot(IndexLexical, 5, 4),
		work: IndexRepairWork{Documents: 3, Bytes: 100}}
	vector := &fakeIndexBackend{target: IndexTarget{Kind: IndexVector, Key: "space-a"}, snapshot: indexSnapshotTarget(IndexTarget{Kind: IndexVector, Key: "space-a"}, 5, 4),
		work: IndexRepairWork{Documents: 3, Bytes: 300, ProviderCalls: 1, EstimatedCostMicros: 25, CostCurrency: "USD",
			Providers: []IndexProviderDisclosure{{Processor: "local-runtime", Model: "synthetic-model", DataClasses: []string{"derived_text"}}}}}
	coordinator := newTestIndexCoordinator(t, vector, lexical)
	request := IndexRepairPlanRequest{Targets: []IndexTarget{vector.target, lexical.target}}
	first, err := coordinator.PlanRepair(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{lexical.target, vector.target}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == "" || first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprints differ: %q %q", first.Fingerprint, second.Fingerprint)
	}
	if len(first.Projections) != 2 || first.Projections[1].Work.EstimatedCostMicros != 25 || len(first.Projections[1].Work.Providers) != 1 {
		t.Fatalf("plan omits provider/cost preview: %#v", first)
	}
	vectorPlan, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{vector.target}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Repair(t.Context(), IndexRepairRequest{Targets: []IndexTarget{vector.target}, PlanFingerprint: vectorPlan.Fingerprint})
	if !errors.Is(err, ErrIndexProviderConsentRequired) {
		t.Fatalf("got %v, want provider consent", err)
	}
	if lexical.buildCount() != 0 || vector.buildCount() != 0 {
		t.Fatal("work started before provider consent")
	}

	vector.setSnapshot(indexSnapshotTarget(vector.target, 6, 4))
	_, err = coordinator.Repair(t.Context(), IndexRepairRequest{Targets: []IndexTarget{vector.target}, PlanFingerprint: vectorPlan.Fingerprint, AllowProviderWork: true})
	if !errors.Is(err, ErrIndexPlanChanged) {
		t.Fatalf("got %v, want changed plan", err)
	}
}

func TestIndexRepairPlanRequiresExplicitCostCurrency(t *testing.T) {
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexVector, Key: "space-a"},
		snapshot: indexSnapshotTarget(IndexTarget{Kind: IndexVector, Key: "space-a"}, 2, 1),
		work:     IndexRepairWork{EstimatedCostMicros: 1}}
	coordinator := newTestIndexCoordinator(t, backend)
	_, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{backend.target}})
	if !errors.Is(err, ErrInvalidIndexRequest) {
		t.Fatalf("got %v, want explicit currency error", err)
	}
}

func TestIndexRepairCommitsOneTargetPerRequest(t *testing.T) {
	lexical := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, snapshot: indexSnapshot(IndexLexical, 2, 1)}
	metadata := &fakeIndexBackend{target: IndexTarget{Kind: IndexMetadata}, snapshot: indexSnapshot(IndexMetadata, 2, 1)}
	coordinator := newTestIndexCoordinator(t, lexical, metadata)
	plan, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{lexical.target, metadata.target}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Repair(t.Context(), IndexRepairRequest{
		Targets: []IndexTarget{lexical.target, metadata.target}, PlanFingerprint: plan.Fingerprint,
	})
	if !errors.Is(err, ErrInvalidIndexRequest) {
		t.Fatalf("got %v, want one-target bound", err)
	}
	if lexical.buildCount() != 0 || metadata.buildCount() != 0 {
		t.Fatal("multi-target request started work")
	}
}

func TestIndexRepairBuildsValidatesAndCutsOver(t *testing.T) {
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, snapshot: indexSnapshot(IndexLexical, 7, 6),
		work: IndexRepairWork{Documents: 2, Bytes: 50}}
	coordinator := newTestIndexCoordinator(t, backend)
	plan, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{backend.target}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := coordinator.Repair(t.Context(), IndexRepairRequest{Targets: []IndexTarget{backend.target}, PlanFingerprint: plan.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Projections) != 1 || report.Projections[0].PreviousGeneration != "generation-6" || report.Projections[0].Generation != "generation-7" {
		t.Fatalf("repair report = %#v", report)
	}
	if backend.validations != 1 || backend.commits != 1 {
		t.Fatalf("validate=%d commit=%d", backend.validations, backend.commits)
	}
	status, err := coordinator.Status(t.Context(), IndexStatusRequest{})
	if err != nil || status.Projections[0].State != IndexStateQueryable {
		t.Fatalf("status = %#v, %v", status, err)
	}
}

func TestIndexRepairFailuresKeepHealthyGeneration(t *testing.T) {
	tests := []struct {
		name      string
		buildErr  error
		validate  error
		beforeCut func(*fakeIndexBackend)
		want      error
	}{
		{name: "failed build", buildErr: ErrIndexBuildFailed, want: ErrIndexBuildFailed},
		{name: "full disk", buildErr: ErrIndexStorageFull, want: ErrIndexStorageFull},
		{name: "invalid candidate", validate: ErrIndexManifestCorrupt, want: ErrIndexManifestCorrupt},
		{name: "source changed", beforeCut: func(backend *fakeIndexBackend) {
			backend.setSnapshot(indexSnapshot(IndexLexical, 12, 10))
		}, want: ErrIndexSourceChanged},
		{name: "permission changed", beforeCut: func(backend *fakeIndexBackend) {
			snapshot := indexSnapshot(IndexLexical, 11, 10)
			snapshot.Authorized = false
			backend.setSnapshot(snapshot)
		}, want: ErrIndexPermissionChanged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, snapshot: indexSnapshot(IndexLexical, 11, 10),
				work: IndexRepairWork{Documents: 1}, buildErr: test.buildErr, validateErr: test.validate, beforeCommit: test.beforeCut}
			coordinator := newTestIndexCoordinator(t, backend)
			plan, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{backend.target}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = coordinator.Repair(t.Context(), IndexRepairRequest{Targets: []IndexTarget{backend.target}, PlanFingerprint: plan.Fingerprint})
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
			if backend.commits != 0 {
				t.Fatalf("candidate committed after failure")
			}
			if got := backend.currentSnapshot().Generation; got != "generation-10" {
				t.Fatalf("serving generation changed to %q", got)
			}
		})
	}
}

func TestIndexRepairCancellationBeforeCutoverKeepsHealthyGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, snapshot: indexSnapshot(IndexLexical, 4, 3),
		beforeCommit: func(*fakeIndexBackend) { cancel() }}
	coordinator := newTestIndexCoordinator(t, backend)
	plan, err := coordinator.PlanRepair(ctx, IndexRepairPlanRequest{Targets: []IndexTarget{backend.target}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Repair(ctx, IndexRepairRequest{Targets: []IndexTarget{backend.target}, PlanFingerprint: plan.Fingerprint})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	if backend.commits != 0 || backend.discards != 1 || backend.currentSnapshot().Generation != "generation-3" {
		t.Fatalf("commit=%d discard=%d snapshot=%#v", backend.commits, backend.discards, backend.currentSnapshot())
	}
}

func TestIndexStatusBuildingServesOldGeneration(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexLexical}, snapshot: indexSnapshot(IndexLexical, 3, 2),
		build: func(ctx context.Context, request IndexBuildRequest) (IndexCandidate, error) {
			close(started)
			select {
			case <-ctx.Done():
				return IndexCandidate{}, ctx.Err()
			case <-release:
				return IndexCandidate{Target: request.Target, Generation: "generation-3"}, nil
			}
		}}
	coordinator := newTestIndexCoordinator(t, backend)
	plan, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{backend.target}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := coordinator.Repair(t.Context(), IndexRepairRequest{Targets: []IndexTarget{backend.target}, PlanFingerprint: plan.Fingerprint})
		done <- err
	}()
	<-started
	status, err := coordinator.Status(t.Context(), IndexStatusRequest{})
	if err != nil || status.Projections[0].State != IndexStateBuilding || !status.Projections[0].Serving || status.Projections[0].Generation != "generation-2" {
		t.Fatalf("building status = %#v, %v", status, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIndexRestartReusesVerifiedGenerationWithoutRebuild(t *testing.T) {
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexVector, Key: "space-a"},
		snapshot: indexSnapshotTarget(IndexTarget{Kind: IndexVector, Key: "space-a"}, 6, 6)}
	first := newTestIndexCoordinator(t, backend)
	report, err := first.Reconcile(t.Context())
	if err != nil || !report.Fresh {
		t.Fatalf("first reconcile = %#v, %v", report, err)
	}
	second := newTestIndexCoordinator(t, backend)
	report, err = second.Reconcile(t.Context())
	if err != nil || !report.Fresh || backend.buildCount() != 0 {
		t.Fatalf("restart reconcile = %#v, %v, builds=%d", report, err, backend.buildCount())
	}
}

func TestIndexCoordinatorDiscoversNewVectorSpacesWithoutRestart(t *testing.T) {
	base := &fakeIndexBackend{
		target:   IndexTarget{Kind: IndexMap},
		snapshot: IndexProjectionSnapshot{Target: IndexTarget{Kind: IndexMap}},
	}
	vector := &fakeIndexBackend{
		target:   IndexTarget{Kind: IndexVector, Key: "space-a"},
		snapshot: indexSnapshotTarget(IndexTarget{Kind: IndexVector, Key: "space-a"}, 1, 1),
	}
	available := false
	coordinator, err := NewIndexCoordinator(IndexCoordinatorConfig{
		Backends: []IndexProjectionBackend{base},
		Discover: func(context.Context) ([]IndexProjectionBackend, error) {
			if !available {
				return nil, nil
			}
			return []IndexProjectionBackend{vector}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := coordinator.Status(t.Context(), IndexStatusRequest{})
	if err != nil || len(first.Projections) != 1 {
		t.Fatalf("initial status = %#v, %v", first, err)
	}
	available = true
	second, err := coordinator.Status(t.Context(), IndexStatusRequest{})
	if err != nil || len(second.Projections) != 2 || second.Projections[1].Target != vector.target {
		t.Fatalf("discovered status = %#v, %v", second, err)
	}
}

func TestIndexRepairRetriesPreviouslyUnparseableNewRevision(t *testing.T) {
	backend := &fakeIndexBackend{target: IndexTarget{Kind: IndexMetadata}, snapshot: IndexProjectionSnapshot{
		Target: IndexTarget{Kind: IndexMetadata}, Enabled: true, Authorized: true,
		SourceWatermark: 20, IndexWatermark: 19, Generation: "generation-19", FailureReason: IndexFailureBuildFailed,
	}}
	backend.setSnapshot(indexSnapshot(IndexMetadata, 21, 19))
	coordinator := newTestIndexCoordinator(t, backend)
	plan, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{Targets: []IndexTarget{backend.target}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Repair(t.Context(), IndexRepairRequest{Targets: []IndexTarget{backend.target}, PlanFingerprint: plan.Fingerprint})
	if err != nil || backend.currentSnapshot().IndexWatermark != 21 {
		t.Fatalf("repair = %v, snapshot=%#v", err, backend.currentSnapshot())
	}
}

func TestIndexRepairPlanCanReplaceCorruptServingManifest(t *testing.T) {
	backend := &fakeIndexBackend{
		target: IndexTarget{Kind: IndexLexical},
		snapshot: IndexProjectionSnapshot{
			Target: IndexTarget{Kind: IndexLexical}, Enabled: true, Authorized: true,
			Generation: "generation-4", SourceWatermark: 5, IndexWatermark: 4,
			AuthorizationWatermark: 1, Coverage: IndexCoverage{Expected: 1, Indexed: 1},
			FailureReason: IndexFailureCorruptManifest,
		},
	}
	coordinator := newTestIndexCoordinator(t, backend)
	plan, err := coordinator.PlanRepair(t.Context(), IndexRepairPlanRequest{
		Targets: []IndexTarget{backend.target},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Repair(t.Context(), IndexRepairRequest{
		Targets: []IndexTarget{backend.target}, PlanFingerprint: plan.Fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.currentSnapshot().Generation != "generation-5" {
		t.Fatalf("corrupt generation was not replaced: %#v", backend.currentSnapshot())
	}
}

func newTestIndexCoordinator(t *testing.T, backends ...IndexProjectionBackend) *IndexCoordinator {
	t.Helper()
	coordinator, err := NewIndexCoordinator(IndexCoordinatorConfig{
		Backends: backends, MaxFreshWait: time.Second, PollInterval: time.Millisecond, CleanupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func indexSnapshot(kind IndexProjectionKind, source, index uint64) IndexProjectionSnapshot {
	return indexSnapshotTarget(IndexTarget{Kind: kind}, source, index)
}

func indexSnapshotTarget(target IndexTarget, source, index uint64) IndexProjectionSnapshot {
	return IndexProjectionSnapshot{Target: target, Enabled: true, Authorized: true,
		Generation: "generation-" + indexString(index), SourceWatermark: source, IndexWatermark: index,
		AuthorizationWatermark: 1, Coverage: IndexCoverage{Expected: 4, Indexed: 4}}
}

func indexString(value uint64) string {
	if value < 10 {
		return string(rune('0' + value))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}

type fakeIndexBackend struct {
	mu           sync.Mutex
	target       IndexTarget
	snapshot     IndexProjectionSnapshot
	work         IndexRepairWork
	inspectErr   error
	buildErr     error
	validateErr  error
	build        func(context.Context, IndexBuildRequest) (IndexCandidate, error)
	beforeCommit func(*fakeIndexBackend)
	builds       int
	validations  int
	commits      int
	discards     int
}

func (backend *fakeIndexBackend) Target() IndexTarget { return backend.target }

func (backend *fakeIndexBackend) Inspect(context.Context) (IndexProjectionSnapshot, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.snapshot, backend.inspectErr
}

func (backend *fakeIndexBackend) Preview(context.Context, IndexProjectionSnapshot) (IndexRepairWork, error) {
	return backend.work, nil
}

func (backend *fakeIndexBackend) Build(ctx context.Context, request IndexBuildRequest) (IndexCandidate, error) {
	backend.mu.Lock()
	backend.builds++
	build, buildErr, beforeCommit := backend.build, backend.buildErr, backend.beforeCommit
	backend.mu.Unlock()
	var candidate IndexCandidate
	var err error
	if build != nil {
		candidate, err = build(ctx, request)
	} else if buildErr != nil {
		err = buildErr
	} else {
		candidate = IndexCandidate{Target: request.Target, Generation: "generation-" + indexString(request.SourceWatermark)}
	}
	if err == nil && beforeCommit != nil {
		beforeCommit(backend)
	}
	return candidate, err
}

func (backend *fakeIndexBackend) Validate(context.Context, IndexCandidate) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.validations++
	return backend.validateErr
}

func (backend *fakeIndexBackend) Commit(_ context.Context, candidate IndexCandidate, request IndexBuildRequest) (IndexProjectionSnapshot, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.commits++
	backend.snapshot.Generation = candidate.Generation
	backend.snapshot.IndexWatermark = request.SourceWatermark
	backend.snapshot.FailureReason = ""
	return backend.snapshot, nil
}

func (backend *fakeIndexBackend) Discard(context.Context, IndexCandidate) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.discards++
	return nil
}

func (backend *fakeIndexBackend) setSnapshot(snapshot IndexProjectionSnapshot) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.snapshot = snapshot
}

func (backend *fakeIndexBackend) currentSnapshot() IndexProjectionSnapshot {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.snapshot
}

func (backend *fakeIndexBackend) buildCount() int {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.builds
}
