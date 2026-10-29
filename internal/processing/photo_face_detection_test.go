package processing

import (
	"context"
	"encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/internal/store"
)

const detectedFaceReply = `{"faces":[{"box":{"x":0.1,"y":0.2,"width":0.2,"height":0.3},"roll":0,"facing":0.9,"eyes_open":true,"smile":false}]}`

func TestPhotoFaceDetectionFlow(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	require.False(t, f.service.HasPhotoFaceDetectionProfiles())
	service := f.newVisionServiceWith(t, map[string]string{"faces": "detector-1"}, document.FaceDetectionProviderID)
	require.True(t, service.HasPhotoFaceDetectionProfiles())
	require.False(t, service.HasPhotoDescriptionProfiles())
	targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
	require.NoError(t, err)
	require.Empty(t, targets)
	plan, err := service.Plan(ctx, f.selector("faces"))
	require.NoError(t, err)
	require.Contains(t, plan.Estimate.Scope, "finds faces")
	require.Contains(t, plan.BackupConsequence, "Face boxes, measurements, and your decisions")
	require.Contains(t, plan.BackupConsequence, "another scan and provider call")
	require.NotContains(t, plan.BackupConsequence, "suggestions")
	_, err = service.Start(ctx, StartRequest{Selector: f.selector("faces"), PlanFingerprint: plan.Fingerprint})
	require.ErrorIs(t, err, ErrConsentRequired)
	probes, calls := f.counts()
	require.Zero(t, probes+calls)
	// Settle a caption first. It must neither block detection nor be changed by it.
	f.grant(t, f.service, "vision")
	require.Equal(t, 1, f.backfill(t, f.service))
	suggestions := f.suggestions(t)
	require.Len(t, suggestions, 1)
	f.mu.Lock()
	f.reply = detectedFaceReply
	f.mu.Unlock()
	f.grant(t, service, "faces")
	targets, err = service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() { failures <- service.admitPhotoDescription(ctx, targets[0]) })
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	f.runWorker(t, service)
	faces, assertions, _, err := f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	require.Len(t, faces, 1)
	require.Empty(t, assertions)
	face := faces[0]
	require.Equal(t, document.FaceBox{X: .1, Y: .2, Width: .2, Height: .3}, face.Box)
	require.NotEmpty(t, face.DetectorFingerprint)
	require.Empty(t, face.AssertionID)
	require.Equal(t, suggestions, f.suggestions(t))
	_, calls = f.counts()
	require.Equal(t, 2, calls)
	f.mu.Lock()
	body := append([]byte(nil), f.describeBody...)
	f.mu.Unlock()
	var outbound struct {
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &outbound))
	require.Contains(t, outbound.Messages[0].Content[0].Text, "Find every visible face")
	targets, err = service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
	require.NoError(t, err)
	require.Empty(t, targets)
	plan, err = service.Plan(ctx, f.selector("faces"))
	require.NoError(t, err)
	_, err = service.Start(ctx, StartRequest{Selector: f.selector("faces"), PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	f.runWorker(t, service)
	_, calls = f.counts()
	require.Equal(t, 2, calls, "a repeated explicit start uses the completed scan")
	// A changed detector runs once.
	service = f.newVisionServiceWith(t, map[string]string{"faces": "detector-2"}, document.FaceDetectionProviderID)
	f.grant(t, service, "faces")
	f.mu.Lock()
	f.reply = strings.Replace(detectedFaceReply, `"facing":0.9`, `"facing":0.4`, 1)
	f.mu.Unlock()
	targets, err = service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.NoError(t, service.admitPhotoDescription(ctx, targets[0]))
	f.runWorker(t, service)
	_, calls = f.counts()
	require.Equal(t, 3, calls)
}

func TestPhotoFaceDetectionEmptyAndMalformed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, reply string
		settled     bool
	}{
		{"empty", `{"faces":[]}`, true},
		{"trailing object", `{"faces":[]} {"faces":[`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t)
			ctx := t.Context()
			first := f.newVisionServiceWith(t, map[string]string{"faces": "detector-1"}, document.FaceDetectionProviderID)
			f.mu.Lock()
			observation := strings.TrimSuffix(strings.TrimPrefix(detectedFaceReply, `{"faces":[`), `]}`)
			f.reply = `{"faces":[` + observation + `,` + strings.Replace(observation, `"x":0.1`, `"x":0.7`, 1) + `]}`
			f.mu.Unlock()
			f.grant(t, first, "faces")
			initial, err := first.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
			require.NoError(t, err)
			require.Len(t, initial, 1)
			require.NoError(t, first.admitPhotoDescription(ctx, initial[0]))
			f.runWorker(t, first)
			original, _, _, err := f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
			require.NoError(t, err)
			require.Len(t, original, 2)
			service := f.newVisionServiceWith(t, map[string]string{"faces": "detector-2"}, document.FaceDetectionProviderID)
			for _, runtime := range service.renditions.runtimes {
				provider, ok := runtime.(*providerRenditionRuntime)
				require.True(t, ok)
				provider.clock = time.Now
			}
			f.mu.Lock()
			f.reply = tc.reply
			f.mu.Unlock()
			f.grant(t, service, "faces")
			targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			binding, err := service.photoDescriptionBinding(ctx, targets[0].version)
			require.NoError(t, err)
			profile := service.profiles["faces"]
			job, _, err := service.enqueueRendition(ctx, f.node, targets[0].version, profile, service.renditionConsentRequest(profile), binding, false)
			require.NoError(t, err)
			f.runWorker(t, service)
			if !tc.settled {
				for range 3 {
					f.skew.Add(int64(2 * time.Second))
					f.runWorker(t, service)
				}
			}
			_, calls := f.counts()
			if tc.settled {
				require.Equal(t, 2, calls)
			} else {
				require.Equal(t, 4, calls)
			}
			faces, _, _, err := f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
			require.NoError(t, err)
			savedJob, err := f.catalog.RenditionJobByID(ctx, job.ID)
			require.NoError(t, err)
			if tc.settled {
				require.Equal(t, store.RenditionJobCompleted, savedJob.State)
			} else {
				require.Equal(t, original, faces, "a failed reply preserves every earlier face")
				require.Equal(t, store.RenditionJobFailed, savedJob.State)
				require.Equal(t, 3, savedJob.ProviderAttempts)
			}
			// Completed and terminally failed scans stay out of the automatic pass.
			targets, err = service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
			require.NoError(t, err)
			require.Empty(t, targets)
		})
	}
}

