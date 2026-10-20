package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/document/vision/visiontest"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// photoDescriptionFixture is a vault with one photo, a synthetic vision
// endpoint, and a service configured with one description profile.
type photoDescriptionFixture struct {
	catalog *store.Store
	blobs   *blob.Store
	written blob.WriteReceipt
	node    store.Node
	asset   store.PhotoAsset
	recipe  document.VisualPreviewRecipeV1
	service *Service
	profile document.ProcessingProfileV1
	server  *httptest.Server

	mu           sync.Mutex
	probeCalls   int
	describeBody []byte
	describes    int
	probeFails   bool
	reply        string
	onDescribe   func()
	onProbe      func(call int)
}

func newPhotoDescriptionFixture(t *testing.T) *photoDescriptionFixture {
	t.Helper()
	f := &photoDescriptionFixture{reply: `{"caption":"A white panorama.","tags":["panorama","harbor"],"visible_text":"HELLO"}`}
	ctx := t.Context()
	catalog, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(t.TempDir(), "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	written, err := blobs.WriteDetailedContext(ctx, bytes.NewReader(mediatest.JPEG(1024, 768, color.White)))
	require.NoError(t, err)
	run, err := catalog.BeginIngest(ctx, "cli", "Synthetic photo coverage")
	require.NoError(t, err)
	node, err := catalog.IngestFileExact(ctx, run, catalog.RootID(), "synthetic-photo.jpg", written.Hash, written.Size, "image/jpeg", "synthetic-photo.jpg", "", processingBlobPhysical(t, written))
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(ctx, node.ID)
	require.NoError(t, err)
	recipe, _ := VisualPreviewRecipeForSize("grid")
	_, err = EnsureVisualPreview(ctx, catalog, blobs, node.CurrentVersionID, recipe)
	require.NoError(t, err)
	f.server = httptest.NewServer(f.serve(t))
	t.Cleanup(f.server.Close)
	f.catalog, f.blobs, f.written, f.node, f.asset, f.recipe = catalog, blobs, written, node, asset, recipe
	f.service = f.newService(t, "vision", "synthetic-vision")
	return f
}

func (f *photoDescriptionFixture) serve(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		probe := visiontest.IsProbe(body)
		reply, hook, onProbe := f.reply, f.onDescribe, f.onProbe
		switch {
		case probe:
			f.probeCalls++
			reply = "red,blue"
			if onProbe != nil {
				f.mu.Unlock()
				onProbe(f.probeCalls)
				f.mu.Lock()
			}
			if f.probeFails {
				f.mu.Unlock()
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		default:
			f.describes++
			f.describeBody = bytes.Clone(body)
		}
		f.mu.Unlock()
		if !probe && hook != nil {
			hook()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(visiontest.Completion(reply))
	}
}

// newService builds a service with one description profile named name.
func (f *photoDescriptionFixture) newService(t *testing.T, name, model string) *Service {
	t.Helper()
	provider, err := vision.New(vision.Profile{Origin: f.server.URL, Model: model, TrustBoundary: document.RenditionTrustOperatorNetwork, RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20}, nil, f.server.Client())
	require.NoError(t, err)
	var profile document.ProcessingProfileV1
	require.NoError(t, json.Unmarshal(workerProcessingProfile(t, provider.Descriptor()).CanonicalProfile, &profile))
	profile.Rendition.AdapterContract = vision.AdapterContract
	profile.Rendition.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
	profile.RetentionDisclosure.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
	profile.RetentionDisclosure.RetainSanitizedMarkdown = false
	f.profile = profile
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(), SpoolDirectory: t.TempDir(), Lifecycle: t.Context(), Principal: "synthetic-operator", Scope: "document-processing",
		Profiles: map[string]ProfileConfig{name: {Profile: profile, RenditionProvider: provider, RenditionDisclosure: RuntimeDisclosure{Endpoint: f.server.URL}}}})
	require.NoError(t, err)
	return service
}

func (f *photoDescriptionFixture) selector(name string) Selector {
	return Selector{NodeID: f.node.ID, ContentVersionID: f.node.CurrentVersionID, Profile: name}
}

