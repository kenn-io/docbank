package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

const processingTestVersionID = "123e4567-e89b-42d3-a456-426614174000"

func TestProcessingCLIProfilesPlanBuildAndStatus(t *testing.T) {
	jobID := strings.Repeat("a", 64)
	profileFingerprint := strings.Repeat("b", 64)
	planFingerprint := strings.Repeat("c", 64)
	embeddingJobID := strings.Repeat("d", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "test-key", request.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/processing/profiles":
			assert.NoError(t, json.MarshalWrite(w, []api.ProcessingProfileSummary{{
				Name: "private", Fingerprint: profileFingerprint, Rendition: true,
				EmbeddingBindings: []string{"semantic"},
			}}))
		case "GET /api/v1/nodes/42":
			assert.NoError(t, json.MarshalWrite(w, api.Node{
				ID: 42, Kind: "file", Path: "/docs/report.pdf", CurrentVersionID: processingTestVersionID,
			}))
		case "POST /api/v1/processing/plans":
			assert.NoError(t, json.MarshalWrite(w, api.ProcessingPlan{
				Fingerprint: planFingerprint, VaultUID: "123e4567-e89b-42d3-a456-426614174001",
				Selector: api.ProcessingSelector{NodeID: 42, ContentVersionID: processingTestVersionID,
					Profile: "private"},
				ProfileFingerprint: profileFingerprint,
				Flow: []api.ProcessingFlowHop{{Capability: "rendition", ProviderID: "docling-local",
					TrustBoundary: "private-network", InputClasses: []string{"document_bytes"},
					RuntimeDisclosure: api.ProcessingRuntimeDisclosure{
						ImmediateProcessor: "docbank rendition adapter", UltimateProcessor: "Docling Serve",
						Endpoint: "http://docling.internal:5001", Deployment: strings.Repeat("e", 64),
						Model: "layout", ModelRevision: "2026.08", MetadataClasses: []string{"synthetic_filename"},
						RetainedArtifactRoles: []string{"sanitized_markdown"}}}},
				DisclosedClasses: []string{"document_bytes"}, RetainedClasses: []string{"sanitized_markdown"},
				Estimate:        api.ProcessingEstimate{SourceBytes: 1024, ProviderCalls: 1, VectorSpaces: 1},
				ConsentRequired: true, BackupConsequence: "retained derivatives enter future snapshots",
			}))
		case "POST /api/v1/processing/jobs":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, err := w.Write([]byte(`{"sequence":1,"type":"job","job":{"id":"` + jobID +
				`","embedding_job_ids":["` + embeddingJobID + `"],"profile_fingerprint":"` +
				profileFingerprint + `","content_version_id":"` + processingTestVersionID + `"}}` + "\n" +
				`{"sequence":2,"type":"status","status":{"job_id":"` + jobID +
				`","state":"partial","phase":"embedding","failure_code":"provider_unavailable",` +
				`"embedding_job_ids":["` + embeddingJobID + `"],"completed_bindings":0},"terminal":true}` + "\n"))
			assert.NoError(t, err)
		case "GET /api/v1/processing/jobs/" + jobID:
			assert.NoError(t, json.MarshalWrite(w, api.ProcessingStatus{
				JobID: jobID, State: "partial", Phase: "embedding", FailureCode: "provider_unavailable",
				EmbeddingJobIDs: []string{embeddingJobID}, CompletedBindings: 0,
			}))
		default:
			http.Error(w, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	c := client.New(server.URL, "test-key")

	profilesCommand, profilesOutput := processingTestCommand()
	require.NoError(t, runProcessingProfiles(profilesCommand, c, false))
	assert.Contains(t, profilesOutput.String(), "private")
	assert.Contains(t, profilesOutput.String(), "rendition")
	assert.Contains(t, profilesOutput.String(), "semantic")

	planCommand, planOutput := processingTestCommand()
	require.NoError(t, runProcessingPlan(planCommand, c, "id:42", "private", false))
	assert.Contains(t, planOutput.String(), "docling-local")
	assert.Contains(t, planOutput.String(), "private-network")
	assert.Contains(t, planOutput.String(), "Docling Serve")
	assert.Contains(t, planOutput.String(), "http://docling.internal:5001")
	assert.Contains(t, planOutput.String(), "layout@2026.08")
	assert.Contains(t, planOutput.String(), "synthetic_filename")
	assert.Contains(t, planOutput.String(), "sanitized_markdown")
	assert.Contains(t, planOutput.String(), planFingerprint)

	buildCommand, buildOutput := processingTestCommand()
	require.NoError(t, runProcessingBuild(buildCommand, c, "id:42", "private", planFingerprint, true, false, false))
	assert.Contains(t, buildOutput.String(), jobID)
	assert.Contains(t, buildOutput.String(), "partial")
	assert.Contains(t, buildOutput.String(), "provider_unavailable")

	ndjsonCommand, ndjsonOutput := processingTestCommand()
	require.NoError(t, runProcessingBuild(ndjsonCommand, c, "id:42", "private", planFingerprint, true, false, true))
	lines := strings.Split(strings.TrimSpace(ndjsonOutput.String()), "\n")
	require.Len(t, lines, 2)
	var first, second api.ProcessingJobEvent
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))
	assert.Equal(t, "job", first.Type)
	assert.Equal(t, jobID, first.Job.ID)
	assert.Equal(t, "status", second.Type)
	assert.True(t, second.Terminal)
	assert.Equal(t, "partial", second.Status.State)

	statusCommand, statusOutput := processingTestCommand()
	require.NoError(t, runProcessingStatus(statusCommand, c, jobID, true))
	var status api.ProcessingStatus
	require.NoError(t, json.Unmarshal(statusOutput.Bytes(), &status))
	assert.Equal(t, "partial", status.State)
}

