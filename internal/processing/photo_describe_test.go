package processing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type photoDescriptionScenario struct {
	sourceType     string
	action         string
	status         int
	reason         string
	retryCalls     int
	beforeDispatch func()
}

func TestPhotoDescriptionSharesCallAcrossVisibleVersions(t *testing.T) {
	t.Parallel()
	runPhotoDescriptionCases(t, []string{"sibling_excluded"}, runPhotoDescriptionWaiterExclusion)
	for _, hidden := range []bool{false, true} {
		t.Run(strconv.FormatBool(hidden), func(t *testing.T) {
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
			ctx := t.Context()
			duplicate, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "shared.jpg", f.written.Hash, f.written.Size, "image/jpeg", processingBlobPhysical(t, f.written))
			require.NoError(t, err)
			second, err := f.catalog.PhotoAssetForNode(ctx, duplicate.ID)
			require.NoError(t, err)
			_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, duplicate.CurrentVersionID, f.recipe)
			require.NoError(t, err)
			profile := f.service.profiles["vision"]
			require.NoError(t, f.service.grantProfileConsent(ctx, profile, nil))
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			if hidden {
				require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
				second, err = f.catalog.SetPhotoAssetHidden(ctx, second.ID, second.Revision, true)
				require.NoError(t, err)
				require.NotNil(t, second.HiddenAt)
				eligible, err := f.catalog.PhotoDescriptionEligible(ctx, f.node.CurrentVersionID)
				require.NoError(t, err)
				require.True(t, eligible, "the selected visible copy remains eligible beside its hidden identical-byte sibling")
				eligible, err = f.catalog.PhotoDescriptionEligible(ctx, duplicate.CurrentVersionID)
				require.NoError(t, err)
				require.False(t, eligible)
			}

			var jobID string
			nodes := []store.Node{f.node, duplicate}
			if hidden {
				nodes = nodes[:1]
			}
			for _, node := range nodes {
				version, err := f.catalog.ContentVersionByID(ctx, node.CurrentVersionID)
				require.NoError(t, err)
				job, _, err := f.service.enqueueRendition(ctx, node, version, profile, f.service.renditionConsentRequest(profile), "")
				require.NoError(t, err)
				if jobID != "" {
					require.Equal(t, jobID, job.ID)
				}
				jobID = job.ID
			}
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: f.service.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: f.service.clock})
			require.NoError(t, err)
			processed, err := worker.RunJob(ctx, jobID)
			require.NoError(t, err)
			require.True(t, processed)
			require.Equal(t, 1, f.descriptionCalls)
			assets := []store.PhotoAsset{f.asset, second}
			if hidden {
				assets = assets[:1]
			}
			ids := []string{}
			for _, asset := range assets {
				values, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
				require.NoError(t, err)
				require.Len(t, values, 1)
				require.Equal(t, "A white panorama.", values[0].Value.Caption)
				ids = append(ids, values[0].ID)
			}
			if hidden {
				token, _, err := f.catalog.UnlockPhotoHidden(ctx, "synthetic-passcode")
				require.NoError(t, err)
				values, err := f.catalog.PhotoSuggestions(store.WithPhotoHiddenToken(ctx, token), second.ID)
				require.NoError(t, err)
				require.Empty(t, values, "processing the visible copy must leave its hidden sibling without a suggestion")
			} else {
				require.NotEqual(t, ids[0], ids[1])
			}
		})
	}
}

func TestPhotoDescriptionSelectedWaiterSurvivesAcceptanceAndDuplicateEnqueue(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: "selected_waiter"})
	ctx := t.Context()
	selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
	plan, err := f.service.Plan(ctx, selector)
	require.NoError(t, err)
	_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	original, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
	require.NoError(t, err)
	require.Len(t, original, 1)

	profile := f.profile
	profile.Rendition.DeploymentFingerprint = processingHash("replacement-vision-deployment")
	replacement, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"vision": {Profile: profile, RenditionProvider: f.provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: f.server.URL}}}})
	require.NoError(t, err)
	current := replacement.profiles["vision"]
	require.NoError(t, replacement.grantProfileConsent(ctx, current, nil))
	require.NoError(t, replacement.ProbeMediaOrigins(ctx))
	version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	job, selected, err := replacement.enqueueRendition(ctx, f.node, version, current, replacement.renditionConsentRequest(current), "")
	require.NoError(t, err)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: replacement.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: replacement.clock})
	require.NoError(t, err)
	var release sync.Once
	defer release.Do(func() { close(f.probeReleased) })
	finished := make(chan error, 1)
	go func() {
		_, err := worker.RunJob(ctx, job.ID)
		finished <- err
	}()
	select {
	case <-f.probeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("description did not reach the provider")
	}
	prepared, err := f.catalog.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: []string{f.asset.ID}})
	require.NoError(t, err)
	accepted, err := f.catalog.RespondPhotoSuggestion(ctx, original[0].ID, store.PhotoSuggestionResponse{Revision: original[0].Revision, Response: "accepted", Caption: true, Targets: []store.PhotoAuthoredEditTarget{prepared.Targets[0].PhotoAuthoredEditTarget}})
	require.NoError(t, err)
	joined, waiter, err := replacement.enqueueRendition(ctx, f.node, version, current, replacement.renditionConsentRequest(current), "")
	require.NoError(t, err)
	assert.Equal(t, job.ID, joined.ID)
	assert.Equal(t, selected.ID, waiter.ID)
	assert.Equal(t, "waiting", waiter.State, "acceptance must leave the selected provider call authorized to publish")
	release.Do(func() { close(f.probeReleased) })
	require.NoError(t, <-finished)
	completed, err := f.catalog.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, completed.State)
	values, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, accepted.ID, values[0].ID)
	require.Equal(t, "accepted", values[0].Response)
	require.Equal(t, accepted.Revision, values[0].Revision)
	asset, err := f.catalog.PhotoAssetByID(ctx, f.asset.ID)
	require.NoError(t, err)
	require.Equal(t, "A white panorama.", asset.Files[0].Caption)
}

func TestPhotoDescriptionStartConsentRefreshesReadinessBeforeEnqueue(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
	ctx := t.Context()
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	require.Zero(t, f.calls)
	selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
	plan, err := f.service.Plan(ctx, selector)
	require.NoError(t, err)
	announced := false
	_, err = f.service.StartWithProgress(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, func(Job) {
		announced = true
		assert.Equal(t, "ready", f.service.Profiles()[0].ProbeState)
		assert.Equal(t, 1, f.calls, "consent probes before the worker starts")
		assert.Zero(t, f.descriptionCalls)
	})
	require.NoError(t, err)
	require.True(t, announced)
	require.Equal(t, 2, f.calls, "the worker reuses readiness, then sends one description")
	require.Equal(t, 1, f.descriptionCalls)
	require.Equal(t, "ready", f.service.Profiles()[0].ProbeState)
}

func TestPhotoDescriptionProbeReadiness(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: "concurrent_probe"})
	ctx := t.Context()
	require.NoError(t, f.service.grantProfileConsent(ctx, f.service.profiles["vision"], nil))
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() { assert.NoError(t, f.service.ProbeMediaOrigins(ctx)) })
	}
	group.Wait()
	require.Equal(t, 1, f.calls)
	for _, summary := range f.service.Profiles() {
		require.Equal(t, "ready", summary.ProbeState)
	}
}

