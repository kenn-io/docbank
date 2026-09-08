package retrieval

import (
	"errors"
	"fmt"
	"slices"
)

func FuseReciprocalRank(lexical, semantic []Candidate, limit int) ([]Result, bool, error) {
	if limit < 1 || limit > MaxCandidateLimit {
		return nil, false, fmt.Errorf("retrieval candidate limit must be between 1 and %d", MaxCandidateLimit)
	}
	if err := validateOneSemanticVectorSpace(semantic); err != nil {
		return nil, false, err
	}
	byDocument := make(map[DocumentIdentity]*Result, len(lexical)+len(semantic))
	if err := addLane(byDocument, lexical, LaneLexical); err != nil {
		return nil, false, fmt.Errorf("lexical lane: %w", err)
	}
	if err := addLane(byDocument, semantic, LaneSemantic); err != nil {
		return nil, false, fmt.Errorf("semantic lane: %w", err)
	}
	results := make([]Result, 0, len(byDocument))
	for _, result := range byDocument {
		results = append(results, *result)
	}
	slices.SortFunc(results, compareResults)
	for index := range results {
		results[index].Rank = index + 1
	}
	truncated := len(results) > limit
	if truncated {
		results = results[:limit]
	}
	return results, truncated, nil
}

func validateOneSemanticVectorSpace(candidates []Candidate) error {
	vectorSpaceID := ""
	for _, candidate := range candidates {
		if candidate.VectorSpaceID == "" {
			return errors.New("semantic candidates require one active vector space")
		}
		if vectorSpaceID == "" {
			vectorSpaceID = candidate.VectorSpaceID
		} else if candidate.VectorSpaceID != vectorSpaceID {
			return errors.New("semantic candidates must belong to one active vector space")
		}
		for _, evidence := range candidate.Evidence {
			if evidence.VectorSpaceID != candidate.VectorSpaceID {
				return errors.New("semantic evidence must belong to one active vector space")
			}
		}
	}
	return nil
}

func addLane(results map[DocumentIdentity]*Result, candidates []Candidate, lane Lane) error {
	lastRank := 0
	seen := make(map[DocumentIdentity]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.Lane != lane || candidate.Rank <= lastRank {
			return errors.New("candidate lane and ranks must be ordered and exact")
		}
		if candidate.Document.VaultID == "" || candidate.Document.NodeID <= 0 || candidate.Document.ContentVersionID == "" {
			return errors.New("candidate document identity is incomplete")
		}
		if _, duplicate := seen[candidate.Document]; duplicate {
			return errors.New("candidate document identity is duplicated within one lane")
		}
		seen[candidate.Document] = struct{}{}
		lastRank = candidate.Rank
		result := results[candidate.Document]
		if result == nil {
			result = &Result{Document: candidate.Document, Path: candidate.Path}
			results[candidate.Document] = result
		} else if result.Path != "" && candidate.Path != "" && result.Path != candidate.Path {
			return errors.New("candidate lanes disagree on the stable document path")
		} else if result.Path == "" {
			result.Path = candidate.Path
		}
		contribution := 1 / float64(ReciprocalRankK+candidate.Rank)
		result.Score += contribution
		result.Explanation = append(result.Explanation, Contribution{
			Lane: lane, Rank: candidate.Rank, Contribution: contribution,
		})
		if lane == LaneLexical {
			result.LexicalRank = candidate.Rank
			result.Excerpt = candidate.Excerpt
		} else {
			result.SemanticRank = candidate.Rank
		}
		result.Evidence = append(result.Evidence, candidate.Evidence...)
	}
	return nil
}

func compareResults(left, right Result) int {
	switch {
	case left.Score > right.Score:
		return -1
	case left.Score < right.Score:
		return 1
	case left.Document.VaultID < right.Document.VaultID:
		return -1
	case left.Document.VaultID > right.Document.VaultID:
		return 1
	case left.Document.NodeID < right.Document.NodeID:
		return -1
	case left.Document.NodeID > right.Document.NodeID:
		return 1
	case left.Document.ContentVersionID < right.Document.ContentVersionID:
		return -1
	case left.Document.ContentVersionID > right.Document.ContentVersionID:
		return 1
	default:
		return 0
	}
}
