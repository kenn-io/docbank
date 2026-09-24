package daemonconn

import (
	"context"
	"errors"
	"fmt"
	"math"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/store"
)

// TagNeighborhood reads one bounded direct-assignment projection from the
// daemon and verifies that the response stays inside the requested fence.
func (c *Connection) TagNeighborhood(
	ctx context.Context, request api.TagNeighborhoodRequest,
) (api.TagNeighborhoodResponse, error) {
	normalized, err := normalizeTagNeighborhoodRequest(request)
	if err != nil {
		return api.TagNeighborhoodResponse{}, fmt.Errorf("tag neighborhood request is invalid: %w", err)
	}
	apiResponse, err := c.API().TagNeighborhood(ctx, &apiclient.TagNeighborhoodRequestOptions{Body: &normalized})
	if err != nil {
		return api.TagNeighborhoodResponse{}, err
	}
	result := *apiResponse
	if err := validateTagNeighborhoodResponse(normalized, result); err != nil {
		return api.TagNeighborhoodResponse{}, fmt.Errorf("tag neighborhood response is invalid: %w", err)
	}
	return result, nil
}

func normalizeTagNeighborhoodRequest(request api.TagNeighborhoodRequest) (api.TagNeighborhoodRequest, error) {
	if !validUUIDv4(request.Fence.VaultUID) || len(request.Fence.ContentVersionIDs) > store.MaxSearchSourceFenceIDs {
		return api.TagNeighborhoodRequest{}, errors.New("source fence is invalid")
	}
	seenVersions := make(map[string]struct{}, len(request.Fence.ContentVersionIDs))
	for _, id := range request.Fence.ContentVersionIDs {
		if !validUUIDv4(id) {
			return api.TagNeighborhoodRequest{}, errors.New("source fence contains an invalid content version ID")
		}
		if _, duplicate := seenVersions[id]; duplicate {
			return api.TagNeighborhoodRequest{}, errors.New("source fence contains duplicate content version IDs")
		}
		seenVersions[id] = struct{}{}
	}
	seeds := 0
	if request.Seed.NodeID > 0 {
		seeds++
	}
	if request.Seed.TagID != "" {
		seeds++
		if !validUUIDv4(request.Seed.TagID) {
			return api.TagNeighborhoodRequest{}, errors.New("tag seed is invalid")
		}
	}
	if request.Seed.PassageID != "" {
		seeds++
	}
	if seeds != 1 || request.Seed.NodeID < 0 || request.Seed.PassageID != "" {
		return api.TagNeighborhoodRequest{}, errors.New("select one supported document or tag seed")
	}
	if len(request.AssignmentKinds) == 0 {
		request.AssignmentKinds = []string{store.TagAssignmentDocument}
	}
	if len(request.AssignmentKinds) != 1 || request.AssignmentKinds[0] != store.TagAssignmentDocument {
		return api.TagNeighborhoodRequest{}, errors.New("only direct document assignments are supported")
	}
	if request.Limit == 0 {
		request.Limit = store.DefaultTagNeighborhoodLimit
	}
	if request.Limit < 1 || request.Limit > store.MaxTagNeighborhoodLimit {
		return api.TagNeighborhoodRequest{}, errors.New("limit is out of range")
	}
	if request.MaxHops == 0 {
		request.MaxHops = 2
	}
	if request.MaxHops < 1 || request.MaxHops > store.MaxTagGraphHops {
		return api.TagNeighborhoodRequest{}, errors.New("max hops is out of range")
	}
	if request.MaxVisited == 0 {
		request.MaxVisited = store.MaxTagGraphVisitedNodes
	}
	if request.MaxVisited < 1 || request.MaxVisited > store.MaxTagGraphVisitedNodes {
		return api.TagNeighborhoodRequest{}, errors.New("max visited nodes is out of range")
	}
	return request, nil
}