func TestPhotoDescriptionProbeClassificationAndRetry(t *testing.T) {
	t.Parallel()
	for _, scenario := range []photoDescriptionScenario{
		{action: "probe_auth", status: 401, reason: "Check the provider credential.", retryCalls: 1},
		{action: "probe_rate", status: 429, reason: "Provider rate limit reached. Retry later.", retryCalls: 1},
		{action: "probe_capacity_delay", status: 503, reason: "Provider unavailable. Check connectivity and retry.", retryCalls: 1},
		{action: "probe_policy", status: 400, reason: "Check the provider model and endpoint.", retryCalls: 1},
		{action: "probe_malformed", status: 0, reason: "Provider returned an invalid response. Check model compatibility.", retryCalls: 1},
	} {
		t.Run(scenario.action, func(t *testing.T) { runPhotoDescriptionProbeClassification(t, scenario) })
	}
}
func TestPhotoDescriptionConsentAndAuthorityFences(t *testing.T) {
	t.Parallel()
	runPhotoDescriptionCases(t, []string{"hide_before", "regrant"}, runPhotoDescriptionConsentFence)
}
func TestPhotoDescriptionRejectsRevokedSelectedEgress(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: "publish"})
	ctx := t.Context()
	duplicate, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "duplicate.jpg", f.written.Hash, f.written.Size, "image/jpeg", processingBlobPhysical(t, f.written))
	require.NoError(t, err)
	otherAsset, err := f.catalog.PhotoAssetForNode(ctx, duplicate.ID)
	require.NoError(t, err)
	_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, duplicate.CurrentVersionID, f.recipe)
	require.NoError(t, err)
	other, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: f.service.gate, SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-other", Scope: "document-processing", Profiles: f.profiles})
	require.NoError(t, err)
	configured := f.service.profiles["vision"]
	require.NoError(t, f.service.grantProfileConsent(ctx, configured, nil))
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	require.NoError(t, other.grantProfileConsent(ctx, other.profiles["vision"], nil))
	require.NoError(t, other.ProbeMediaOrigins(ctx))
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	job, first, err := f.service.enqueueRendition(ctx, f.node, version, configured, f.service.renditionConsentRequest(configured), "")
	require.NoError(t, err)
	version, err = f.catalog.ContentVersionByID(ctx, duplicate.CurrentVersionID)
	require.NoError(t, err)
	shared, second, err := other.enqueueRendition(ctx, duplicate, version, other.profiles["vision"], other.renditionConsentRequest(other.profiles["vision"]), "")
	require.NoError(t, err)
	require.Equal(t, job.ID, shared.ID)

	// Pause between actual provider egress and publication, while the worker's
	// claim still owns the result. Both principals initially have consent.
	claim, err := f.catalog.ClaimRenditionJob(ctx, job.ID, "description-publication", time.Now().UTC(), time.Minute)
	require.NoError(t, err)
	work, err := f.catalog.RenditionJobWorkByClaim(ctx, claim, time.Now().UTC())
	require.NoError(t, err)
	execution, err := f.service.renditions.Prepare(ctx, renditionProviderRuntimeWork(work), time.Now().UTC())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, execution.Upload.Close()) })
	snapshot, err := document.SealRenditionExecutionAt(time.Now().UTC(), execution.Provider, execution.Upload, execution.Authorization, execution.EvidencePolicy, execution.RenditionPolicy)
	require.NoError(t, err)
	snapshot, err = renditionExecutionSnapshotForWork(work, snapshot)
	require.NoError(t, err)
	_, fence, err := f.catalog.BeginRenditionProviderEgress(ctx, claim, work.Waiter.ID, time.Now().UTC(), snapshot)
	require.NoError(t, err)
	t.Cleanup(fence.Close)
	result, err := document.RenderRendition(ctx, renditionProviderWithEgressFence(execution.Provider, fence), execution.Upload, snapshot.Authorization)
	require.NoError(t, err)
	require.Equal(t, 1, f.descriptionCalls)

	selected, pending := first, second
	selectedService, retryService := f.service, other
	retryNode, retryAsset := duplicate, otherAsset
	if work.Waiter.ID == second.ID {
		selected, pending = second, first
		selectedService, retryService = other, f.service
		retryNode, retryAsset = f.node, f.asset
	}
	_, err = selectedService.RevokeConsent(ctx)
	require.NoError(t, err)
	err = f.catalog.PublishPhotoDescription(ctx, claim, result, time.Now().UTC())
	require.ErrorIs(t, err, store.ErrProcessingConsentRevoked)
	for _, assetID := range []string{f.asset.ID, otherAsset.ID} {
		suggestions, err := f.catalog.PhotoSuggestions(ctx, assetID)
		require.NoError(t, err)
		require.Empty(t, suggestions)
	}
	selected, err = f.catalog.RenditionJobWaiterByID(ctx, selected.ID)
	require.NoError(t, err)
	require.Equal(t, "rejected", selected.State)
	require.Equal(t, store.RenditionFailureConsent, selected.FailureCode)
	pending, err = f.catalog.RenditionJobWaiterByID(ctx, pending.ID)
	require.NoError(t, err)
	require.Equal(t, "waiting", pending.State)
	require.Empty(t, pending.FailureCode)
	require.Equal(t, 1, f.descriptionCalls, "refusing publication must not repeat provider egress")

	require.NoError(t, f.catalog.MarkRenditionJobFailed(ctx, claim, store.RenditionFailureConsent, time.Now().UTC()))
	failed, err := f.catalog.RenditionJobByID(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionPhaseQueued, failed.Phase)
	retryProfile := retryService.profiles["vision"]
	require.NoError(t, retryService.ProbeMediaOrigins(ctx))
	version, err = f.catalog.ContentVersionByID(ctx, retryNode.CurrentVersionID)
	require.NoError(t, err)
	reopened, retryWaiter, err := retryService.enqueueRendition(ctx, retryNode, version, retryProfile, retryService.renditionConsentRequest(retryProfile), "")
	require.NoError(t, err)
	require.Equal(t, shared.ID, reopened.ID)
	_, err = retryService.runRenditionJob(ctx, reopened.ID, retryWaiter.ID)
	require.NoError(t, err)
	suggestions, err := f.catalog.PhotoSuggestions(ctx, retryAsset.ID)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.Equal(t, 2, f.descriptionCalls)
	require.NoError(t, f.catalog.ValidateMetadata(ctx))
}

func TestPhotoDescriptionPublication(t *testing.T) {
	t.Parallel()
	runPhotoDescriptionCases(t, []string{"publish"}, runPhotoDescriptionPublication)
}
func TestPhotoDescriptionCoverageAfterDuplicateFailure(t *testing.T) {
	t.Parallel()
	runPhotoDescriptionCases(t, []string{
		"duplicate_failed", "duplicate_failed_empty",
		"duplicate_ambiguous", "duplicate_ambiguous_empty",
	}, runPhotoDescriptionDuplicateFailure)
}

func TestPhotoDescriptionDeploymentBackfill(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct{ name, action string }{
		{"changed_deployment_refreshes_pending_suggestion", "deployment"},
		{"changed_model_preserves_dismissed_suggestion", "model_dismissed"},
		{"changed_model_preserves_accepted_suggestion", "model_accepted"},
		{"evidence_policy_change_reuses_description", "evidence_policy"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runPhotoDescriptionDeployment(t, photoDescriptionScenario{action: scenario.action})
		})
	}
}

func runPhotoDescriptionCases(t *testing.T, actions []string, run func(*testing.T, photoDescriptionScenario)) {
	t.Helper()
	for _, action := range actions {
		t.Run(action, func(t *testing.T) { run(t, photoDescriptionScenario{action: action}) })
	}
}

func TestPhotoDescriptionPairedProposalsRefresh(t *testing.T) {
	t.Parallel()
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty_%t", empty), func(t *testing.T) {
			action := "pair_refresh"
			if empty {
				action = "pair_refresh_empty"
			}
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: action})
			ctx := t.Context()
			written, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(mediatest.JPEG(32, 32, color.Black)))
			require.NoError(t, err)
			companion, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "companion.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
			require.NoError(t, err)
			_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, companion.CurrentVersionID, f.recipe)
			require.NoError(t, err)
			for _, node := range []store.Node{f.node, companion} {
				selector := Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "vision"}
				plan, err := f.service.Plan(ctx, selector)
				require.NoError(t, err)
				_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
				require.NoError(t, err)
			}
			original, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
			require.NoError(t, err)
			require.Len(t, original, 1)
			other, err := f.catalog.PhotoAssetForNode(ctx, companion.ID)
			require.NoError(t, err)
			retired, err := f.catalog.PhotoSuggestions(ctx, other.ID)
			require.NoError(t, err)
			require.Len(t, retired, 1)
			_, err = f.catalog.DetachPhotoFile(ctx, other.ID, other.Revision, other.Files[0].ID, store.PhotoDetachOptions{})
			require.NoError(t, err)
			asset, err := f.catalog.AttachPhotoFile(ctx, f.asset.ID, f.asset.Revision, companion.ID, store.PhotoRoleImage, nil)
			require.NoError(t, err)
			for _, file := range asset.Files {
				if file.NodeID == f.node.ID {
					asset, err = f.catalog.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &file.ID)
					require.NoError(t, err)
					break
				}
			}
			provider, err := vision.New(vision.Profile{Origin: f.server.URL, Model: "replacement-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, f.server.Client())
			require.NoError(t, err)
			profile := f.profile
			profile.Rendition.Descriptor.Fingerprint = provider.Descriptor().Fingerprint
			profile.Rendition.DisclosureFingerprint = vision.DisclosureFingerprint(provider.Descriptor(), f.server.URL, profile.Rendition.DeploymentFingerprint)
			replacement, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: f.server.URL}}}})
			require.NoError(t, err)
			selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
			plan, err := replacement.Plan(ctx, selector)
			require.NoError(t, err)
			_, err = replacement.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
			require.NoError(t, err, "pairing must not make replacement violate the version uniqueness constraint")
			values, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			if empty {
				require.Empty(t, values)
				prepared, err := f.catalog.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: []string{asset.ID}})
				require.NoError(t, err)
				_, err = f.catalog.RespondPhotoSuggestion(ctx, original[0].ID, store.PhotoSuggestionResponse{Revision: original[0].Revision, Response: "accepted", Caption: true, Targets: []store.PhotoAuthoredEditTarget{prepared.Targets[0].PhotoAuthoredEditTarget}})
				require.ErrorIs(t, err, store.ErrNotFound)
			} else {
				require.Len(t, values, 1)
				require.Equal(t, original[0].ID, values[0].ID)
				require.Equal(t, original[0].Revision+1, values[0].Revision)
				require.Equal(t, "HARBOR", values[0].Value.VisibleText)
			}
			for _, text := range []string{"COAST", "LIGHTHOUSE", "HARBOR"} {
				parsed, err := query.Parse([]byte(fmt.Sprintf(`{"text":%q}`, text)))
				require.NoError(t, err)
				page, err := f.catalog.ListPhotoAssets(ctx, store.PhotoBrowseRequest{Query: parsed}, nil)
				require.NoError(t, err)
				if !empty && text == "HARBOR" {
					require.Len(t, page.Items, 1)
					require.True(t, page.Items[0].MatchedAISuggestion)
				} else {
					require.Empty(t, page.Items)
				}
			}

			asset, err = f.catalog.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &other.Files[0].ID)
			require.NoError(t, err)
			selector = Selector{NodeID: companion.ID, ContentVersionID: companion.CurrentVersionID, Profile: "vision"}
			plan, err = replacement.Plan(ctx, selector)
			require.NoError(t, err)
			_, err = replacement.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
			require.NoError(t, err)
			values, err = f.catalog.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			if empty {
				require.Empty(t, values)
			} else {
				require.Len(t, values, 1)
				require.Equal(t, retired[0].ID, values[0].ID)
				require.Equal(t, retired[0].Revision+1, values[0].Revision)
			}
			require.Equal(t, 4, f.descriptionCalls)
		})
	}
}