func TestPhotoFaceDetectionPublicationAuthority(t *testing.T) {
	t.Parallel()
	f := newPhotoDescriptionFixture(t)
	ctx := t.Context()
	service := f.newVisionServiceWith(t, map[string]string{"faces": "detector"}, document.FaceDetectionProviderID)
	f.grant(t, service, "faces")
	f.mu.Lock()
	f.reply = detectedFaceReply
	f.mu.Unlock()
	targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.NoError(t, service.admitPhotoDescription(ctx, targets[0]))
	catalog := revokingFaceDetectionCatalog{Store: f.catalog}
	worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: catalog, Blobs: f.blobs, Runtime: service.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: service.clock})
	require.NoError(t, err)
	processed, err := worker.RunOne(ctx)
	require.NoError(t, err)
	require.True(t, processed)
	faces, _, _, err := f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
	require.NoError(t, err)
	require.Empty(t, faces)
	f.grant(t, service, "faces")
	targets, err = service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
	require.NoError(t, err)
	require.Len(t, targets, 1, "discarded publication leaves no completion marker")
}

// Revocation waits for outbound requests. Apply it after the HTTP result has
// returned, immediately before the real store publication checks its authority.
type revokingFaceDetectionCatalog struct{ *store.Store }

func (catalog revokingFaceDetectionCatalog) PublishPhotoDescription(ctx context.Context, claim store.RenditionJobClaim, result *document.RenditionResult, at time.Time) (store.RenditionJobPublication, error) {
	if _, err := catalog.RevokeConsent(ctx, store.ProcessingConsentRevocationRequest{Principal: "synthetic-operator", Scope: "document-processing"}); err != nil {
		return store.RenditionJobPublication{}, err
	}
	return catalog.Store.PublishPhotoDescription(ctx, claim, result, at)
}

