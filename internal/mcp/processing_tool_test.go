package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

const testProcessingNodeID int64 = 7

var (
	testProcessingPlanFingerprint = strings.Repeat("d", 64)
	testProcessingJobID           = strings.Repeat("e", 64)
)

func TestStartProcessingIsUnavailableWhenCatalogIsReadOnly(t *testing.T) {
	server := newServerWithOptions(testImplementation(), ServerOptions{})
	listed := decodeResult(t, exchangeRaw(t, server, requestFor("tools/list", map[string]any{})))
	assert.NotContains(t, listedToolNames(t, listed), "start_processing")

	response := exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "start_processing", "arguments": processingToolArguments(testProcessingPlanFingerprint),
	}))
	wireErr := decodeWireError(t, response)
	assert.NotZero(t, wireErr.Code)
	assert.NotContains(t, string(response), "/home/private")
}

func TestStartProcessingRejectsUnseenPlanFingerprintWithoutDaemonMutation(t *testing.T) {
	harness := newProcessingToolHarness(t, testProcessingPlanFingerprint, nil)
	server := harness.server(t)
	primeProcessingPlan(t, server)

	result := callProcessingTool(t, server, strings.Repeat("f", 64))
	assertProcessingToolError(t, result, "plan_changed")
	assert.Zero(t, harness.startCalls.Load())
}

func TestStartProcessingMapsDisclosureConsentAndSourceFailures(t *testing.T) {
	for _, testCase := range []struct {
		name, daemonCode, wantCode string
		status                     int
	}{
		{name: "changed disclosure", daemonCode: "processing_plan_changed", wantCode: "plan_changed", status: http.StatusConflict},
		{name: "missing consent", daemonCode: "processing_consent_required", wantCode: "consent_required", status: http.StatusPreconditionRequired},
		{name: "stale source", daemonCode: "stale_version", wantCode: "stale_version", status: http.StatusConflict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newProcessingToolHarness(t, testProcessingPlanFingerprint,
				func(t *testing.T, response http.ResponseWriter, request *http.Request) {
					t.Helper()
					harnessRequest := decodeStartProcessingRequest(t, request)
					assertExactMCPStartRequest(t, harnessRequest)
					response.Header().Set("Content-Type", "application/problem+json")
					response.WriteHeader(testCase.status)
					require.NoError(t, json.MarshalWrite(response, api.Error{
						Title: "Synthetic failure", Status: testCase.status, Code: testCase.daemonCode,
						Detail: "private document /home/private secret-provider-token",
					}))
				})
			server := harness.server(t)
			primeProcessingPlan(t, server)

			result := callProcessingTool(t, server, testProcessingPlanFingerprint)
			assertProcessingToolError(t, result, testCase.wantCode)
			assert.Equal(t, int32(1), harness.startCalls.Load())
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "/home/private")
			assert.NotContains(t, string(encoded), "secret-provider-token")
			assert.NotContains(t, string(encoded), "private document")
		})
	}
}

func TestStartProcessingEnqueuesOnceWithoutWaitingForProgress(t *testing.T) {
	requestCanceled := make(chan struct{})
	harness := newProcessingToolHarness(t, testProcessingPlanFingerprint,
		func(t *testing.T, response http.ResponseWriter, request *http.Request) {
			t.Helper()
			input := decodeStartProcessingRequest(t, request)
			assertExactMCPStartRequest(t, input)
			response.Header().Set("Content-Type", "application/x-ndjson")
			require.NoError(t, json.MarshalWrite(response, api.ProcessingJobEvent{
				Sequence: 1, Type: "job", Job: syntheticProcessingJob(),
			}))
			flusher, ok := response.(http.Flusher)
			require.True(t, ok)
			flusher.Flush()
			<-request.Context().Done()
			close(requestCanceled)
		})
	server := harness.server(t)
	primeProcessingPlan(t, server)

	done := make(chan *sdkCallResult, 1)
	go func() {
		result, err := invokeProcessingTool(t.Context(), server.daemon, server.plans,
			processingToolArguments(testProcessingPlanFingerprint))
		done <- &sdkCallResult{result: result, err: err}
	}()
	select {
	case call := <-done:
		require.NoError(t, call.err)
		require.NotNil(t, call.result)
		output := structuredMap(t, call.result.StructuredContent)
		assert.Equal(t, testProcessingJobID, output["job_id"])
		assert.Equal(t, testVersionID, output["content_version_id"])
		assert.Equal(t, testProfileID, output["profile_fingerprint"])
		assert.Equal(t, "queued", output["state"])
		assert.EqualValues(t, 0, output["ttlMs"])
		assert.Equal(t, "private", output["cacheScope"])
		assertSchemaAccepts(t, catalogMap(toolCatalog(true))["start_processing"].OutputSchema, output)
	case <-t.Context().Done():
		t.Fatal("start_processing waited for terminal progress instead of returning the durable enqueue")
	}
	select {
	case <-requestCanceled:
	case <-t.Context().Done():
		t.Fatal("processing response stream was not closed after the enqueue identity")
	}
	assert.Equal(t, int32(1), harness.startCalls.Load())
	assert.Zero(t, harness.statusCalls.Load())
}