type photoDescriptionFixture struct {
	catalog          *store.Store
	databasePath     string
	blobs            *blob.Store
	written          blob.WriteReceipt
	node             store.Node
	collectionID     string
	asset            store.PhotoAsset
	recipe           document.VisualPreviewRecipeV1
	service          *Service
	provider         *vision.Provider
	profile          document.ProcessingProfileV1
	profiles         map[string]ProfileConfig
	server           *httptest.Server
	probeStarted     chan struct{}
	probeReleased    chan struct{}
	calls            int
	descriptionCalls int
	excluded         *store.PhotoAsset
	clockOffset      time.Duration
}

func newPhotoDescriptionFixture(t *testing.T, scenario photoDescriptionScenario) *photoDescriptionFixture {
	t.Helper()
	f := &photoDescriptionFixture{}
	action := scenario.action
	modelChange := action == "model_dismissed" || action == "model_accepted"
	deploymentCase := action == "deployment" || modelChange || action == "selected_waiter" || strings.HasPrefix(action, "pair_refresh")
	authInvalidationCase := action == "auth_invalidate"
	duplicateFailureCase := strings.HasPrefix(action, "duplicate_failed")
	duplicateAmbiguousCase := strings.HasPrefix(action, "duplicate_ambiguous")
	duplicateCase := duplicateFailureCase || duplicateAmbiguousCase
	emptyDescription := strings.HasSuffix(action, "_empty")
	coverageCase := action == "publish" || duplicateCase
	ctx := t.Context()
	f.databasePath = filepath.Join(t.TempDir(), "vault.db")
	catalog, err := store.Open(f.databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	source := syntheticJPEGSegment(t, mediatest.JPEG(2048, 8, color.White), 0xe1, append([]byte("Exif\x00\x00"), syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 6)}, nil)...))
	source = syntheticJPEGSegment(t, source, 0xe1, []byte("http://ns.adobe.com/xap/1.0/\x00synthetic-private-marker"))
	mediaType, filename := "image/jpeg", "synthetic-photo.jpg"
	switch scenario.sourceType {
	case "png":
		source, mediaType, filename = mediatest.PNG(8, 2048, color.White), "image/png", "synthetic-photo.png"
	case "gif":
		source, mediaType, filename = mediatest.GIF(8, 2048, 1), "image/gif", "synthetic-photo.gif"
	}
	written, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(source))
	require.NoError(t, err)
	var node store.Node
	var collectionID string
	if coverageCase {
		run, err := catalog.BeginIngest(ctx, "cli", "Synthetic photo coverage")
		require.NoError(t, err)
		collectionID = run.ID()
		node, err = catalog.IngestFileExact(ctx, run, catalog.RootID(), filename, written.Hash, written.Size, mediaType, filename, "", processingBlobPhysical(t, written))
		require.NoError(t, err)
	} else {
		node, err = catalog.CreateFile(ctx, catalog.RootID(), filename, written.Hash, written.Size, mediaType, processingBlobPhysical(t, written))
		require.NoError(t, err)
	}
	asset, err := catalog.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	recipe, _ := VisualPreviewRecipeForSize("grid")
	preview, err := EnsureVisualPreview(ctx, catalog, blobs, node.CurrentVersionID, recipe)
	require.NoError(t, err)
	_ = preview
	var service *Service
	probeStarted := make(chan struct{})
	probeReleased := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if action == "recover" && f.calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if status := scenario.status; status != 0 {
			if action == "probe_capacity_delay" {
				w.Header().Set("Retry-After", "1200")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte("synthetic-private-provider-details"))
			return
		}
		if action == "probe_malformed" {
			_, _ = w.Write([]byte("synthetic-private-provider-details"))
			return
		}
		var request struct {
			Messages []struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL *struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		imageURL := request.Messages[0].Content[1].ImageURL.URL
		data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageURL, "data:image/jpeg;base64,"))
		assert.NoError(t, err)
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		assert.NoError(t, err)
		isProbe := strings.Contains(request.Messages[0].Content[0].Text, "left and right")
		if isProbe && action == "blocked_probe" {
			close(probeStarted)
			select {
			case <-probeReleased:
			case <-r.Context().Done():
				return
			}
		}

		if !isProbe {
			assert.LessOrEqual(t, max(config.Width, config.Height), 1024)
			f.descriptionCalls++
			if action == "selected_waiter" && f.descriptionCalls == 2 {
				close(probeStarted)
				select {
				case <-probeReleased:
				case <-r.Context().Done():
					return
				}
			}

			if action == "authorization_expires_after_dispatch" {
				<-r.Context().Done()
				close(probeStarted)
				return
			}
			if duplicateAmbiguousCase && f.descriptionCalls == 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if deploymentCase && max(config.Width, config.Height) <= 32 {
				assert.LessOrEqual(t, config.Width, 32)
				assert.LessOrEqual(t, config.Height, 32)
			} else {
				assert.Equal(t, 1024, max(config.Width, config.Height))
				assert.NotEqual(t, source, data)
				assert.NotContains(t, string(data), "synthetic-private-marker")
				assert.NotContains(t, string(data), "Exif")
				assert.Equal(t, 4, config.Width)
				assert.Equal(t, 1024, config.Height)
			}
		}
		if authInvalidationCase && !isProbe && f.calls == 2 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.excluded != nil && !isProbe {
			_, err := catalog.SetPhotoAssetExcluded(ctx, f.excluded.ID, f.excluded.Revision, true)
			assert.NoError(t, err)
		}

		result := `{"caption":"A white panorama.","tags":["panorama"],"visible_text":"HELLO"}`
		if strings.HasPrefix(action, "pair_refresh") && !isProbe {
			result = `{"caption":"A harbor.","tags":[],"visible_text":"HARBOR"}`
			if f.descriptionCalls <= 2 {
				result = `{"caption":"A coast.","tags":[],"visible_text":"COAST"}`
				if f.descriptionCalls == 2 {
					result = `{"caption":"A lighthouse.","tags":[],"visible_text":"LIGHTHOUSE"}`
				}
			} else if emptyDescription {
				result = `{"caption":"","tags":[],"visible_text":""}`
			}
		}
		if emptyDescription && !strings.HasPrefix(action, "pair_refresh") {
			result = `{"caption":"","tags":[],"visible_text":""}`
		}
		if action == "malformed_description" || duplicateFailureCase && f.descriptionCalls >= 2 && f.descriptionCalls <= 4 {
			result = `{"caption":"incomplete"}`
		}
		if strings.HasPrefix(action, "retry_") && !isProbe && f.descriptionCalls == 1 {
			result = `{"caption":"incomplete"}`
			if action == "retry_revoked" {
				_, err := service.RevokeConsent(ctx)
				assert.NoError(t, err)
			}
			if action == "retry_hidden" {
				_, err := catalog.SetPhotoAssetHidden(ctx, f.asset.ID, f.asset.Revision, true)
				assert.NoError(t, err)
			}
		}
		if isProbe {
			result = "red,blue"
		}
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": result}, "finish_reason": "stop"}}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	if scenario.beforeDispatch != nil {
		client.Transport = photoDispatchTransport{next: client.Transport, before: scenario.beforeDispatch}
	}
	provider, err := vision.New(vision.Profile{Origin: server.URL, Model: "synthetic-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, client)
	require.NoError(t, err)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(workerProcessingProfile(t, provider.Descriptor()).CanonicalProfile, &profile))
	profile.Rendition.AdapterContract = vision.AdapterContract
	profile.Rendition.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
	profile.RetentionDisclosure.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	profiles := map[string]ProfileConfig{"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: server.URL}}}
	if action == "concurrent_probe" || action == "probe_capacity_delay" {
		profiles["vision-copy"] = profiles["vision"]
	}
	service, err = NewService(ServiceConfig{Clock: func() time.Time { return time.Now().UTC().Add(f.clockOffset) }, Catalog: catalog, Blobs: blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: profiles})
	require.NoError(t, err)
	f.catalog = catalog
	f.blobs = blobs
	f.written = written
	f.node = node
	f.collectionID = collectionID
	f.asset = asset
	f.recipe = recipe
	f.service = service
	f.provider = provider
	f.profile = profile
	f.profiles = profiles
	f.server = server
	f.probeStarted = probeStarted
	f.probeReleased = probeReleased
	return f
}

func runPhotoDescriptionProbeClassification(t *testing.T, scenario photoDescriptionScenario) {
	t.Helper()
	f := newPhotoDescriptionFixture(t, scenario)
	ctx := t.Context()
	action := scenario.action

	require.NoError(t, f.service.grantProfileConsent(ctx, f.service.profiles["vision"], nil))
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	summaries := f.service.Profiles()
	require.Len(t, summaries, len(f.profiles))
	for _, summary := range summaries {
		require.Equal(t, "provider_unavailable", summary.ProbeState)
	}
	require.Equal(t, scenario.reason, summaries[0].ProbeReason)
	require.NotContains(t, summaries[0].ProbeReason, "synthetic-private-provider-details")
	if action == "probe_rate" {
		return
	}
	selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
	plan, err := f.service.Plan(ctx, selector)
	require.NoError(t, err)
	_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.ErrorIs(t, err, ErrRenditionRuntimeUnavailable)
	wantCalls := scenario.retryCalls
	require.Equal(t, wantCalls, f.calls)
	require.NotEmpty(t, f.service.Profiles()[0].ProbeFailureCode)
	if action == "probe_capacity_delay" {
		require.NoError(t, f.service.ProbeMediaOrigins(ctx))
		require.Equal(t, 1, f.calls, "shared runtime retains its retry hint for both profiles")
		f.clockOffset = 11 * time.Minute
		_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
		require.ErrorIs(t, err, ErrRenditionRuntimeUnavailable)
		require.Equal(t, 1, f.calls, "longer provider retry hints are honored")
	}
	if action == "probe_auth" {
		f.clockOffset = 20 * time.Minute
		require.NoError(t, f.service.ProbeMediaOrigins(ctx))
		require.Equal(t, wantCalls+1, f.calls, "authentication probe retries after its delay")
	}
}

func runPhotoDescriptionConsentFence(t *testing.T, scenario photoDescriptionScenario) {
	t.Helper()
	f := newPhotoDescriptionFixture(t, scenario)
	ctx := t.Context()
	asset := f.asset
	action := scenario.action

	configured := f.service.profiles["vision"]
	require.NoError(t, f.service.grantProfileConsent(ctx, configured, nil))
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	f.calls = 0
	version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	job, waiter, err := f.service.enqueueRendition(ctx, f.node, version, configured, f.service.renditionConsentRequest(configured), "")
	require.NoError(t, err)
	if action == "regrant" {
		_, err = f.service.RevokeConsent(ctx)
	} else {
		require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
		_, err = f.catalog.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	}
	require.NoError(t, err)
	_, err = f.service.runRenditionJob(ctx, job.ID, waiter.ID)
	if action == "regrant" {
		require.ErrorIs(t, err, ErrConsentRequired)
	} else {
		require.ErrorIs(t, err, ErrPlanChanged)
	}
	require.Zero(t, f.calls)
	failed, err := f.catalog.RenditionJobWaiterByID(ctx, waiter.ID)
	require.NoError(t, err)
	if action == "regrant" {
		require.Equal(t, store.RenditionFailureConsent, failed.FailureCode)
	} else {
		current, err := f.catalog.RenditionJobByID(ctx, job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionFailureStaleAuthority, current.FailureCode)
	}
	if action == "regrant" {
		require.NoError(t, f.service.grantProfileConsent(ctx, configured, nil))
		require.NoError(t, f.service.ProbeMediaOrigins(ctx))
		versions, err := f.catalog.PhotoDescriptionTargets(ctx, configured.record.Fingerprint, "", 50)
		require.NoError(t, err)
		require.Len(t, versions, 1)
		require.NoError(t, f.service.ProbeMediaOrigins(ctx))
		version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
		require.NoError(t, err)
		_, renewed, err := f.service.enqueueRendition(ctx, f.node, version, configured, f.service.renditionConsentRequest(configured), "")
		require.NoError(t, err)
		_, err = f.service.runRenditionJob(ctx, job.ID, renewed.ID)
		require.NoError(t, err)
		proposals, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
		require.NoError(t, err)
		require.Len(t, proposals, 1)
	}
}

func runPhotoDescriptionWaiterExclusion(t *testing.T, scenario photoDescriptionScenario) {
	t.Helper()
	f := newPhotoDescriptionFixture(t, scenario)
	ctx := t.Context()
	written := f.written
	asset := f.asset

	duplicate, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "duplicate.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
	require.NoError(t, err)
	other, err := f.catalog.PhotoAssetForNode(ctx, duplicate.ID)
	require.NoError(t, err)
	_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, duplicate.CurrentVersionID, f.recipe)
	require.NoError(t, err)
	configured := f.service.profiles["vision"]
	require.NoError(t, f.service.grantProfileConsent(ctx, configured, nil))
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	f.calls = 0
	version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	job, _, err := f.service.enqueueRendition(ctx, f.node, version, configured, f.service.renditionConsentRequest(configured), "")
	require.NoError(t, err)
	version, err = f.catalog.ContentVersionByID(ctx, duplicate.CurrentVersionID)
	require.NoError(t, err)
	siblingJob, _, err := f.service.enqueueRendition(ctx, duplicate, version, configured, f.service.renditionConsentRequest(configured), "")
	require.NoError(t, err)
	require.Equal(t, job.ID, siblingJob.ID)
	selected, excluded := asset, other
	runtime := &afterPrepareRenditionRuntime{RenditionRuntime: f.service.renditions, afterPrepare: func(work store.RenditionJobWork) {
		if work.Waiter.ContentVersionID == duplicate.CurrentVersionID {
			selected, excluded = other, asset
		}
		f.excluded = &excluded
	}}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: runtime, Gate: newTestOperationGate(), Owner: "exclusion-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: f.service.clock})
	require.NoError(t, err)
	processed, err := worker.RunJob(ctx, job.ID)
	require.NoError(t, err)
	require.True(t, processed)
	proposals, err := f.catalog.PhotoSuggestions(ctx, selected.ID)
	require.NoError(t, err)
	require.Len(t, proposals, 1)
	proposals, err = f.catalog.PhotoSuggestions(ctx, excluded.ID)
	require.NoError(t, err)
	require.Empty(t, proposals)
	require.Equal(t, 1, f.calls)
}