func TestProcessingCLINDJSONFlushesDurableJobBeforeTerminalStatus(t *testing.T) {
	jobID := strings.Repeat("a", 64)
	profileFingerprint := strings.Repeat("b", 64)
	terminalRelease := make(chan struct{})
	jobFlushed := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(terminalRelease) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/nodes/42":
			w.Header().Set("Content-Type", "application/json")
			if !assert.NoError(t, json.MarshalWrite(w, api.Node{ID: 42, Kind: "file",
				CurrentVersionID: processingTestVersionID})) {
				return
			}
		case "POST /api/v1/processing/jobs":
			w.Header().Set("Content-Type", "application/x-ndjson")
			job := api.ProcessingJob{ID: jobID, EmbeddingJobIDs: []string{},
				ProfileFingerprint: profileFingerprint, ContentVersionID: processingTestVersionID}
			if !assert.NoError(t, json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 1, Type: "job", Job: &job})) {
				return
			}
			flusher, ok := w.(http.Flusher)
			if !assert.True(t, ok) {
				return
			}
			flusher.Flush()
			close(jobFlushed)
			<-terminalRelease
			status := api.ProcessingStatus{JobID: jobID, State: "completed", Phase: "published",
				EmbeddingJobIDs: []string{}}
			assert.NoError(t, json.MarshalWrite(w, api.ProcessingJobEvent{Sequence: 2, Type: "status",
				Status: &status, Terminal: true}))
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	c := client.New(server.URL, "test-key")
	command := &cobra.Command{}
	command.SetContext(t.Context())
	output := &synchronizedBuffer{}
	command.SetOut(output)
	command.SetErr(output)
	done := make(chan error, 1)
	go func() {
		done <- runProcessingBuild(command, c, "id:42", "private", strings.Repeat("c", 64), true, false, true)
	}()

	<-jobFlushed
	require.Eventually(t, func() bool { return strings.Contains(output.String(), `"type":"job"`) },
		time.Second, 10*time.Millisecond, "the durable job event must be observable while terminal delivery is blocked")
	releaseOnce.Do(func() { close(terminalRelease) })
	require.NoError(t, <-done)
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	require.Len(t, lines, 2)
}

func TestProcessingCLIBuildRequiresReviewedFingerprintAndConsent(t *testing.T) {
	command, _ := processingTestCommand()
	err := runProcessingBuild(command, client.New("http://127.0.0.1:1", "test-key"),
		"id:42", "private", "", true, false, false)
	require.ErrorContains(t, err, "plan fingerprint")

	err = runProcessingBuild(command, client.New("http://127.0.0.1:1", "test-key"),
		"id:42", "private", strings.Repeat("a", 64), false, false, false)
	require.ErrorContains(t, err, "--consent")
}

func TestProcessingCLIRegistersCommandsAndValidatesBeforeDaemon(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	out, err := runCLI(t, "processing", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "profiles")
	assert.Contains(t, out, "plan")
	assert.Contains(t, out, "build")
	assert.Contains(t, out, "status")

	_, err = runCLI(t, "processing", "build", "id:42", "--profile", "private")
	require.ErrorContains(t, err, "plan fingerprint")
	_, err = runCLI(t, "rendition", "get", "not-a-hash")
	require.ErrorContains(t, err, "attachment ID")
	_, err = runCLI(t, "search", "query", "--mode", "auto", "--profile", "private")
	require.ErrorContains(t, err, "--source-version")
}

func processingTestCommand() (*cobra.Command, *bytes.Buffer) {
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&output)
	command.SetErr(&output)
	return command, &output
}

type synchronizedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	written, err := buffer.data.Write(value)
	if err != nil {
		return written, fmt.Errorf("writing synchronized test output: %w", err)
	}
	return written, nil
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.data.String()
}