func TestPhotoFaceDetectionPublicationFailureKeepsWorkerRunning(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"capacity", "audit"} {
		t.Run(mode, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t)
			ctx := t.Context()
			service := f.newVisionServiceWith(t, map[string]string{"faces": "detector"}, document.FaceDetectionProviderID)
			f.grant(t, service, "faces")
			if mode == "capacity" {
				for range document.MaxPhotoRecordsPerVersion {
					_, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .05, Height: .05}, "")
					require.NoError(t, err)
				}
			}
			f.mu.Lock()
			f.reply = detectedFaceReply
			f.mu.Unlock()
			targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.NoError(t, service.admitPhotoDescription(ctx, targets[0]))
			catalog := &auditingFaceDetectionCatalog{Store: f.catalog, enable: mode == "audit"}
			for fingerprint, runtime := range f.service.renditions.runtimes {
				require.NoError(t, service.renditions.Register(fingerprint, runtime))
			}
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: catalog, Blobs: f.blobs, Runtime: service.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: service.clock})
			require.NoError(t, err)
			processed, err := worker.RunOne(ctx)
			require.NoError(t, err, "one photo must not stop shared processing")
			require.True(t, processed)
			if mode != "capacity" {
				faces, _, _, err := f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
				require.NoError(t, err)
				require.Empty(t, faces, "rejected canonical publication creates no face")
			}
			f.mu.Lock()
			f.reply = `{"caption":"A harbor.","tags":[],"visible_text":""}`
			f.mu.Unlock()
			f.grant(t, f.service, "vision")
			descriptions, err := f.service.photoVisionTargets(ctx, nil, "", 50, 4, vision.ProviderID)
			require.NoError(t, err)
			require.Len(t, descriptions, 1)
			require.NoError(t, f.service.admitPhotoDescription(ctx, descriptions[0]))
			processed, err = worker.RunOne(ctx)
			require.NoError(t, err)
			require.True(t, processed)
			require.Len(t, f.suggestions(t), 1, "the same worker processes unrelated descriptions after the rejected detection")
		})
	}
}

type auditingFaceDetectionCatalog struct {
	*store.Store

	enable bool
}

func (c *auditingFaceDetectionCatalog) PublishPhotoDescription(ctx context.Context, claim store.RenditionJobClaim, result *document.RenditionResult, at time.Time) (store.RenditionJobPublication, error) {
	if c.enable {
		plan, err := c.PreviewInitialAudit(ctx, c.RootID(), "cli", nil)
		if err != nil {
			return store.RenditionJobPublication{}, err
		}
		if _, err = c.EnableInitialAudit(ctx, plan); err != nil {
			return store.RenditionJobPublication{}, err
		}
		c.enable = false
	}
	return c.Store.PublishPhotoDescription(ctx, claim, result, at)
}

func TestPhotoFaceDetectionAuditRefusesEgress(t *testing.T) {
	t.Parallel()
	for _, queued := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "queued"}[queued], func(t *testing.T) {
			f := newPhotoDescriptionFixture(t)
			ctx := t.Context()
			service := f.newVisionServiceWith(t, map[string]string{"faces": "detector"}, document.FaceDetectionProviderID)
			f.grant(t, service, "faces")
			plan, err := service.Plan(ctx, f.selector("faces"))
			require.NoError(t, err)
			if queued {
				targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
				require.NoError(t, err)
				require.Len(t, targets, 1)
				require.NoError(t, service.admitPhotoDescription(ctx, targets[0]))
			}
			probesBefore, callsBefore := f.counts()
			audit, err := f.catalog.PreviewInitialAudit(ctx, f.catalog.RootID(), "cli", nil)
			require.NoError(t, err)
			_, err = f.catalog.EnableInitialAudit(ctx, audit)
			require.NoError(t, err)
			if queued {
				f.runWorker(t, service)
				probes, calls := f.counts()
				require.Equal(t, probesBefore, probes)
				require.Equal(t, callsBefore, calls)
			} else {
				_, err = service.Start(ctx, StartRequest{Selector: f.selector("faces"), PlanFingerprint: plan.Fingerprint})
				require.ErrorIs(t, err, store.ErrAuditMutationUnsupported)
				targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
				require.NoError(t, err)
				require.Empty(t, targets)
				probes, calls := f.counts()
				require.Zero(t, probes+calls)
			}
		})
	}
}