func runPhotoDescriptionPublication(t *testing.T, scenario photoDescriptionScenario) {
	t.Helper()
	f := newPhotoDescriptionFixture(t, scenario)
	ctx := t.Context()
	asset := f.asset
	selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
	plan, err := f.service.Plan(ctx, selector)
	require.NoError(t, err)
	require.Equal(t, []string{"derived_upload"}, plan.Flow[0].InputClasses)
	require.Equal(t, 1, plan.Estimate.ProviderCalls)
	require.Equal(t, "Consent queues all eligible photos in the vault for description. Pending photo suggestions rebuild after restore; accepted and dismissed decisions are included in backups.", plan.BackupConsequence)
	require.Equal(t, []string{"photo_suggestion"}, plan.RetainedClasses)
	require.Empty(t, plan.Flow[0].RuntimeDisclosure.RetainedArtifactRoles)
	_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrConsentRequired)
	require.Zero(t, f.calls)
	var enqueued Job
	completed, err := f.service.StartWithProgress(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, func(job Job) { enqueued = job })
	require.NoError(t, err)

	require.NotEmpty(t, enqueued.ID)
	require.NotEmpty(t, enqueued.RenditionJobID)
	assert.Equal(t, enqueued.ID, completed.ID)
	assert.Equal(t, enqueued.RenditionJobID, completed.RenditionJobID)
	assert.Empty(t, enqueued.AttachmentID, "photo descriptions have no rendition attachment at enqueue")
	assert.Empty(t, completed.AttachmentID, "photo descriptions have no rendition attachment after publication")

	suggestions, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.Equal(t, "pending", suggestions[0].Response)
	require.Equal(t, "A white panorama.", suggestions[0].Value.Caption)
	_, err = f.catalog.ActiveRendition(ctx, f.node.CurrentVersionID, f.service.profiles["vision"].record.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound)
	targets, err := f.catalog.PhotoDescriptionTargets(ctx, f.service.profiles["vision"].record.Fingerprint, "", 50)
	require.NoError(t, err)
	require.Empty(t, targets)
}