func (f *photoDescriptionFixture) counts() (probes, describes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probeCalls, f.describes
}

// grant records consent for the plan of the service's only profile.
func (f *photoDescriptionFixture) grant(t *testing.T, service *Service, name string) {
	t.Helper()
	plan, err := service.Plan(t.Context(), f.selector(name))
	require.NoError(t, err)
	_, err = service.GrantConsent(t.Context(), ConsentGrantRequest{Selector: f.selector(name), PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
}

// backfill admits what one automatic pass lists and lets a worker run each job.
func (f *photoDescriptionFixture) backfill(t *testing.T, service *Service) int {
	t.Helper()
	ctx := t.Context()
	targets, err := service.photoDescriptionTargets(ctx, nil, "", 50, 4)
	require.NoError(t, err)
	for _, target := range targets {
		require.NoError(t, service.admitPhotoDescription(ctx, target))
	}
	f.runWorker(t, service)
	return len(targets)
}

func (f *photoDescriptionFixture) runWorker(t *testing.T, service *Service) {
	t.Helper()
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: f.catalog, Blobs: f.blobs, Runtime: service.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: service.clock})
	require.NoError(t, err)
	for {
		processed, err := worker.RunOne(t.Context())
		require.NoError(t, err)
		if !processed {
			return
		}
	}
}

func (f *photoDescriptionFixture) suggestions(t *testing.T) []store.PhotoSuggestion {
	t.Helper()
	prepared, err := f.catalog.PreparePhotoAuthored(t.Context(), store.PhotoSetSelection{AssetIDs: []string{f.asset.ID}})
	require.NoError(t, err)
	return prepared.Suggestions
}

func (f *photoDescriptionFixture) dismiss(t *testing.T, suggestion store.PhotoSuggestion) {
	t.Helper()
	_, err := f.catalog.RespondPhotoSuggestion(t.Context(), suggestion.ID, store.PhotoSuggestionResponse{Response: "dismissed"})
	require.NoError(t, err)
}

func TestPhotoDescriptionProfileCoverageIgnoresDescriptionJobs(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	f.grant(t, f.service, "vision")
	targets, err := f.service.photoDescriptionTargets(ctx, nil, "", 50, 4)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.NoError(t, f.service.admitPhotoDescription(ctx, targets[0]), "a description job is waiting")
	fence := SourceFence{VaultUID: f.catalog.VaultID(), ContentVersionIDs: []string{f.node.CurrentVersionID}}

	coverage, err := f.service.Coverage(ctx, "vision", fence)
	require.NoError(t, err)
	require.Equal(t, "complete", coverage.State, "a description profile attaches no rendition to cover")
	require.Zero(t, coverage.Renditions.Rebuilding)
	require.False(t, coverage.Renditions.Required)
}