func TestStartProcessingRejectsDurableJobFromDifferentReviewedProfile(t *testing.T) {
	otherProfile := strings.Repeat("f", 64)
	harness := newProcessingToolHarness(t, testProcessingPlanFingerprint,
		func(t *testing.T, response http.ResponseWriter, request *http.Request) {
			t.Helper()
			assertExactMCPStartRequest(t, decodeStartProcessingRequest(t, request))
			job := *syntheticProcessingJob()
			job.ProfileFingerprint = otherProfile
			response.Header().Set("Content-Type", "application/x-ndjson")
			require.NoError(t, json.MarshalWrite(response, api.ProcessingJobEvent{
				Sequence: 1, Type: "job", Job: &job,
			}))
		})
	server := harness.server(t)
	primeProcessingPlan(t, server)

	result := callProcessingTool(t, server, testProcessingPlanFingerprint)
	assertProcessingToolError(t, result, "processing_outcome_unknown")
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), otherProfile)
	assert.Equal(t, int32(1), harness.startCalls.Load())
}

func TestStartProcessingWireResultMatchesPublishedSchema(t *testing.T) {
	harness := newProcessingToolHarness(t, testProcessingPlanFingerprint,
		func(t *testing.T, response http.ResponseWriter, request *http.Request) {
			t.Helper()
			assertExactMCPStartRequest(t, decodeStartProcessingRequest(t, request))
			response.Header().Set("Content-Type", "application/x-ndjson")
			require.NoError(t, json.MarshalWrite(response, api.ProcessingJobEvent{
				Sequence: 1, Type: "job", Job: syntheticProcessingJob(),
			}))
		})
	server := harness.server(t)
	primeProcessingPlan(t, server)

	result := callProcessingTool(t, server, testProcessingPlanFingerprint)
	output := objectField(t, result, "structuredContent")
	assertSchemaAccepts(t, catalogMap(toolCatalog(true))["start_processing"].OutputSchema, output)
	assert.Equal(t, "queued", output["state"])
	assert.NotContains(t, result, "status")
	assert.NotContains(t, result, "progress")
	assert.Equal(t, int32(1), harness.startCalls.Load())
}

func TestStartProcessingPropagatesCancellationBeforeEnqueue(t *testing.T) {
	harness := newProcessingToolHarness(t, testProcessingPlanFingerprint, nil)
	lease := harness.lease(t)
	plans := newProcessingPlanRegistry()
	plans.remember(syntheticProcessingPlan(testProcessingPlanFingerprint))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := invokeProcessingTool(ctx, lease, plans,
		processingToolArguments(testProcessingPlanFingerprint))
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, result)
	assert.Zero(t, harness.startCalls.Load())
	assert.Zero(t, harness.ensures.Load())
}

func TestStartProcessingCancellationAfterDaemonAcceptsRequestIsUnknown(t *testing.T) {
	accepted := make(chan struct{})
	var startCalls atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		input := decodeStartProcessingRequest(t, request)
		assertExactMCPStartRequest(t, input)
		startCalls.Add(1)
		close(accepted)
		<-request.Context().Done()
	}))
	t.Cleanup(daemon.Close)
	var ensures, closes atomic.Int32
	lease := newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		ensures.Add(1)
		return client.New(daemon.URL, "synthetic-key"), nil
	}, func(*client.Client) error {
		closes.Add(1)
		return nil
	})
	plans := newProcessingPlanRegistry()
	plans.remember(syntheticProcessingPlan(testProcessingPlanFingerprint))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := invokeProcessingTool(ctx, lease, plans,
			processingToolArguments(testProcessingPlanFingerprint))
		done <- err
	}()
	<-accepted
	cancel()

	err := <-done
	require.ErrorIs(t, err, errProcessingOutcomeUnknown)
	require.NotErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), startCalls.Load())
	assert.Equal(t, int32(1), ensures.Load())
	assert.Equal(t, int32(1), closes.Load())
}