func runPhotoDescriptionDuplicateFailure(t *testing.T, scenario photoDescriptionScenario) {
	t.Helper()
	f := newPhotoDescriptionFixture(t, scenario)
	ctx := t.Context()
	written := f.written
	asset := f.asset
	provider := f.provider
	profile := f.profile
	action := scenario.action
	duplicateFailureCase := strings.HasPrefix(action, "duplicate_failed")
	duplicateAmbiguousCase := strings.HasPrefix(action, "duplicate_ambiguous")
	emptyDescription := strings.HasSuffix(action, "_empty")
	selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
	plan, err := f.service.Plan(ctx, selector)
	require.NoError(t, err)
	_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	suggestions, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	if emptyDescription {
		require.Empty(t, suggestions)
	} else {
		require.Len(t, suggestions, 1)
		require.Equal(t, "pending", suggestions[0].Response)
	}

	configured := f.service.profiles["vision"]
	version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	published, originalWaiter, err := f.service.enqueueRendition(ctx, f.node, version, configured, f.service.renditionConsentRequest(configured), "")
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, published.State)
	require.Equal(t, "published", originalWaiter.State)
	duplicate, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "duplicate.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
	require.NoError(t, err)
	require.NotEqual(t, f.node.CurrentVersionID, duplicate.CurrentVersionID)
	other, err := f.catalog.PhotoAssetForNode(ctx, duplicate.ID)
	require.NoError(t, err)
	_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, duplicate.CurrentVersionID, f.recipe)
	require.NoError(t, err)
	version, err = f.catalog.ContentVersionByID(ctx, duplicate.CurrentVersionID)
	require.NoError(t, err)
	shared, sibling, err := f.service.enqueueRendition(ctx, duplicate, version, configured, f.service.renditionConsentRequest(configured), "")
	require.NoError(t, err)
	require.Equal(t, published.ID, shared.ID)
	require.NotEqual(t, originalWaiter.ID, sibling.ID)
	require.Equal(t, "waiting", sibling.State)
	require.Equal(t, store.RenditionJobQueued, shared.State)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: f.service.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
	require.NoError(t, err)
	processed, err := worker.RunJob(ctx, shared.ID)
	require.NoError(t, err)
	require.True(t, processed)
	failed, err := f.catalog.RenditionJobByID(ctx, shared.ID)
	require.NoError(t, err)
	if duplicateAmbiguousCase {
		require.Equal(t, store.RenditionJobOperatorRequired, failed.State)
		require.Equal(t, store.RenditionFailureAmbiguous, failed.FailureCode)
		require.Equal(t, 2, f.descriptionCalls)
		version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
		require.NoError(t, err)
		joined, retainedWaiter, err := f.service.enqueueRendition(ctx, f.node, version, configured, f.service.renditionConsentRequest(configured), "")
		require.NoError(t, err)
		require.Equal(t, published.ID, joined.ID)
		require.Equal(t, store.RenditionJobOperatorRequired, joined.State)
		require.Equal(t, "published", retainedWaiter.State)
		require.Equal(t, 2, f.descriptionCalls)
		retryPlan, err := f.service.Plan(ctx, selector)
		require.NoError(t, err)
		_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: retryPlan.Fingerprint})
		require.NoError(t, err)
		current, err := f.catalog.RenditionJobByID(ctx, shared.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobOperatorRequired, current.State)
		require.Equal(t, store.RenditionFailureAmbiguous, current.FailureCode)
		require.Equal(t, 2, f.descriptionCalls, "retrying the published photo does not resubmit the quarantined duplicate")
		duplicateStatus, err := f.service.Status(ctx, sibling.ID)
		require.NoError(t, err)
		require.Equal(t, "operator_required", duplicateStatus.State)
		require.Equal(t, "ambiguous", duplicateStatus.FailureCode)
	} else {
		require.Equal(t, store.RenditionJobFailed, failed.State)
		require.Equal(t, 3, f.descriptionCalls)
		rejected, err := f.catalog.RenditionJobWaiterByID(ctx, sibling.ID)
		require.NoError(t, err)
		require.Equal(t, "rejected", rejected.State)
		require.Equal(t, store.RenditionFailureTerminal, rejected.FailureCode)
		status, err := f.service.Status(ctx, sibling.ID)
		require.NoError(t, err)
		require.Equal(t, "failed", status.State)
		require.Equal(t, "provider", status.Phase)
		require.Equal(t, "terminal", status.FailureCode)
		_, err = f.service.runRenditionJob(ctx, shared.ID, sibling.ID)
		require.ErrorIs(t, err, ErrRenditionFailed)
		_, err = f.service.runRenditionJob(ctx, published.ID, originalWaiter.ID)
		require.NoError(t, err)
	}

	retained, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	require.Equal(t, suggestions, retained)
	retainedWaiter, err := f.catalog.RenditionJobWaiterByID(ctx, originalWaiter.ID)
	require.NoError(t, err)
	require.Equal(t, "published", retainedWaiter.State)
	values, err := f.catalog.PhotoSuggestions(ctx, other.ID)
	require.NoError(t, err)
	require.Empty(t, values)
	coverage, err := f.catalog.ProcessingCoverage(ctx, store.ProcessingCoverageScope{ContentVersionIDs: []string{f.node.CurrentVersionID}, ProcessingProfileFingerprint: configured.record.Fingerprint})
	require.NoError(t, err)
	assert.Equal(t, store.ProcessingClassCoverage{Name: "rendition", State: "complete", Complete: 1, Total: 1}, coverage.Renditions)
	collection, err := f.catalog.CollectionQuality(ctx, f.collectionID, store.CoverageSelection{Configuration: "configured", ProfileFingerprint: configured.record.Fingerprint}, nil)
	require.NoError(t, err)
	wantCounts := store.CoverageCounts{Complete: 1}
	if emptyDescription {
		wantCounts = store.CoverageCounts{None: 1}
	}
	assert.Equal(t, wantCounts, *collection.Collection.Coverage.Counts)
	if duplicateFailureCase {
		coverage, err = f.catalog.ProcessingCoverage(ctx, store.ProcessingCoverageScope{ContentVersionIDs: []string{duplicate.CurrentVersionID}, ProcessingProfileFingerprint: configured.record.Fingerprint})
		require.NoError(t, err)
		assert.Equal(t, store.ProcessingClassCoverage{Name: "rendition", State: "unavailable", Unavailable: 1, Total: 1}, coverage.Renditions)
	}
	if !duplicateAmbiguousCase || emptyDescription {
		targets, err := f.catalog.PhotoDescriptionTargets(ctx,
			configured.record.Fingerprint, "", 50)
		require.NoError(t, err)
		require.Empty(t, targets)
		return
	}

	var metadata bytes.Buffer
	require.NoError(t, f.catalog.ExportMetadata(ctx, &metadata))
	restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(metadata.Bytes())))
	rebuilt, err := NewService(ServiceConfig{Catalog: restored, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: f.server.URL}}}})
	require.NoError(t, err)
	require.NoError(t, rebuilt.grantProfileConsent(ctx, rebuilt.profiles["vision"], nil))
	require.NoError(t, rebuilt.ProbeMediaOrigins(ctx))
	targets, err := restored.PhotoDescriptionTargets(ctx,
		rebuilt.profiles["vision"].record.Fingerprint, "", 50)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, f.node.CurrentVersionID, targets[0].ID)
	backfillPhotoDescription(t, rebuilt)
	quarantined, err := restored.RenditionJobByID(ctx, shared.ID)
	require.NoError(t, err)
	require.Equal(t, store.RenditionJobCompleted, quarantined.State)
	ambiguousStatus, err := rebuilt.Status(ctx, sibling.ID)
	require.NoError(t, err)
	require.Equal(t, "operator_required", ambiguousStatus.State)
	require.Equal(t, "ambiguous", ambiguousStatus.FailureCode)
	suggestions, err = restored.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.Equal(t, "pending", suggestions[0].Response)
	require.Equal(t, 3, f.descriptionCalls, "restore rebuilds the successful photo without retrying its ambiguous sibling")
	duplicateValues, err := restored.PhotoSuggestions(ctx, other.ID)
	require.NoError(t, err)
	require.Empty(t, duplicateValues)
}

