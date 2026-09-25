package mcp

import (
	"context"

	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type productionRevisionPageInput struct {
	SetID     string `json:"set_id"`
	Revision  int64  `json:"revision"`
	Cursor    string `json:"cursor"`
	Limit     int    `json:"limit"`
	Uncertain string `json:"uncertain"`
}

type productionJobInput struct {
	SetID string `json:"set_id"`
	JobID string `json:"job_id"`
}

type productionMemberSummary struct {
	ID              string `json:"id"`
	Ordinal         int64  `json:"ordinal"`
	SourceVersionID string `json:"source_version_id"`
	Mode            string `json:"mode"`
	MapSHA256       string `json:"map_sha256"`
	Reviewed        bool   `json:"reviewed"`
	ReviewBinding   string `json:"review_binding"`
}

type productionDecisionSummary struct {
	ID           string `json:"id"`
	MemberID     string `json:"member_id"`
	Action       string `json:"action"`
	Reason       string `json:"reason"`
	Label        string `json:"label"`
	Uncertain    bool   `json:"uncertain"`
	SelectorKind string `json:"selector_kind"`
	MapSHA256    string `json:"map_sha256"`
}

type productionMemberPageOutput struct {
	privateCache

	Items      []productionMemberSummary `json:"items"`
	NextCursor string                    `json:"next_cursor"`
}

type productionDecisionPageOutput struct {
	privateCache

	Items      []productionDecisionSummary `json:"items"`
	NextCursor string                      `json:"next_cursor"`
}

type productionJobOutput struct {
	privateCache

	Job api.ProductionJobStatus `json:"job"`
}

func listProductionMembers(ctx context.Context, lease *daemonLease, raw []byte) (productionMemberPageOutput, error) {
	var input productionRevisionPageInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionMemberPageOutput{}, err
	}
	if !validToolUUID(input.SetID) || input.Revision < 1 || input.Limit < 0 ||
		input.Limit > redaction.MaxProductionPage || len(input.Cursor) > 4096 || input.Uncertain != "" {
		return productionMemberPageOutput{}, invalidToolArgumentsError()
	}
	limit := input.Limit
	if limit == 0 {
		limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionMemberPage, error) {
		return c.ProductionMembers(ctx, input.SetID, input.Revision, input.Cursor, limit)
	})
	if err != nil {
		return productionMemberPageOutput{}, err
	}
	output := productionMemberPageOutput{privateCache: newPrivateCache(),
		Items: make([]productionMemberSummary, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, item := range page.Items {
		output.Items = append(output.Items, productionMemberSummary{ID: item.ID, Ordinal: item.Ordinal,
			SourceVersionID: item.SourceVersionID, Mode: item.Mode, MapSHA256: item.MapSHA256,
			Reviewed: item.Reviewed, ReviewBinding: item.ReviewBinding})
	}
	return output, nil
}

func listProductionDecisions(ctx context.Context, lease *daemonLease, raw []byte) (productionDecisionPageOutput, error) {
	var input productionRevisionPageInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionDecisionPageOutput{}, err
	}
	if !validToolUUID(input.SetID) || input.Revision < 1 || input.Limit < 0 ||
		input.Limit > 100 || len(input.Cursor) > 2048 {
		return productionDecisionPageOutput{}, invalidToolArgumentsError()
	}
	var uncertain *bool
	switch input.Uncertain {
	case "", "all":
	case "true":
		value := true
		uncertain = &value
	case "false":
		value := false
		uncertain = &value
	default:
		return productionDecisionPageOutput{}, invalidToolArgumentsError()
	}
	limit := input.Limit
	if limit == 0 {
		limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionDecisionPage, error) {
		return c.ProductionDecisionsFiltered(ctx, input.SetID, input.Revision, input.Cursor, limit, uncertain)
	})
	if err != nil {
		return productionDecisionPageOutput{}, err
	}
	output := productionDecisionPageOutput{privateCache: newPrivateCache(),
		Items: make([]productionDecisionSummary, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, item := range page.Items {
		output.Items = append(output.Items, productionDecisionSummary{ID: item.ID, MemberID: item.MemberID,
			Action: item.Action, Reason: item.Reason, Label: item.Label, Uncertain: item.Uncertain,
			SelectorKind: item.Selector.Kind, MapSHA256: item.Selector.MapSHA256})
	}
	return output, nil
}

func getProductionJob(ctx context.Context, lease *daemonLease, raw []byte) (productionJobOutput, error) {
	var input productionJobInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionJobOutput{}, err
	}
	if !validToolUUID(input.SetID) || !validToolUUID(input.JobID) {
		return productionJobOutput{}, invalidToolArgumentsError()
	}
	job, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ProductionJobStatus, error) {
		return c.ProductionJobStatus(ctx, input.SetID, input.JobID)
	})
	if err != nil {
		return productionJobOutput{}, err
	}
	return productionJobOutput{privateCache: newPrivateCache(), Job: job}, nil
}