func TestStartProcessingNeverReplaysAmbiguousResponse(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		handler func(*testing.T, http.ResponseWriter)
	}{
		{name: "transport failure before response", handler: func(t *testing.T, response http.ResponseWriter) {
			t.Helper()
			hijacker, ok := response.(http.Hijacker)
			require.True(t, ok)
			connection, _, err := hijacker.Hijack()
			require.NoError(t, err)
			require.NoError(t, connection.Close())
		}},
		{name: "truncated response after headers", handler: func(t *testing.T, response http.ResponseWriter) {
			t.Helper()
			response.Header().Set("Content-Type", "application/x-ndjson")
			_, err := response.Write([]byte(`{"sequence":1,"type":"job","job":`))
			require.NoError(t, err)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newProcessingToolHarness(t, testProcessingPlanFingerprint,
				func(t *testing.T, response http.ResponseWriter, _ *http.Request) {
					t.Helper()
					testCase.handler(t, response)
				})
			server := harness.server(t)
			primeProcessingPlan(t, server)

			result := callProcessingTool(t, server, testProcessingPlanFingerprint)
			assertProcessingToolError(t, result, "processing_outcome_unknown")
			assert.Equal(t, int32(1), harness.startCalls.Load())
			assert.Equal(t, int32(1), harness.ensures.Load())
			assert.Equal(t, int32(1), harness.closes.Load())
		})
	}
}

func TestProcessingPlanRegistryRetainsReviewedProfileAndEvictsOldest(t *testing.T) {
	registry := newProcessingPlanRegistry()
	first := syntheticProcessingPlan(testProcessingPlanFingerprint)
	registry.remember(first)

	var newest api.ProcessingPlan
	for index := range maxRememberedProcessingPlans {
		newest = syntheticProcessingPlan(fmt.Sprintf("%064x", index))
		newest.Selector.ContentVersionID = fmt.Sprintf("00000000-0000-4000-8000-%012x", index)
		newest.ProfileFingerprint = fmt.Sprintf("%064x", index+1)
		registry.remember(newest)
	}

	_, err := registry.reviewed(first.Selector.ContentVersionID, first.Fingerprint)
	require.ErrorIs(t, err, client.ErrProcessingPlanChanged)
	reviewed, err := registry.reviewed(newest.Selector.ContentVersionID, newest.Fingerprint)
	require.NoError(t, err)
	assert.Equal(t, newest.Selector, reviewed.Selector)
	assert.Equal(t, newest.ProfileFingerprint, reviewed.ProfileFingerprint)
}

