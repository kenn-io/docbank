package processing

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/workerprotocol"
	"go.kenn.io/docbank/internal/store"
)

type workerCheckpointRuntime struct {
	base     *workerProvider
	provider document.RenditionProvider
	lifetime time.Duration
}

func (r workerCheckpointRuntime) Prepare(ctx context.Context, work store.RenditionJobWork, now time.Time) (RenditionExecution, error) {
	execution, err := (workerRuntime{provider: r.base}).Prepare(ctx, work, now)
	execution.Provider = r.provider
	if r.lifetime > 0 {
		execution.Authorization.ExpiresAt = now.Add(r.lifetime).Format("2006-01-02T15:04:05.000000000Z")
	}
	return execution, err
}
func (r workerCheckpointRuntime) ResumeProvider(context.Context, store.RenditionJobWork, document.RenditionExecutionSnapshotV1) (document.RenditionProvider, error) {
	return r.provider, nil
}

type workerProbeSecrets struct{}

func (workerProbeSecrets) ResolveSecret(context.Context, string) (string, error) {
	return "synthetic-worker-key", nil
}

type workerProbeTransport func(*http.Request) (*http.Response, error)

func (f workerProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGenuineWorkerExpiryBeforeCheckpointReconciles(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fixture := newPublicationFixture(t)
		base := newWorkerProvider(t)
		descriptor, err := docling.WorkerDocumentDescriptor()
		require.NoError(t, err)
		base.descriptor = descriptor
		posts := 0
		interrupted, accepted := false, false
		var key string
		issued := strings.Repeat("e", 32)
		task := `{"task_id":"` + issued + `","task_status":"success"}`
		transport := &http.Client{Transport: workerProbeTransport(func(r *http.Request) (*http.Response, error) {
			current, status, body := "lookup", 404, `{}`
			switch {
			case strings.HasPrefix(r.URL.Path, workerprotocol.LookupPath):
				currentKey := strings.TrimPrefix(r.URL.Path, workerprotocol.LookupPath)
				if key == "" {
					key = currentKey
				} else {
					require.Equal(t, key, currentKey, "fresh authority must reconcile the same immutable key")
				}
				if accepted {
					status, body = 200, task
				}
			case r.URL.Path == workerprotocol.ReadyPath:
				current, status, body = "ready", 200, `{"protocol":"`+workerprotocol.Contract+`"}`
			case r.Method == http.MethodPost:
				current = "submit"
				posts++
				_, err := io.Copy(io.Discard, r.Body)
				require.NoError(t, err)
				require.Equal(t, key, r.Header.Get("Idempotency-Key"))
				accepted = true
				status, body = 202, task
			case strings.HasPrefix(r.URL.Path, workerprotocol.ResultPath):
				current, status, body = "result", 200, `{"document":{"filename":"document.pdf","json_content":{"schema_name":"DoclingDocument","version":"1.7.0","pages":{"1":{}},"texts":[{"text":"Synthetic reconciled content","prov":[{"page_no":1}]}]}},"status":"success","errors":[]}`
			default:
				t.Fatalf("unexpected worker probe route %s", r.URL.Path)
			}
			if current == "submit" && !interrupted {
				interrupted = true
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return &http.Response{Request: r, StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}, workerprotocol.ProtocolHeader: []string{workerprotocol.Contract}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		provider, err := docling.NewWorker(docling.Profile{Origin: "http://127.0.0.1:5002", Descriptor: descriptor, SecretBinding: "worker", RequestTimeout: time.Minute, TotalTimeout: time.Minute, PollInterval: time.Millisecond, MaxPollAttempts: 3, MaxDocumentBytes: 1 << 20, MaxResponseBytes: 1 << 20}, workerProbeSecrets{}, transport)
		require.NoError(t, err)
		profile := workerProcessingProfile(t, descriptor, func(p *document.ProcessingProfileV1) {
			p.Rendition.AdapterContract = workerprotocol.DocumentAdapter
			p.Rendition.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
			p.RetentionDisclosure.TrustBoundary = string(document.RenditionTrustOperatorNetwork)
		})
		fixture.profile = profile
		request := workerJobRequest(fixture.versionID, profile, descriptor)
		grantWorkerConsent(t, fixture.catalog, request)
		job, _, err := fixture.catalog.EnqueueRenditionJob(t.Context(), request)
		require.NoError(t, err)
		worker, err := NewRenditionWorker(RenditionWorkerConfig{Catalog: fixture.catalog, Blobs: fixture.blobs, Runtime: workerCheckpointRuntime{base: base, provider: provider, lifetime: time.Second}, Gate: newTestOperationGate(), Owner: "worker-precheckpoint-expiry", LeaseDuration: time.Minute, IdleDelay: time.Millisecond})
		require.NoError(t, err)
		processed, err := worker.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		current, err := fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobRetryWait, current.State)
		require.Equal(t, 1, posts)
		time.Sleep(31 * time.Second)
		processed, err = worker.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		current, err = fixture.catalog.RenditionJobByID(t.Context(), job.ID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobCompleted, current.State)
		require.Equal(t, 1, posts, "expiry and renewed authority must not duplicate an accepted job")
	})
}
