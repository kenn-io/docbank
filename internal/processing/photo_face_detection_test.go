package processing

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

const detectedFaceReply = `{"faces":[{"box":{"x":0.1,"y":0.2,"width":0.2,"height":0.3},"roll":0,"facing":0.9,"eyes_open":true,"smile":false}]}`

func TestPhotoFaceDetectionFlow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, reply string }{
		{"faces", detectedFaceReply},
		{"empty", `{"faces":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t)
			ctx := t.Context()
			service := f.newVisionServiceWith(t, map[string]string{"faces": "detector-1"}, document.FaceDetectionProviderID)
			targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
			require.NoError(t, err)
			require.Empty(t, targets)
			if tc.name == "faces" {
				require.False(t, f.service.HasPhotoFaceDetectionProfiles())
				require.True(t, service.HasPhotoFaceDetectionProfiles())
				require.False(t, service.HasPhotoDescriptionProfiles())
				plan, err := service.Plan(ctx, f.selector("faces"))
				require.NoError(t, err)
				require.Contains(t, plan.Estimate.Scope, "finds faces")
				require.Contains(t, plan.BackupConsequence, "Face boxes, measurements, and your decisions")
				require.Contains(t, plan.BackupConsequence, "another scan and provider call")
				require.NotContains(t, plan.BackupConsequence, "suggestions")
			}
			// Settle a caption first. It must neither block detection nor be changed by it.
			f.grant(t, f.service, "vision")
			require.Equal(t, 1, f.backfill(t, f.service))
			suggestions := f.suggestions(t)
			require.Len(t, suggestions, 1)
			f.mu.Lock()
			f.reply = tc.reply
			f.mu.Unlock()
			job := f.enqueueFaceDetection(t, service)
			f.runWorker(t, service)
			saved, err := f.catalog.RenditionJobByID(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, store.RenditionJobCompleted, saved.State)
			faces, assertions, _, err := f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
			require.NoError(t, err)
			require.Empty(t, assertions)
			if tc.name == "faces" {
				require.Len(t, faces, 1)
				face := faces[0]
				require.Equal(t, document.FaceBox{X: .1, Y: .2, Width: .2, Height: .3}, face.Box)
				require.NotEmpty(t, face.DetectorFingerprint)
				require.Empty(t, face.AssertionID)
			}
			require.Equal(t, suggestions, f.suggestions(t))
			_, calls := f.counts()
			require.Equal(t, 2, calls)
			targets, err = service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
			require.NoError(t, err)
			require.Empty(t, targets)
			// A changed detector runs once.
			service = f.newVisionServiceWith(t, map[string]string{"faces": "detector-2"}, document.FaceDetectionProviderID)
			f.grant(t, service, "faces")
			targets, err = service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.NoError(t, service.admitPhotoDescription(ctx, targets[0]))
			f.runWorker(t, service)
			_, calls = f.counts()
			require.Equal(t, 3, calls)
		})
	}
}

func TestPhotoFaceDetectionPublicationFailureKeepsWorkerRunning(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"capacity", "audit"} {
		t.Run(mode, func(t *testing.T) {
			f := newPhotoDescriptionFixture(t)
			ctx := t.Context()
			service := f.newVisionServiceWith(t, map[string]string{"faces": "detector"}, document.FaceDetectionProviderID)
			if mode == "capacity" {
				for range document.MaxPhotoRecordsPerVersion {
					_, _, err := f.catalog.CreatePhotoFace(ctx, f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{Width: .05, Height: .05}, "")
					require.NoError(t, err)
				}
			}
			f.mu.Lock()
			f.reply = detectedFaceReply
			f.mu.Unlock()
			job := f.enqueueFaceDetection(t, service)
			catalog := &auditingFaceDetectionCatalog{Store: f.catalog, enable: mode == "audit"}
			worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: catalog, Blobs: f.blobs, Runtime: service.renditions, Gate: newTestOperationGate(), Owner: "synthetic-worker", LeaseDuration: time.Minute, IdleDelay: time.Millisecond, Clock: service.clock})
			require.NoError(t, err)
			processed, err := worker.RunOne(ctx)
			require.NoError(t, err, "one photo must not stop shared processing")
			require.True(t, processed)
			saved, err := f.catalog.RenditionJobByID(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, store.RenditionJobFailed, saved.State)
			if mode != "capacity" {
				faces, _, _, err := f.catalog.PhotoFacePersonNames(ctx, f.node.CurrentVersionID)
				require.NoError(t, err)
				require.Empty(t, faces, "rejected canonical publication creates no face")
			}
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
			if queued {
				f.enqueueFaceDetection(t, service)
			} else {
				f.grant(t, service, "faces")
			}
			plan, err := service.Plan(ctx, f.selector("faces"))
			require.NoError(t, err)
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

func (f *photoDescriptionFixture) enqueueFaceDetection(t *testing.T, service *Service) store.RenditionJob {
	t.Helper()
	ctx := t.Context()
	f.grant(t, service, "faces")
	targets, err := service.photoVisionTargets(ctx, nil, "", 50, 4, document.FaceDetectionProviderID)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	binding, err := service.photoDescriptionBinding(ctx, targets[0].version)
	require.NoError(t, err)
	profile := service.profiles["faces"]
	job, _, err := service.enqueueRendition(ctx, f.node, targets[0].version, profile, service.renditionConsentRequest(profile), binding, false)
	require.NoError(t, err)
	return job
}
