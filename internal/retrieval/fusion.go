package retrieval

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/docbank/document/embedding"
)

func FuseReciprocalRank(lexical, semantic []Candidate, limit int) ([]Result, bool, error) {
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
	identities := make([]DocumentIdentity, 0, len(byDocument))
	for identity := range byDocument {
		identities = append(identities, identity)
	}
	slices.SortFunc(identities, func(left, right DocumentIdentity) int {
		return cmp.Or(cmp.Compare(left.VaultID, right.VaultID),
			cmp.Compare(left.NodeID, right.NodeID), cmp.Compare(left.ContentVersionID, right.ContentVersionID))
	})
	// Shared fusion orders string keys before truncation. Ordinals preserve
	// the document order, including numeric node IDs, without encoding evidence.
	keys := make(map[DocumentIdentity]string, len(identities))
	metadata := make(map[string]*Result, len(identities))
	for index, identity := range identities {
		key := fmt.Sprintf("%020d", index)
		keys[identity], metadata[key] = key, byDocument[identity]
	}
	rankLane := func(candidates []Candidate) embedding.ScopedCandidates {
		lane := embedding.ScopedCandidates{Candidates: make([]embedding.RankedCandidate, len(candidates))}
		for index, candidate := range candidates {
			lane.Candidates[index] = embedding.RankedCandidate{
				Key: keys[candidate.Document], Rank: candidate.Rank, Score: candidate.Score,
			}
		}
		return lane
	}
	fused, err := embedding.FuseReciprocalRank(embedding.FusionInput{
		Lexical: rankLane(lexical), Semantic: rankLane(semantic),
	}, limit)
	if err != nil {
		return nil, false, err
	}
	results := make([]Result, len(fused.Candidates))
	for index, candidate := range fused.Candidates {
		result := *metadata[candidate.Key]
		result.Rank, result.Score = candidate.Rank, candidate.Score
		for _, signal := range []struct {
			lane   Lane
			signal *embedding.CandidateSignal
		}{{LaneLexical, candidate.Lexical}, {LaneSemantic, candidate.Semantic}} {
			if signal.signal != nil {
				result.Explanation = append(result.Explanation, Contribution{
					Lane: signal.lane, Rank: signal.signal.Rank,
					Contribution: 1 / float64(ReciprocalRankK+signal.signal.Rank),
				})
			}
		}
		results[index] = result
	}
	return results, fused.Truncated, nil
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
	for _, candidate := range candidates {
		if candidate.Lane != lane {
			return errors.New("candidate belongs to the wrong lane")
		}
		if candidate.Document.VaultID == "" || candidate.Document.NodeID <= 0 || candidate.Document.ContentVersionID == "" {
			return errors.New("candidate document identity is incomplete")
		}
		result := results[candidate.Document]
		if result == nil {
			result = &Result{Document: candidate.Document, Path: candidate.Path}
			results[candidate.Document] = result
		} else if result.Path != "" && candidate.Path != "" && result.Path != candidate.Path {
			return errors.New("candidate lanes disagree on the stable document path")
		} else if result.Path == "" {
			result.Path = candidate.Path
		}
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
