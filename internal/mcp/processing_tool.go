package mcp

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

const maxRememberedProcessingPlans = 4096

type processingPlanKey struct {
	contentVersionID string
	fingerprint      string
}

type reviewedProcessingPlan struct {
	Selector           api.ProcessingSelector
	ProfileFingerprint string
}

// processingPlanRegistry retains only selectors and profile identities whose
// complete disclosures were returned by this MCP process. The daemon still
// recomputes and verifies the fingerprint immediately before enqueue.
type processingPlanRegistry struct {
	mu         sync.Mutex
	entries    map[processingPlanKey]reviewedProcessingPlan
	order      []processingPlanKey
	jobSources map[string]string
	jobOrder   []string
}

func newProcessingPlanRegistry() *processingPlanRegistry {
	return &processingPlanRegistry{entries: make(map[processingPlanKey]reviewedProcessingPlan), jobSources: make(map[string]string)}
}

func (registry *processingPlanRegistry) rememberJob(jobID, contentVersionID string) {
	if registry == nil || jobID == "" || contentVersionID == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.jobSources[jobID]; !exists {
		if len(registry.jobOrder) == maxRememberedProcessingPlans {
			delete(registry.jobSources, registry.jobOrder[0])
			copy(registry.jobOrder, registry.jobOrder[1:])
			registry.jobOrder[len(registry.jobOrder)-1] = jobID
		} else {
			registry.jobOrder = append(registry.jobOrder, jobID)
		}
	}
	registry.jobSources[jobID] = contentVersionID
}

func (registry *processingPlanRegistry) jobSource(jobID string) (string, bool) {
	if registry == nil {
		return "", false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	sourceID, ok := registry.jobSources[jobID]
	return sourceID, ok
}

func (registry *processingPlanRegistry) remember(plan api.ProcessingPlan) {
	if registry == nil {
		return
	}
	key := processingPlanKey{contentVersionID: plan.Selector.ContentVersionID, fingerprint: plan.Fingerprint}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	reviewed := reviewedProcessingPlan{Selector: plan.Selector, ProfileFingerprint: plan.ProfileFingerprint}
	if _, exists := registry.entries[key]; exists {
		registry.entries[key] = reviewed
		return
	}
	if len(registry.order) == maxRememberedProcessingPlans {
		delete(registry.entries, registry.order[0])
		copy(registry.order, registry.order[1:])
		registry.order[len(registry.order)-1] = key
	} else {
		registry.order = append(registry.order, key)
	}
	registry.entries[key] = reviewed
}

func (registry *processingPlanRegistry) reviewed(
	contentVersionID, fingerprint string,
) (reviewedProcessingPlan, error) {
	if registry == nil {
		return reviewedProcessingPlan{}, daemonconn.ErrProcessingPlanChanged
	}
	registry.mu.Lock()
	reviewed, exists := registry.entries[processingPlanKey{
		contentVersionID: contentVersionID, fingerprint: fingerprint,
	}]
	registry.mu.Unlock()
	if !exists || reviewed.Selector.ContentVersionID != contentVersionID {
		return reviewedProcessingPlan{}, daemonconn.ErrProcessingPlanChanged
	}
	return reviewed, nil
}

type startProcessingInput struct {
	ContentVersionID string `json:"content_version_id"`
	PlanFingerprint  string `json:"plan_fingerprint"`
}

type startProcessingOutput struct {
	privateCache

	JobID              string   `json:"job_id"`
	RenditionJobID     string   `json:"rendition_job_id,omitzero"`
	AttachmentID       string   `json:"attachment_id,omitzero"`
	EmbeddingJobIDs    []string `json:"embedding_job_ids"`
	ProfileFingerprint string   `json:"profile_fingerprint"`
	ContentVersionID   string   `json:"content_version_id"`
	State              string   `json:"state"`
}

func processingToolHandler(
	lease *daemonLease, plans *processingPlanRegistry, policy operationPolicy, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		result, err := executeProcessingToolWithPolicy(ctx, lease, plans, policy, validator, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, processingToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return result, nil
	}
}

func executeProcessingTool(
	ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, validator *jsonschema.Resolved, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	return executeProcessingToolWithPolicy(ctx, lease, plans, newOperationPolicy(nil, api.Principal{}), validator, raw)
}

func executeProcessingToolWithPolicy(
	ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, policy operationPolicy, validator *jsonschema.Resolved, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return nil, err
	}
	var input startProcessingInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	if _, err := policy.authorize(ctx, api.OperationProcessing, []string{input.ContentVersionID}, true, true); err != nil {
		return nil, err
	}
	reviewed, err := plans.reviewed(input.ContentVersionID, input.PlanFingerprint)
	if err != nil {
		return nil, err
	}
	job, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.ProcessingJob, error) {
		return c.EnqueueProcessing(ctx, api.StartProcessingRequest{
			Selector: reviewed.Selector, PlanFingerprint: input.PlanFingerprint, Consent: false,
		}, reviewed.ProfileFingerprint)
	})
	if err != nil {
		return nil, err
	}
	if job.ID == "" || job.ContentVersionID != input.ContentVersionID ||
		job.ProfileFingerprint != reviewed.ProfileFingerprint {
		return nil, sanitizedDaemonError(errProcessingOutcomeUnknown,
			errors.New("processing enqueue response does not bind its reviewed plan"))
	}
	if job.EmbeddingJobIDs == nil {
		job.EmbeddingJobIDs = []string{}
	}
	plans.rememberJob(job.ID, job.ContentVersionID)
	output := startProcessingOutput{JobID: job.ID, RenditionJobID: job.RenditionJobID,
		AttachmentID: job.AttachmentID, EmbeddingJobIDs: job.EmbeddingJobIDs,
		ProfileFingerprint: job.ProfileFingerprint, ContentVersionID: job.ContentVersionID,
		State: "queued", privateCache: newPrivateCache()}
	return boundedToolSuccess(validator, output, nil)
}