func TestProcessingPlanRegistryPreservesBindingsConcurrently(t *testing.T) {
	const workers = 64
	registry := newProcessingPlanRegistry()
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			plan := syntheticProcessingPlan(fmt.Sprintf("%064x", index+1))
			plan.Selector.ContentVersionID = fmt.Sprintf("00000000-0000-4000-8000-%012x", index+1)
			plan.ProfileFingerprint = fmt.Sprintf("%064x", index+workers+1)
			registry.remember(plan)
			reviewed, err := registry.reviewed(plan.Selector.ContentVersionID, plan.Fingerprint)
			if err != nil {
				errs <- err
				return
			}
			if reviewed.Selector != plan.Selector || reviewed.ProfileFingerprint != plan.ProfileFingerprint {
				errs <- fmt.Errorf("reviewed processing binding changed for worker %d", index)
			}
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

type sdkCallResult struct {
	result *sdkmcp.CallToolResult
	err    error
}

type processingToolHarness struct {
	t               *testing.T
	planFingerprint string
	startHandler    func(*testing.T, http.ResponseWriter, *http.Request)
	serverInstance  *httptest.Server
	ensures, closes atomic.Int32
	startCalls      atomic.Int32
	statusCalls     atomic.Int32
}

func newProcessingToolHarness(t *testing.T, planFingerprint string,
	startHandler func(*testing.T, http.ResponseWriter, *http.Request),
) *processingToolHarness {
	t.Helper()
	harness := &processingToolHarness{t: t, planFingerprint: planFingerprint, startHandler: startHandler}
	harness.serverInstance = httptest.NewServer(http.HandlerFunc(harness.serveHTTP))
	t.Cleanup(harness.serverInstance.Close)
	return harness
}

func (harness *processingToolHarness) serveHTTP(response http.ResponseWriter, request *http.Request) {
	assert.Equal(harness.t, "synthetic-key", request.Header.Get("X-Api-Key"))
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/processing/plans":
		writeDaemonJSON(harness.t, response, syntheticProcessingPlan(harness.planFingerprint))
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/processing/jobs":
		harness.startCalls.Add(1)
		if harness.startHandler == nil {
			http.Error(response, "unexpected processing start", http.StatusInternalServerError)
			return
		}
		harness.startHandler(harness.t, response, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/v1/processing/jobs/"):
		harness.statusCalls.Add(1)
		http.Error(response, "MCP must not poll", http.StatusInternalServerError)
	default:
		http.NotFound(response, request)
	}
}

func (harness *processingToolHarness) lease(t *testing.T) *daemonLease {
	t.Helper()
	return newDaemonLeaseWith(func(context.Context) (*client.Client, error) {
		harness.ensures.Add(1)
		return client.New(harness.serverInstance.URL, "synthetic-key"), nil
	}, func(*client.Client) error {
		harness.closes.Add(1)
		return nil
	})
}

func (harness *processingToolHarness) server(t *testing.T) *Server {
	t.Helper()
	return newServerWithOptionsAndDaemon(testImplementation(),
		ServerOptions{AllowProcessing: true}, harness.lease(t))
}

func syntheticProcessingPlan(fingerprint string) api.ProcessingPlan {
	return api.ProcessingPlan{Fingerprint: fingerprint, VaultUID: testVaultID,
		Selector: api.ProcessingSelector{NodeID: testProcessingNodeID,
			ContentVersionID: testVersionID, Profile: "local"},
		ProfileFingerprint: testProfileID, Flow: []api.ProcessingFlowHop{},
		DisclosedClasses: []string{}, RetainedClasses: []string{},
		ConsentRequired: false, ConsentState: "active", BackupConsequence: "synthetic derivatives are backed up"}
}

func syntheticProcessingJob() *api.ProcessingJob {
	return &api.ProcessingJob{ID: testProcessingJobID, EmbeddingJobIDs: []string{},
		ProfileFingerprint: testProfileID, ContentVersionID: testVersionID}
}

func processingToolArguments(fingerprint string) map[string]any {
	return map[string]any{"content_version_id": testVersionID, "plan_fingerprint": fingerprint}
}

func primeProcessingPlan(t *testing.T, server *Server) {
	t.Helper()
	result := decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "get_processing_plan", "arguments": map[string]any{
			"node_id": testProcessingNodeID, "content_version_id": testVersionID, "profile": "local",
		},
	})))
	assert.NotContains(t, result, "isError")
}

func callProcessingTool(t *testing.T, server *Server, fingerprint string) map[string]any {
	t.Helper()
	return decodeResult(t, exchangeRaw(t, server, requestFor("tools/call", map[string]any{
		"name": "start_processing", "arguments": processingToolArguments(fingerprint),
	})))
}

func assertProcessingToolError(t *testing.T, result map[string]any, code string) {
	t.Helper()
	assert.Equal(t, true, result["isError"])
	structured := objectField(t, result, "structuredContent")
	assert.Equal(t, code, structured["code"])
	message, ok := structured["message"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, message)
}

func decodeStartProcessingRequest(t *testing.T, request *http.Request) api.StartProcessingRequest {
	t.Helper()
	var input api.StartProcessingRequest
	require.NoError(t, json.UnmarshalRead(request.Body, &input, json.RejectUnknownMembers(true)))
	return input
}

func assertExactMCPStartRequest(t *testing.T, input api.StartProcessingRequest) {
	t.Helper()
	assert.Equal(t, api.ProcessingSelector{NodeID: testProcessingNodeID,
		ContentVersionID: testVersionID, Profile: "local"}, input.Selector)
	assert.Equal(t, testProcessingPlanFingerprint, input.PlanFingerprint)
	assert.False(t, input.Consent)
}