func TestPhotoDescriptionFlow(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	selector := f.selector("vision")

	plan, err := f.service.Plan(ctx, selector)
	require.NoError(t, err)
	require.Equal(t, []string{"derived_upload"}, plan.Flow[0].InputClasses)
	require.Contains(t, plan.Estimate.Scope, "queues eligible photos throughout this vault")
	require.NotContains(t, plan.BackupConsequence, "throughout this vault", "the backup field describes backups only")
	require.Empty(t, plan.Flow[0].RuntimeDisclosure.RetainedArtifactRoles)
	_, err = f.service.Start(ctx, StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrConsentRequired)
	probes, describes := f.counts()
	require.Zero(t, probes+describes, "nothing leaves the vault before consent")
	require.Zero(t, f.backfill(t, f.service), "automatic work waits for consent")

	f.grant(t, f.service, "vision")
	require.Equal(t, 1, f.backfill(t, f.service))
	probes, describes = f.counts()
	require.Equal(t, 1, probes, "the provider is probed once under the grant")
	require.Equal(t, 1, describes)
	f.mu.Lock()
	var request struct {
		Messages []struct {
			Content []struct {
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(f.describeBody, &request))
	f.mu.Unlock()
	sent, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(request.Messages[0].Content[1].ImageURL.URL, "data:image/jpeg;base64,"))
	require.NoError(t, err)
	config, err := jpeg.DecodeConfig(bytes.NewReader(sent))
	require.NoError(t, err)
	require.LessOrEqual(t, max(config.Width, config.Height), 512, "only the grid preview is sent")
	require.NotContains(t, string(f.describeBody), f.node.Name, "the file name stays home")

	_, err = f.catalog.ActiveRendition(ctx, f.node.CurrentVersionID, f.service.profiles["vision"].record.Fingerprint)
	require.ErrorIs(t, err, store.ErrNotFound, "a description attaches nothing")
	suggestions := f.suggestions(t)
	require.Len(t, suggestions, 1)
	require.Equal(t, document.PhotoDescription{Caption: "A white panorama.", Tags: []string{"panorama", "harbor"}, VisibleText: "HELLO"}, suggestions[0].Value)
}

func TestPhotoDescriptionBackfillLoopAdmitsAndStops(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	f.grant(t, f.service, "vision")
	drain, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	originalGate := f.service.gate
	f.service.gate = &cancelMediaAfterCommitGate{processingOperationGate: originalGate, cancel: cancel}
	defer func() { f.service.gate = originalGate }()
	require.ErrorIs(t, f.service.PhotoDescriptionBackfill(drain, nil, 1), context.Canceled)
	f.runWorker(t, f.service)
	require.Len(t, f.suggestions(t), 1)
}

func TestPhotoDescriptionRenewedConsentWithBlockedProbeSendsNothing(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	f.grant(t, f.service, "vision")
	require.NoError(t, f.service.requireVisionReadiness(ctx, f.service.profiles["vision"]))
	probes, _ := f.counts()
	require.Equal(t, 1, probes, "the first grant has a passing probe")
	_, err := f.service.RevokeConsent(ctx)
	require.NoError(t, err)
	f.mu.Lock()
	f.probeFails = true
	f.mu.Unlock()
	f.grant(t, f.service, "vision")

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for range 3 {
		targets, err := f.service.photoDescriptionTargets(ctx, logger, "", 50, 4)
		require.NoError(t, err)
		require.Empty(t, targets)
	}
	require.Equal(t, 1, strings.Count(logs.String(), "level=WARN"), "the stalled provider is logged once, not on every pass: %s", logs.String())
	require.Contains(t, logs.String(), "profile=vision")

	require.Zero(t, f.backfill(t, f.service), "the renewed grant has no passing probe")
	plan, err := f.service.Plan(ctx, f.selector("vision"))
	require.NoError(t, err)
	_, err = f.service.Start(ctx, StartRequest{Selector: f.selector("vision"), PlanFingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrRenditionRuntimeUnavailable)
	_, describes := f.counts()
	require.Zero(t, describes)

	probesBefore, _ := f.counts()
	_, err = f.service.Start(ctx, StartRequest{Selector: f.selector("vision"), PlanFingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrRenditionRuntimeUnavailable)
	probesAfter, _ := f.counts()
	require.Equal(t, probesBefore, probesAfter, "a failed probe repeats no sooner than a minute later")

	f.mu.Lock()
	f.probeFails = false
	f.mu.Unlock()
	f.service.mediaOriginMu.Lock()
	id := visionReadinessKey(f.service.profiles["vision"].record.Fingerprint)
	observation := f.service.mediaOriginEvidence[id]
	observation.ProbedAt = observation.ProbedAt.Add(-2 * time.Minute)
	f.service.mediaOriginEvidence[id] = observation
	f.service.mediaOriginMu.Unlock()
	require.Equal(t, 1, f.backfill(t, f.service), "the provider recovers without a restart")
	require.Len(t, f.suggestions(t), 1)
}

func TestPhotoDescriptionOverlappingReadinessChecksShareOneProbe(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	f.grant(t, f.service, "vision")
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.mu.Lock()
	// The first probe waits; any other probe fails.
	f.onProbe = func(call int) {
		if call == 1 {
			<-release
			return
		}
		f.mu.Lock()
		f.probeFails = true
		f.mu.Unlock()
	}
	f.mu.Unlock()
	profile := f.service.profiles["vision"]
	first := make(chan error, 1)
	go func() { first <- f.service.requireVisionReadiness(t.Context(), profile) }()
	require.Eventually(t, func() bool { probes, _ := f.counts(); return probes == 1 }, 5*time.Second, time.Millisecond)

	second := make(chan error, 1)
	go func() { second <- f.service.requireVisionReadiness(t.Context(), profile) }()
	require.Never(t, func() bool { probes, _ := f.counts(); return probes > 1 }, 100*time.Millisecond, time.Millisecond,
		"an overlapping request waits for the probe in flight")
	unblock()
	require.NoError(t, <-first)
	require.NoError(t, <-second, "the success stands")
	probes, _ := f.counts()
	require.Equal(t, 1, probes)
}

func TestPhotoDescriptionStartSavesConsentBeforeReportingProbeFailure(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	f.mu.Lock()
	f.probeFails = true
	f.mu.Unlock()
	plan, err := f.service.Plan(t.Context(), f.selector("vision"))
	require.NoError(t, err)
	_, err = f.service.Start(t.Context(), StartRequest{Selector: f.selector("vision"), PlanFingerprint: plan.Fingerprint, Consent: true})
	require.ErrorIs(t, err, ErrConsentSaved)
	require.ErrorIs(t, err, ErrRenditionRuntimeUnavailable)
	plan, err = f.service.Plan(t.Context(), f.selector("vision"))
	require.NoError(t, err)
	require.False(t, plan.ConsentRequired, "the grant stayed saved")

	hidden := newPhotoDescriptionFixture(t)
	require.NoError(t, hidden.catalog.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
	_, err = hidden.catalog.SetPhotoAssetHidden(t.Context(), hidden.asset.ID, hidden.asset.Revision, true)
	require.NoError(t, err)
	plan, err = hidden.service.Plan(t.Context(), hidden.selector("vision"))
	require.NoError(t, err)
	require.NotEmpty(t, plan.RenditionIneligibleReason)
	_, err = hidden.service.Start(t.Context(), StartRequest{Selector: hidden.selector("vision"), PlanFingerprint: plan.Fingerprint, Consent: true})
	require.ErrorIs(t, err, ErrSourceIneligible)
	require.NotErrorIs(t, err, ErrConsentSaved, "an ineligible photo is refused before consent is saved")
	plan, err = hidden.service.Plan(t.Context(), hidden.selector("vision"))
	require.NoError(t, err)
	require.True(t, plan.ConsentRequired)
}

func TestPhotoDescriptionStartDescribesOnce(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	plan, err := f.service.Plan(ctx, f.selector("vision"))
	require.NoError(t, err)
	var enqueued Job
	completed, err := f.service.StartWithProgress(ctx, StartRequest{Selector: f.selector("vision"), PlanFingerprint: plan.Fingerprint, Consent: true}, func(job Job) { enqueued = job })
	require.NoError(t, err)
	require.Equal(t, enqueued.RenditionJobID, completed.RenditionJobID)
	require.Empty(t, completed.AttachmentID)
	require.Len(t, f.suggestions(t), 1)
}

func TestPhotoDescriptionDefaultProfileChangeLeavesDismissals(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	f.grant(t, f.service, "vision")
	require.Equal(t, 1, f.backfill(t, f.service))
	f.dismiss(t, f.suggestions(t)[0])

	other := f.newService(t, "other", "replacement-model")
	f.grant(t, other, "other")
	require.Zero(t, f.backfill(t, other), "automatic work never re-describes a dismissed photo")
	_, describes := f.counts()
	require.Equal(t, 1, describes)

	plan, err := other.Plan(t.Context(), f.selector("other"))
	require.NoError(t, err)
	_, err = other.Start(t.Context(), StartRequest{Selector: f.selector("other"), PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	_, describes = f.counts()
	require.Equal(t, 2, describes, "an explicit start with another profile replaces the dismissal")
	require.Len(t, f.suggestions(t), 1)
}

func TestPhotoDescriptionHiddenAtPublishRetriesOnceVisible(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	require.NoError(t, f.catalog.SetupPhotoHidden(ctx, "synthetic-passcode"))
	token, _, err := f.catalog.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	unlocked := store.WithPhotoHiddenToken(ctx, token)
	f.grant(t, f.service, "vision")
	f.mu.Lock()
	f.onDescribe = func() {
		_, hideErr := f.catalog.SetPhotoAssetHidden(unlocked, f.asset.ID, f.asset.Revision, true)
		assert.NoError(t, hideErr)
	}
	f.mu.Unlock()
	f.backfill(t, f.service)
	prepared, err := f.catalog.PreparePhotoAuthored(unlocked, store.PhotoSetSelection{AssetIDs: []string{f.asset.ID}, Hidden: true})
	require.NoError(t, err)
	require.Empty(t, prepared.Suggestions, "an answer for a photo hidden mid-call is discarded")

	f.mu.Lock()
	f.onDescribe = nil
	f.mu.Unlock()
	hidden, err := f.catalog.PhotoAssetByID(unlocked, f.asset.ID)
	require.NoError(t, err)
	_, err = f.catalog.SetPhotoAssetHidden(unlocked, f.asset.ID, hidden.Revision, false)
	require.NoError(t, err)
	require.Equal(t, 1, f.backfill(t, f.service), "the photo is described again once it is visible")
	require.Len(t, f.suggestions(t), 1)
}

func TestPhotoDescriptionUploadsStrippedPreviewBytes(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	exif := append([]byte("Exif\x00\x00"), syntheticTIFF(42, []syntheticTIFFEntry{tiffShort(0x0112, 1)}, nil)...)
	for _, scenario := range []string{"exif", "after_scan", "trailing", "wide", "tall"} {
		t.Run(scenario, func(t *testing.T) {
			raw := mediatest.JPEG(8, 8, color.White)
			segment := syntheticJPEGSegment(t, []byte{0xff, 0xd8}, 0xe1, exif)[2:]
			switch scenario {
			case "exif":
				raw = syntheticJPEGSegment(t, raw, 0xe1, exif)
			case "after_scan":
				raw = append(raw[:len(raw)-2:len(raw)-2], append(segment, 0xff, 0xd9)...)
			case "trailing":
				raw = append(raw, segment...)
			case "wide":
				// Publishing refuses honest oversize dimensions, so the bytes outgrow the recorded 8x8.
				raw = mediatest.JPEG(513, 8, color.White)
			case "tall":
				raw = mediatest.JPEG(8, 513, color.White)
			}
			// A restored preview is whatever the backup held, not what the producer wrote.
			node, err := f.catalog.CreateFile(ctx, f.catalog.RootID(), scenario+".jpg", f.written.Hash, f.written.Size, "image/jpeg", processingBlobPhysical(t, f.written))
			require.NoError(t, err)
			written, err := f.blobs.WriteDetailedContext(ctx, bytes.NewReader(raw))
			require.NoError(t, err)
			canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
				ContractVersion: document.VisualPreviewContractV1, SourceSHA256: f.written.Hash,
				Recipe: f.recipe, State: document.VisualPreviewReady,
				Output: &document.VisualPreviewOutputV1{BlobSHA256: written.Hash, Size: written.Size, MediaType: "image/jpeg", Width: 8, Height: 8},
			})
			require.NoError(t, err)
			_, err = f.catalog.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, canonical, new(processingBlobPhysical(t, written)))
			require.NoError(t, err)
			version, err := f.catalog.ContentVersionByID(ctx, node.CurrentVersionID)
			require.NoError(t, err)
			outbound, capability, err := photoDescriptionInput(ctx, f.catalog, f.blobs, version, f.service.profiles["vision"])
			if scenario == "wide" || scenario == "tall" {
				require.ErrorIs(t, err, ErrSourceIneligible)
				return
			}
			require.NoError(t, err)
			sum := sha256.Sum256(outbound)
			require.Equal(t, hex.EncodeToString(sum[:]), capability.SourceSHA256)
			require.NotContains(t, string(outbound), "Exif")
			require.NotContains(t, string(outbound), "\xff\xe1")
			_, err = jpeg.Decode(bytes.NewReader(outbound))
			require.NoError(t, err)
		})
	}
}
