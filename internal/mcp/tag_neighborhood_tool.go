package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

type tagNeighborhoodInput struct {
	VaultID           string   `json:"vault_id"`
	ContentVersionIDs []string `json:"content_version_ids"`
	SeedNodeID        int64    `json:"seed_node_id"`
	SeedTagID         string   `json:"seed_tag_id"`
	Limit             int      `json:"limit"`
	MaxHops           int      `json:"max_hops"`
	MaxVisited        int      `json:"max_visited"`
}

type tagNeighborhoodOutput struct {
	api.TagNeighborhoodResponse
	privateCache
}

func getTagNeighborhood(
	ctx context.Context, lease *daemonLease, raw []byte,
) (tagNeighborhoodOutput, error) {
	var input tagNeighborhoodInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return tagNeighborhoodOutput{}, err
	}
	result, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.TagNeighborhoodResponse, error) {
		return c.TagNeighborhood(ctx, api.TagNeighborhoodRequest{
			Fence: api.ResolvedDocumentSourceFence{VaultUID: input.VaultID,
				ContentVersionIDs: input.ContentVersionIDs},
			Seed:            api.TagGraphSeed{NodeID: input.SeedNodeID, TagID: input.SeedTagID},
			AssignmentKinds: []string{store.TagAssignmentDocument},
			Limit:           input.Limit, MaxHops: input.MaxHops, MaxVisited: input.MaxVisited,
		})
	})
	if err != nil {
		return tagNeighborhoodOutput{}, err
	}
	return tagNeighborhoodOutput{TagNeighborhoodResponse: result, privateCache: newPrivateCache()}, nil
}