func runPhotoDescriptionDeployment(t *testing.T, scenario photoDescriptionScenario) {
	t.Helper()
	f := newPhotoDescriptionFixture(t, scenario)
	ctx := t.Context()
	asset := f.asset
	provider := f.provider
	profile := f.profile
	action := scenario.action
	modelChange := action == "model_dismissed" || action == "model_accepted"
	selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
	plan, err := f.service.Plan(ctx, selector)
	require.NoError(t, err)
	_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	suggestions, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.Equal(t, "pending", suggestions[0].Response)
	if action == "evidence_policy" {
		profile.EvidenceLexical.NormalizerFingerprint = processingHash("replacement-normalizer")
		replacement, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: f.server.URL}}}})
		require.NoError(t, err)
		require.Equal(t, f.service.profiles["vision"].record.RenditionRequestFingerprint, replacement.profiles["vision"].record.RenditionRequestFingerprint)
		require.NoError(t, replacement.grantProfileConsent(ctx, replacement.profiles["vision"], nil))
		require.NoError(t, replacement.ProbeMediaOrigins(ctx))
		plan, err = replacement.Plan(ctx, selector)
		require.NoError(t, err)
		require.False(t, plan.ConsentRequired)
		version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
		require.NoError(t, err)
		queued, waiter, err := replacement.enqueueRendition(ctx, f.node, version, replacement.profiles["vision"], replacement.renditionConsentRequest(replacement.profiles["vision"]), "")
		require.NoError(t, err)
		require.Equal(t, "published", waiter.State)
		require.Equal(t, store.RenditionJobCompleted, queued.State)
		buildCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		job, err := replacement.Start(buildCtx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
		require.NoError(t, err)
		stored, err := f.catalog.RenditionJobByID(ctx, job.RenditionJobID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobCompleted, stored.State)
		require.Equal(t, 1, f.descriptionCalls, "existing suggestions satisfy evidence-only changes")
		return
	}

	old := suggestions[0]
	if modelChange {
		response := store.PhotoSuggestionResponse{Revision: old.Revision, Response: "dismissed"}
		if action == "model_accepted" {
			prepared, err := f.catalog.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: []string{asset.ID}})
			require.NoError(t, err)
			response.Response = "accepted"
			response.Caption = true
			for _, target := range prepared.Targets {
				response.Targets = append(response.Targets, target.PhotoAuthoredEditTarget)
			}
		}
		_, err = f.catalog.RespondPhotoSuggestion(ctx, old.ID, response)
		require.NoError(t, err)
		targets, err := f.catalog.PhotoDescriptionTargets(ctx, f.service.profiles["vision"].record.Fingerprint, "", 50)
		require.NoError(t, err)
		require.Empty(t, targets, "a decision suppresses further proposals from the same deployment")
	}
	oldRequest := f.service.profiles["vision"].record.RenditionRequestFingerprint
	if modelChange {
		provider, err = vision.New(vision.Profile{Origin: f.server.URL, Model: "replacement-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, f.server.Client())
		require.NoError(t, err)
		profile.Rendition.Descriptor.Fingerprint = provider.Descriptor().Fingerprint
		profile.Rendition.DisclosureFingerprint = vision.DisclosureFingerprint(provider.Descriptor(), f.server.URL, profile.Rendition.DeploymentFingerprint)
	} else {
		profile.Rendition.DeploymentFingerprint = processingHash("replacement-vision-deployment")
	}
	replacement, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: f.server.URL}}}})
	require.NoError(t, err)
	require.NotEqual(t, oldRequest, replacement.profiles["vision"].record.RenditionRequestFingerprint)
	if !modelChange {
		require.Equal(t, f.service.profiles["vision"].provider.Descriptor(), replacement.profiles["vision"].provider.Descriptor())
	}
	// Consent on a new photo also authorizes backfill of existing photos.
	written, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(mediatest.JPEG(16, 16, color.Black)))
	require.NoError(t, err)
	selected, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "selected.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
	require.NoError(t, err)
	_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, selected.CurrentVersionID, f.recipe)
	require.NoError(t, err)
	selector = Selector{NodeID: selected.ID, ContentVersionID: selected.CurrentVersionID, Profile: "vision"}
	plan, err = replacement.Plan(ctx, selector)
	require.NoError(t, err)
	require.True(t, plan.ConsentRequired)
	_, err = replacement.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	targets, err := f.catalog.PhotoDescriptionTargets(ctx, replacement.profiles["vision"].record.Fingerprint, "", 50)
	require.NoError(t, err)
	if modelChange {
		require.Empty(t, targets)
	} else {
		require.Len(t, targets, 1, "a changed producer refreshes the pending proposal")
		require.Equal(t, f.node.CurrentVersionID, targets[0].ID)
	}
	values, err := f.catalog.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, old.ID, values[0].ID)
	require.Equal(t, old.Value, values[0].Value)
	if modelChange {
		if action == "model_accepted" {
			require.Equal(t, "accepted", values[0].Response)
		} else {
			require.Equal(t, "dismissed", values[0].Response)
		}
	} else {
		require.Equal(t, "pending", values[0].Response)
	}

	require.NoError(t, f.catalog.ValidateMetadata(ctx))
}

func backfillPhotoDescription(t *testing.T, service *Service) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, service.ProbeMediaOrigins(ctx))
	drain, cancel := context.WithTimeout(ctx, 2*time.Second)
	originalGate := service.gate
	service.gate = &cancelMediaAfterCommitGate{processingOperationGate: originalGate, cancel: cancel}
	defer func() {
		cancel()
		service.gate = originalGate
	}()
	require.ErrorIs(t, service.PhotoDescriptionBackfill(drain, nil, 1), context.Canceled)
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: service.catalog, Blobs: service.blobs, Runtime: service.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: service.clock})
	require.NoError(t, err)
	processed, err := worker.RunOne(ctx)
	require.NoError(t, err)
	require.True(t, processed)
}

type afterPrepareRenditionRuntime struct {
	RenditionRuntime

	afterPrepare func(store.RenditionJobWork)
}

func (runtime *afterPrepareRenditionRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	execution, err := runtime.RenditionRuntime.Prepare(ctx, work, now)
	if err == nil {
		if runtime.afterPrepare != nil {
			runtime.afterPrepare(work)
		}
	}
	return execution, err
}

func TestPhotoDescriptionPreviewRefusalBeforeProbe(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"missing", "failed", "document", "at_limit", "oversized"} {
		t.Run(state, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
			ctx := t.Context()
			written, name, mime := f.written, "unready.jpg", "image/jpeg"
			reason := "preview_missing"
			if state == "document" {
				var err error
				written, err = f.blobs.WriteDetailedContext(ctx, strings.NewReader("ordinary document"))
				require.NoError(t, err)
				name, mime, reason = "document.txt", "text/plain", "photo_not_eligible"
			}
			node := f.node
			var err error
			if state != "at_limit" && state != "oversized" {
				node, err = f.catalog.CreateFile(ctx, f.catalog.RootID(), name, written.Hash, written.Size, mime, processingBlobPhysical(t, written))
				require.NoError(t, err)
			}
			if state == "failed" {
				raw, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{ContractVersion: document.VisualPreviewContractV1, Recipe: f.recipe, SourceSHA256: node.BlobHash, State: document.VisualPreviewState(state), Failure: &document.VisualPreviewFailureV1{Code: "synthetic_failure", Detail: "Synthetic unavailable preview"}})
				require.NoError(t, err)
				_, err = f.catalog.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, raw, nil)
				require.NoError(t, err)
				reason = "preview_unavailable"
			}
			if state == "at_limit" || state == "oversized" {
				version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
				require.NoError(t, err)
				data, _, _, err := photoDescriptionInput(ctx, f.catalog, f.blobs, version, f.service.profiles["vision"])
				require.NoError(t, err)
				f.profile.Rendition.MaxDocumentBytes = int64(len(data))
				reason = ""
				if state == "oversized" {
					f.profile.Rendition.MaxDocumentBytes--
					reason = "source_bytes_exceeded"
				}
				config := f.profiles["vision"]
				config.Profile = f.profile
				f.service, err = NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"vision": config}})
				require.NoError(t, err)
			}
			selector := Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: "vision"}
			plan, err := f.service.Plan(ctx, selector)
			require.NoError(t, err)
			require.Equal(t, reason, plan.RenditionIneligibleReason)
			if state != "document" {
				// Isolate worker readiness from the synthetic probe at consent grant.
				require.NoError(t, f.service.grantProfileConsent(ctx, f.service.profiles["vision"], nil))
				require.NoError(t, f.service.ProbeMediaOrigins(ctx))
				if state == "at_limit" {
					backfill := f.service.photoDescriptionBackfill(nil, 1)
					targets, err := backfill.List(ctx, "", 50)
					require.NoError(t, err)
					require.Len(t, targets, 1)
					require.NoError(t, backfill.Process(ctx, targets[0]))
				}
				_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
				if state == "at_limit" {
					require.Greater(t, f.written.Size, f.profile.Rendition.MaxDocumentBytes)
					require.NoError(t, err)
					require.Equal(t, 1, f.descriptionCalls)
					values, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
					require.NoError(t, err)
					require.Len(t, values, 1)
					require.Equal(t, f.node.CurrentVersionID, values[0].ContentVersionID)
					return
				}
				require.ErrorIs(t, err, ErrSourceIneligible)
			}
			expectedCalls := 1
			if state == "document" {
				expectedCalls = 0
			}
			require.Equal(t, expectedCalls, f.calls)
		})
	}
}

type photoAdmissionGate struct {
	processingOperationGate

	after func()
}

func (gate *photoAdmissionGate) MutateContext(ctx context.Context, fn func() error) error {
	err := gate.processingOperationGate.MutateContext(ctx, fn)
	if err == nil {
		gate.after()
	}
	return err
}

func TestPhotoDescriptionBackfillSaturationRefills(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
	ctx := t.Context()
	profile := f.service.profiles["vision"]
	require.NoError(t, f.service.grantProfileConsent(ctx, profile, nil))
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	for _, name := range []string{"second.jpg", "third.jpg"} {
		node, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), name, f.written.Hash, f.written.Size, "image/jpeg", processingBlobPhysical(t, f.written))
		require.NoError(t, err)
		_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, node.CurrentVersionID, f.recipe)
		require.NoError(t, err)
	}
	now := time.Now()
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Until(now))
		admitted := make(chan struct{}, 3)
		f.service.gate = &photoAdmissionGate{processingOperationGate: f.service.gate, after: func() { admitted <- struct{}{} }}
		run, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.service.PhotoDescriptionBackfill(run, nil, 1) }()
		<-admitted
		synctest.Wait()
		select {
		case <-admitted:
			t.Fatal("admitted beyond the profile limit")
		default:
		}
		// Let repeated saturated scans reach the ten-second idle cap.
		time.Sleep(20 * time.Second)
		synctest.Wait()
		// Actively claimed work frees a slot; the saturated list must recount
		// within one second instead of returning to the ten-second idle wait.
		_, claimed, err := f.catalog.ClaimNextRenditionJob(t.Context(), "synthetic-worker", time.Now().UTC(), time.Minute)
		require.NoError(t, err)
		require.True(t, claimed)
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-admitted:
		default:
			t.Fatal("saturated profile did not refill within one second")
		}
		count, err := f.catalog.UnclaimedPhotoDescriptionWaiters(t.Context(), profile.record.Fingerprint, time.Now())
		require.NoError(t, err)
		require.Zero(t, count)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestPhotoDescriptionBackfillIdleScansBackOffBeyondTenSeconds(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
	ctx := t.Context()
	require.NoError(t, f.service.grantProfileConsent(ctx, f.service.profiles["vision"], nil))
	require.NoError(t, f.service.ProbeMediaOrigins(ctx))
	_, err := f.catalog.SetPhotoAssetExcluded(ctx, f.asset.ID, f.asset.Revision, true)
	require.NoError(t, err)
	now := time.Now()
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Until(now))
		backfill := f.service.photoDescriptionBackfill(nil, 1)
		list := backfill.List
		var listed []time.Time
		backfill.List = func(ctx context.Context, after string, limit int) ([]photoDescriptionTarget, error) {
			listed = append(listed, time.Now())
			targets, err := list(ctx, after, limit)
			assert.Empty(t, targets)
			return targets, err
		}
		run, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- backfill.Run(run) }()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		require.GreaterOrEqual(t, len(listed), 2)
		require.Greater(t, listed[len(listed)-1].Sub(listed[len(listed)-2]), 10*time.Second, "idle photo scans must back off beyond the generic ten-second ceiling")
	})
}