func validateTagNeighborhoodResponse(
	request api.TagNeighborhoodRequest, result api.TagNeighborhoodResponse,
) error {
	if result.VaultUID != request.Fence.VaultUID {
		return errors.New("vault identity does not match the requested source fence")
	}
	if result.DocumentCount != len(request.Fence.ContentVersionIDs) ||
		result.UntaggedDocumentCount < 0 || result.UntaggedDocumentCount > result.DocumentCount {
		return errors.New("scoped document counts are inconsistent")
	}
	if result.VisitedNodes < 1 || result.VisitedNodes > request.MaxVisited {
		return errors.New("visited-node count is outside the requested bound")
	}
	if len(result.Documents)+len(result.Tags) > request.Limit {
		return errors.New("neighbor count exceeds the requested limit")
	}
	fence := make(map[string]struct{}, len(request.Fence.ContentVersionIDs))
	for _, id := range request.Fence.ContentVersionIDs {
		fence[id] = struct{}{}
	}
	seenDocuments := make(map[int64]struct{}, len(result.Documents))
	seenVersions := make(map[string]struct{}, len(result.Documents))
	for i, document := range result.Documents {
		if document.NodeID < 1 || !validUUIDv4(document.ContentVersionID) {
			return fmt.Errorf("document %d has invalid identity", i)
		}
		if _, allowed := fence[document.ContentVersionID]; !allowed {
			return fmt.Errorf("document %d is outside the requested source fence", i)
		}
		if _, duplicate := seenDocuments[document.NodeID]; duplicate {
			return fmt.Errorf("document %d repeats a node identity", i)
		}
		if _, duplicate := seenVersions[document.ContentVersionID]; duplicate {
			return fmt.Errorf("document %d repeats a content version identity", i)
		}
		seenDocuments[document.NodeID] = struct{}{}
		seenVersions[document.ContentVersionID] = struct{}{}
		if document.Path == "" || document.Path[0] != '/' || math.IsNaN(document.Score) || math.IsInf(document.Score, 0) || document.Score < 0 {
			return fmt.Errorf("document %d has invalid path or score", i)
		}
		tagIDs := make(map[string]struct{}, len(document.SharedTags))
		score := 0.0
		for j, tag := range document.SharedTags {
			if err := validateTagGraphTag(tag, result.DocumentCount); err != nil {
				return fmt.Errorf("document %d shared tag %d: %w", i, j, err)
			}
			if _, duplicate := tagIDs[tag.ID]; duplicate {
				return fmt.Errorf("document %d repeats shared tag %s", i, tag.ID)
			}
			tagIDs[tag.ID] = struct{}{}
			score += tag.Weight
		}
		if math.Abs(score-document.Score) > 1e-12 {
			return fmt.Errorf("document %d topical score is inconsistent", i)
		}
		if err := validateTagGraphPath(request, document.GraphPath, store.TagGraphNodeDocument, document.NodeID, ""); err != nil {
			return fmt.Errorf("document %d path: %w", i, err)
		}
	}
	seenTags := make(map[string]struct{}, len(result.Tags))
	for i, tag := range result.Tags {
		if err := validateTagGraphTag(tag, result.DocumentCount); err != nil {
			return fmt.Errorf("tag %d: %w", i, err)
		}
		if _, duplicate := seenTags[tag.ID]; duplicate {
			return fmt.Errorf("tag %d repeats identity %s", i, tag.ID)
		}
		seenTags[tag.ID] = struct{}{}
		if err := validateTagGraphPath(request, tag.Path, store.TagGraphNodeTag, 0, tag.ID); err != nil {
			return fmt.Errorf("tag %d path: %w", i, err)
		}
	}
	return nil
}

func validateTagGraphTag(tag api.TagGraphTag, documentCount int) error {
	if !validUUIDv4(tag.ID) || tag.AssignmentOrigin != store.TagAssignmentOriginLegacy ||
		tag.ScopedDocumentCount < 1 || tag.ScopedDocumentCount > documentCount {
		return errors.New("identity, origin, or scoped frequency is invalid")
	}
	normalized, err := store.NormalizeTagName(tag.Name)
	if err != nil || normalized != tag.Name {
		return errors.New("name is invalid or non-canonical")
	}
	want := store.TagRarityWeight(documentCount, tag.ScopedDocumentCount)
	if math.IsNaN(tag.Weight) || math.IsInf(tag.Weight, 0) || math.Abs(tag.Weight-want) > 1e-12 {
		return errors.New("rarity weight is inconsistent")
	}
	return nil
}

func validateTagGraphPath(
	request api.TagNeighborhoodRequest,
	path []api.TagGraphPathStep,
	targetKind string,
	targetNodeID int64,
	targetTagID string,
) error {
	if len(path) < 2 || len(path) > request.MaxHops+1 {
		return errors.New("length is outside the requested hop bound")
	}
	for i, step := range path {
		wantKind := store.TagGraphNodeDocument
		if i%2 == 1 {
			wantKind = store.TagGraphNodeTag
		}
		if request.Seed.TagID != "" {
			if i%2 == 0 {
				wantKind = store.TagGraphNodeTag
			} else {
				wantKind = store.TagGraphNodeDocument
			}
		}
		if step.Kind != wantKind ||
			(step.Kind == store.TagGraphNodeDocument && (step.NodeID < 1 || step.TagID != "")) ||
			(step.Kind == store.TagGraphNodeTag && (!validUUIDv4(step.TagID) || step.NodeID != 0)) {
			return fmt.Errorf("step %d has invalid typed identity", i)
		}
	}
	first, last := path[0], path[len(path)-1]
	if request.Seed.NodeID > 0 && (first.Kind != store.TagGraphNodeDocument || first.NodeID != request.Seed.NodeID) {
		return errors.New("start does not match the requested document seed")
	}
	if request.Seed.TagID != "" && (first.Kind != store.TagGraphNodeTag || first.TagID != request.Seed.TagID) {
		return errors.New("start does not match the requested tag seed")
	}
	if last.Kind != targetKind || last.NodeID != targetNodeID || last.TagID != targetTagID {
		return errors.New("end does not match the result identity")
	}
	return nil
}
