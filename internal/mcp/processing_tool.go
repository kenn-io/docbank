package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
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
	mu      sync.Mutex
	entries map[processingPlanKey]reviewedProcessingPlan
	order   []processingPlanKey
}

func newProcessingPlanRegistry() *processingPlanRegistry {
	return &processingPlanRegistry{entries: make(map[processingPlanKey]reviewedProcessingPlan)}
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
		return reviewedProcessingPlan{}, client.ErrProcessingPlanChanged
	}
	registry.mu.Lock()
	reviewed, exists := registry.entries[processingPlanKey{
		contentVersionID: contentVersionID, fingerprint: fingerprint,
	}]
	registry.mu.Unlock()
	if !exists || reviewed.Selector.ContentVersionID != contentVersionID {
		return reviewedProcessingPlan{}, client.ErrProcessingPlanChanged
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

func processingToolHandler(lease *daemonLease, plans *processingPlanRegistry) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		result, err := executeProcessingTool(ctx, lease, plans, request.Params.Arguments)
		if err != nil {
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return result, nil
	}
}

func invokeProcessingTool(
	ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, arguments map[string]any,
) (*sdkmcp.CallToolResult, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	return executeProcessingTool(ctx, lease, plans, raw)
}

func executeProcessingTool(
	ctx context.Context, lease *daemonLease, plans *processingPlanRegistry, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return nil, err
	}
	var input startProcessingInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return nil, err
	}
	reviewed, err := plans.reviewed(input.ContentVersionID, input.PlanFingerprint)
	if err != nil {
		return nil, err
	}
	job, err := daemonProcessingStart(ctx, lease, func(c *client.Client) (api.ProcessingJob, error) {
		return c.EnqueueProcessing(ctx, api.StartProcessingRequest{
			Selector: reviewed.Selector, PlanFingerprint: input.PlanFingerprint, Consent: false,
		})
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
	output := startProcessingOutput{JobID: job.ID, RenditionJobID: job.RenditionJobID,
		AttachmentID: job.AttachmentID, EmbeddingJobIDs: job.EmbeddingJobIDs,
		ProfileFingerprint: job.ProfileFingerprint, ContentVersionID: job.ContentVersionID,
		State: "queued", privateCache: newPrivateCache()}
	return boundedToolSuccess(processingToolDefinition.name, output, nil)
}