func TestPhotoDescriptionBackfillAdmissionStaysLocal(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"purged", "unreadable_source"} {
		t.Run(state, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
			ctx := t.Context()
			version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
			require.NoError(t, err)
			profile := f.service.profiles["vision"]
			require.NoError(t, f.service.grantProfileConsent(ctx, profile, nil))
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			var healthy store.Node
			switch state {
			case "purged":
				job, _, err := f.service.enqueueRendition(ctx, f.node, version, profile, f.service.renditionConsentRequest(profile), "")
				require.NoError(t, err)
				_, err = f.catalog.PurgeDerivatives(ctx, store.PurgeRequest{BuildIDs: []string{job.ID}})
				require.NoError(t, err)
			case "unreadable_source":
				written, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(mediatest.JPEG(16, 16, color.Black)))
				require.NoError(t, err)
				healthy, err = f.catalog.CreateFile(ctx, f.catalog.RootID(), "healthy.jpg", written.Hash, written.Size, "image/jpeg", processingBlobPhysical(t, written))
				require.NoError(t, err)
				_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, healthy.CurrentVersionID, f.recipe)
				require.NoError(t, err)
				require.NoError(t, f.blobs.Remove(f.written.Hash))
			}
			backfill := f.service.photoDescriptionBackfill(nil, 2)
			targets, err := backfill.List(ctx, "", 50)
			require.NoError(t, err)
			switch state {
			case "purged":
				require.Len(t, targets, 1)
				require.ErrorIs(t, backfill.Process(ctx, targets[0]), store.ErrRenditionJobStaleAuthority)
			case "unreadable_source":
				require.Len(t, targets, 2)
				for _, target := range targets {
					if target.version.ID == f.node.CurrentVersionID {
						require.ErrorIs(t, backfill.Process(ctx, target), errPhotoDescriptionInput)
					} else {
						require.Equal(t, healthy.CurrentVersionID, target.version.ID)
						require.NoError(t, backfill.Process(ctx, target))
					}
				}
				count, err := f.catalog.UnclaimedPhotoDescriptionWaiters(ctx, profile.record.Fingerprint, time.Now())
				require.NoError(t, err)
				require.Equal(t, 1, count)
				require.Equal(t, 1, f.calls, "only the synthetic probe leaves the machine")
			}
			require.Zero(t, f.descriptionCalls)
		})
	}
}

func TestPhotoDescriptionExpiryAtSubmissionBoundary(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"preview_read", "after_dispatch"} {
		t.Run(phase, func(t *testing.T) {
			action := ""
			if phase == "after_dispatch" {
				action = "authorization_expires_after_dispatch"
			}
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: action})
			ctx := t.Context()
			profile := f.service.profiles["vision"]
			require.NoError(t, f.service.grantProfileConsent(ctx, profile, nil))
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
			require.NoError(t, err)
			job, _, err := f.service.enqueueRendition(ctx, f.node, version, profile, f.service.renditionConsentRequest(profile), "")
			require.NoError(t, err)
			prepared := false
			readStarted := false
			runtime := photoExpiryRuntime{RenditionRuntime: f.service.renditions, prepare: func(execution *RenditionExecution) {
				if prepared {
					return
				}
				prepared = true
				expiry := time.Now().UTC().Add(200 * time.Millisecond)
				execution.Authorization.ExpiresAt = expiry.Format(timestampForm)
				if phase == "preview_read" {
					execution.Upload = &photoExpiryUpload{AuthorizedUpload: execution.Upload, expiry: expiry, started: &readStarted}
				}
			}}
			offset := time.Duration(0)
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: runtime, Gate: newTestOperationGate(), Owner: "expiry-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: func() time.Time { return time.Now().UTC().Add(offset) }})
			require.NoError(t, err)
			processed, err := worker.RunJob(ctx, job.ID)
			require.NoError(t, err)
			require.True(t, processed)
			failed, err := f.catalog.RenditionJobByID(ctx, job.ID)
			require.NoError(t, err)
			if phase == "after_dispatch" {
				<-f.probeStarted
				require.Equal(t, 1, f.descriptionCalls)
				require.Equal(t, store.RenditionJobOperatorRequired, failed.State)
				return
			}
			require.True(t, readStarted)
			require.Zero(t, f.descriptionCalls)
			require.Equal(t, store.RenditionJobRetryWait, failed.State)
			require.Equal(t, store.RenditionFailureTransient, failed.FailureCode)
			offset = time.Minute
			processed, err = worker.RunJob(ctx, job.ID)
			require.NoError(t, err)
			require.True(t, processed)
			suggestions, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
			require.NoError(t, err)
			require.Len(t, suggestions, 1)
			require.Equal(t, 1, f.descriptionCalls)
		})
	}
}

type photoExpiryRuntime struct {
	RenditionRuntime

	prepare func(*RenditionExecution)
}

func (runtime photoExpiryRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	execution, err := runtime.RenditionRuntime.Prepare(ctx, work, now)
	if err == nil {
		runtime.prepare(&execution)
	}
	return execution, err
}

type photoExpiryUpload struct {
	document.AuthorizedUpload

	expiry  time.Time
	started *bool
}

func (upload *photoExpiryUpload) Read(p []byte) (int, error) {
	*upload.started = true
	<-time.After(time.Until(upload.expiry) + time.Millisecond)
	return upload.AuthorizedUpload.Read(p)
}

func TestPhotoDescriptionVaultEstimate(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
	ctx := t.Context()
	node, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), "second.jpg", f.written.Hash, f.written.Size, "image/jpeg", processingBlobPhysical(t, f.written))
	require.NoError(t, err)
	_, err = EnsureVisualPreview(ctx, f.catalog, f.blobs, node.CurrentVersionID, f.recipe)
	require.NoError(t, err)
	plan, err := f.service.Plan(ctx, Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"})
	require.NoError(t, err)
	require.Equal(t, 2, plan.Estimate.ProviderCalls)
	require.Zero(t, f.calls, "estimating consent stays local")
}

func TestPhotoDescriptionProbeDueLifecycle(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"ready", "recover"} {
		t.Run(action, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: action})
			ctx := t.Context()
			clock := f.service.clock
			now := time.Now().UTC()
			f.service.clock = func() time.Time { return now }
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			require.Zero(t, f.calls, "startup waits for consent")
			require.Equal(t, "consent_required", f.service.Profiles()[0].ProbeState)
			excluded, err := f.catalog.SetPhotoAssetExcluded(ctx, f.asset.ID, f.asset.Revision, true)
			require.NoError(t, err)
			selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
			plan, err := f.service.Plan(ctx, selector)
			require.NoError(t, err)
			_, err = f.service.GrantConsent(ctx, ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
			require.NoError(t, err, "provider availability must not reject consent")
			require.Equal(t, 1, f.calls)
			require.Zero(t, f.descriptionCalls, "refresh uses synthetic input without eligible photos")
			plan, err = f.service.Plan(ctx, selector)
			require.NoError(t, err)
			require.False(t, plan.ConsentRequired)
			if action == "recover" {
				require.Equal(t, "provider_unavailable", f.service.Profiles()[0].ProbeState)
				require.NoError(t, f.service.ProbeMediaOrigins(ctx))
				require.Equal(t, 1, f.calls, "the provider retry deadline bounds due work")
				now = now.Add(time.Minute)
				require.NoError(t, f.service.ProbeMediaOrigins(ctx))
				require.Equal(t, 2, f.calls)
			}
			require.Equal(t, "ready", f.service.Profiles()[0].ProbeState)
			require.Empty(t, f.service.Profiles()[0].ProbeReason)
			if action == "recover" {
				_, err = f.catalog.SetPhotoAssetExcluded(ctx, excluded.ID, excluded.Revision, false)
				require.NoError(t, err)
				// Provider authorization uses wall time; advance only the readiness clock.
				f.service.clock = clock
				backfillPhotoDescription(t, f.service)
				require.Equal(t, 3, f.calls)
				proposals, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
				require.NoError(t, err)
				require.Len(t, proposals, 1)
				f.service.clock = func() time.Time { return now }
			}
			calls := f.calls
			now = now.Add(9 * time.Minute)
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			require.Equal(t, calls, f.calls)
			now = now.Add(time.Minute)
			require.Equal(t, "pending", f.service.Profiles()[0].ProbeState)
			require.Equal(t, calls, f.calls, "reading status does not probe")
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			require.Equal(t, calls+1, f.calls)
			_, err = f.service.RevokeConsent(ctx)
			require.NoError(t, err)
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			require.Equal(t, "consent_required", f.service.Profiles()[0].ProbeState)
			require.Equal(t, calls+1, f.calls)
		})
	}
}

func TestPhotoDescriptionFinalEgressAuthority(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"hidden", "excluded", "revoked", "preview_removed", "rejects"} {
		t.Run(action, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{})
			ctx := t.Context()
			require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
			if action == "rejects" {
				prepared, err := f.catalog.PreparePhotoAuthored(ctx, store.PhotoSetSelection{AssetIDs: []string{f.asset.ID}})
				require.NoError(t, err)
				_, err = f.catalog.EditPhotoAuthoredBatch(ctx, store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{prepared.Targets[0].PhotoAuthoredEditTarget}, Patch: store.PhotoAuthoredPatch{Flag: new("reject")}})
				require.NoError(t, err)
				f.asset, err = f.catalog.PhotoAssetByID(ctx, f.asset.ID)
				require.NoError(t, err)
				f.node, err = f.catalog.NodeByID(ctx, f.node.ID)
				require.NoError(t, err)
			}
			profile := f.service.profiles["vision"]
			require.NoError(t, f.service.grantProfileConsent(ctx, profile, nil))
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
			require.NoError(t, err)
			job, _, err := f.service.enqueueRendition(ctx, f.node, version, profile, f.service.renditionConsentRequest(profile), "")
			require.NoError(t, err)
			runtime := &afterPrepareRenditionRuntime{RenditionRuntime: f.service.renditions, afterPrepare: func(store.RenditionJobWork) {
				switch action {
				case "rejects":
					_, err = f.catalog.MovePhotoRejects(ctx, false, []store.PhotoRejectTarget{{AssetID: f.asset.ID, Revision: f.asset.Revision, MemberRevision: f.node.Revision}})
				case "hidden":
					_, err = f.catalog.SetPhotoAssetHidden(ctx, f.asset.ID, f.asset.Revision, true)
				case "excluded":
					_, err = f.catalog.SetPhotoAssetExcluded(ctx, f.asset.ID, f.asset.Revision, true)
				case "revoked":
					_, err = f.service.RevokeConsent(ctx)
				case "preview_removed":
					db, openErr := f.catalog.SQLiteDriver().Open(f.databasePath, docsqlite.OpenOptions{Access: docsqlite.ReadWriteExisting, TransactionMode: docsqlite.Immediate})
					require.NoError(t, openErr)
					_, err = db.ExecContext(ctx, "DELETE FROM visual_preview_generations WHERE source_sha256=?", f.written.Hash)
					require.NoError(t, db.Close())
				}
				require.NoError(t, err)
			}}
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: runtime, Gate: newTestOperationGate(), Owner: "egress-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
			require.NoError(t, err)
			processed, err := worker.RunJob(ctx, job.ID)
			require.NoError(t, err)
			require.True(t, processed)
			require.Zero(t, f.descriptionCalls)
			current, err := f.catalog.RenditionJobByID(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, store.RenditionJobFailed, current.State)
		})
	}
}

func TestPhotoDescriptionProbeStopsWithService(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: "blocked_probe"})
	t.Cleanup(f.service.Stop)
	require.NoError(t, f.service.grantProfileConsent(t.Context(), f.service.profiles["vision"], nil))
	done := make(chan error, 1)
	go func() { done <- f.service.ProbeMediaOrigins(t.Context()) }()
	<-f.probeStarted
	require.NoError(t, f.service.Shutdown(t.Context()))
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, 1, f.calls)
	require.Zero(t, f.descriptionCalls)
}

func TestPhotoDescriptionRevocationDuringProbe(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: "blocked_probe"})
	require.NoError(t, f.service.grantProfileConsent(t.Context(), f.service.profiles["vision"], nil))
	done := make(chan error, 1)
	go func() { done <- f.service.ProbeMediaOrigins(t.Context()) }()
	<-f.probeStarted
	_, err := f.service.RevokeConsent(t.Context())
	require.NoError(t, err)
	close(f.probeReleased)
	require.NoError(t, <-done)
	require.Equal(t, "consent_required", f.service.Profiles()[0].ProbeState)
	require.Equal(t, 1, f.calls)
	require.Zero(t, f.descriptionCalls)
}

func TestPhotoDescriptionRetryRechecksAuthority(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"retry_valid", "retry_revoked", "retry_hidden"} {
		t.Run(action, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: action})
			ctx := t.Context()
			require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
			selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
			plan, err := f.service.Plan(ctx, selector)
			require.NoError(t, err)
			job, err := f.service.StartWithProgress(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true}, nil)
			switch action {
			case "retry_valid":
				require.NoError(t, err)
			case "retry_revoked":
				require.ErrorIs(t, err, ErrConsentRequired)
			case "retry_hidden":
				require.ErrorIs(t, err, ErrPlanChanged)
			}
			if action == "retry_hidden" {
				require.Equal(t, 1, f.descriptionCalls)
				return
			}
			suggestions, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
			require.NoError(t, err)
			if action == "retry_valid" {
				require.Equal(t, 2, f.descriptionCalls)
				current, err := f.catalog.RenditionJobByID(ctx, job.RenditionJobID)
				require.NoError(t, err)
				require.Equal(t, 1, current.ProviderAttempts)
				require.Len(t, suggestions, 1)
			} else {
				require.Equal(t, 1, f.descriptionCalls)
				require.Empty(t, suggestions)
			}
		})
	}
}

func TestPhotoDescriptionEmptyCompletionIsProfileScoped(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: "publish_empty"})
	ctx := t.Context()
	selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
	run := func(service *Service, consent bool) {
		plan, err := service.Plan(ctx, selector)
		require.NoError(t, err)
		_, err = service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: consent})
		require.NoError(t, err)
	}
	run(f.service, true)
	run(f.service, false)
	require.Equal(t, 1, f.descriptionCalls)
	provider, err := vision.New(vision.Profile{Origin: f.server.URL, Model: "replacement-vision", TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, f.server.Client())
	require.NoError(t, err)
	profile := f.profile
	profile.Rendition.Descriptor.Fingerprint = provider.Descriptor().Fingerprint
	profile.Rendition.DisclosureFingerprint = vision.DisclosureFingerprint(provider.Descriptor(), f.server.URL, profile.Rendition.DeploymentFingerprint)
	replacement, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: ctx, Principal: "synthetic-operator", Scope: "document-processing", Profiles: map[string]ProfileConfig{"vision": {Profile: profile, RenditionProvider: provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: f.server.URL}}}})
	require.NoError(t, err)
	t.Cleanup(replacement.Stop)
	run(replacement, true)
	run(replacement, false)
	require.Equal(t, 2, f.descriptionCalls)
	suggestions, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
	require.NoError(t, err)
	require.Empty(t, suggestions)
}

type photoDispatchTransport struct {
	next   http.RoundTripper
	before func()
}

func (transport photoDispatchTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.before()
	return transport.next.RoundTrip(request)
}

func TestPhotoDescriptionHideWaitsForDispatch(t *testing.T) {
	t.Parallel()
	for _, dispatch := range []int{1, 2} {
		t.Run(fmt.Sprintf("request_%d", dispatch), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			calls := 0
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{action: "retry_valid", beforeDispatch: func() {
				calls++
				if calls == dispatch+1 {
					close(started)
					<-release
				}
			}})
			ctx := t.Context()
			require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
			profile := f.service.profiles["vision"]
			require.NoError(t, f.service.grantProfileConsent(ctx, profile, nil))
			require.NoError(t, f.service.ProbeMediaOrigins(ctx))
			version, err := f.catalog.ContentVersionByID(ctx, f.node.CurrentVersionID)
			require.NoError(t, err)
			job, _, err := f.service.enqueueRendition(ctx, f.node, version, profile, f.service.renditionConsentRequest(profile), "")
			require.NoError(t, err)
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: f.service.renditions, Gate: newTestOperationGate(), Owner: "hide-dispatch-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
			require.NoError(t, err)
			finished := make(chan error, 1)
			go func() { _, err := worker.RunJob(ctx, job.ID); finished <- err }()
			<-started
			hidden := make(chan error, 1)
			go func() {
				_, err := f.catalog.SetPhotoAssetHidden(ctx, f.asset.ID, f.asset.Revision, true)
				hidden <- err
			}()
			select {
			case err := <-hidden:
				t.Errorf("hide committed before authorized request dispatch: %v", err)
				unblock()
				<-finished
				return
			case <-time.After(100 * time.Millisecond): //nolint:kennlint // the timer proves hide stays blocked while the authorized request waits for dispatch
			}
			unblock()
			require.NoError(t, <-hidden)
			require.NoError(t, <-finished)
			require.Equal(t, dispatch, f.descriptionCalls)
		})
	}
}

func TestPhotoDescriptionPublishesNonJPEGSource(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"png", "gif"} {
		t.Run(format, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t, photoDescriptionScenario{sourceType: format})
			ctx := t.Context()
			selector := Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: "vision"}
			plan, err := f.service.Plan(ctx, selector)
			require.NoError(t, err)
			_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
			require.NoError(t, err)
			require.Equal(t, 1, f.descriptionCalls)
			suggestions, err := f.catalog.PhotoSuggestions(ctx, f.asset.ID)
			require.NoError(t, err)
			require.Len(t, suggestions, 1)
			require.Equal(t, "A white panorama.", suggestions[0].Value.Caption)
		})
	}
}
